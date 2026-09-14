// Package remediate locates the rule a score belongs to in its source file
// and computes what a proposed change to it would have done to its past
// firing history. It is the evidence layer a pull request needs: a verdict
// is only reviewable once it points at a file and line and states, in a
// checkable sentence, what would have happened under the proposed fix.
package remediate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// RuleKey identifies a rule the same way the rest of the codebase does:
// (group, alertname). See docs/noisefloor-design.md's data model and
// collect.SyncRules, which refuses to attribute episodes any other way.
type RuleKey struct {
	Group     string
	AlertName string
}

// RuleLocation is where one alerting rule sits in a git checkout of rule
// files: which file, which lines it spans, and where within that span its
// `for:` and `expr:` fields sit. Line numbers are 1-indexed, matching every
// editor and diff tool a reviewer will actually look at.
type RuleLocation struct {
	Key RuleKey

	File string

	// StartLine and EndLine bound the whole rule entry -- from its opening
	// `- alert:` line (or whichever field is written first) through the
	// last line that carries content of this rule's own.
	//
	// EndLine is deliberately NOT the next-sibling boundary. YAML gives no
	// end position, so the next rule's (or group's) first line is the only
	// structural bound available -- but everything between this rule's last
	// field and that bound is blank lines and comments that, to a human
	// reading the file, introduce what FOLLOWS. A retire deletes this span,
	// and deleting another team's doc comments is not a minimal diff, so
	// the span is trimmed back off those trailing lines. The same trim
	// keeps the last rule in a file from swallowing trailing top-level
	// comments all the way to EOF.
	StartLine int
	EndLine   int

	// ForLine is the line of the `for:` key, or 0 if the rule has none (a
	// rule firing instantly on every evaluation).
	ForLine int

	// ExprLine and ExprEndLine bound the `expr:` field's value. They are
	// equal for the common single-line expression; ExprEndLine extends
	// past ExprLine for a multi-line block scalar (`expr: |`), which
	// Prometheus rules use for anything non-trivial.
	ExprLine    int
	ExprEndLine int

	// ExprValue and ForValue are the rule's `expr:` and `for:` exactly as
	// the file writes them (ForValue is "" for a rule with no `for:`).
	// They exist so a caller can check the rule it is about to edit is
	// still the rule Prometheus evaluated and noisefloor scored -- see
	// DriftAgainst. Without that check a stale checkout gets edited against
	// evidence gathered about a different version of the same rule.
	ExprValue string
	ForValue  string

	// Duplicate is true when the scanned checkout defines this exact
	// (group, alertname) more than once -- twice inside one group (legal in
	// Prometheus, and invisible to collect.SyncRules' AmbiguousNames, which
	// only sees the same name across DIFFERENT groups), or once each in two
	// files. Every location sharing a duplicated key carries the flag, so
	// whichever one an index keeps still reports it. A caller must refuse to
	// edit such a rule: there is no way to tell which definition the
	// evidence belongs to, so a retire would delete, and a tune would
	// rewrite, an arbitrary one of them.
	Duplicate bool
}

// FileError is a rule file LocateRules could not parse. It is not returned
// as an error: a git checkout used for locating rules routinely contains
// files LocateRules has no business understanding -- Helm templates with
// unresolved `{{ }}` placeholders, unrelated YAML, a stale or half-written
// file -- and one bad file must not blind every other rule to its location.
type FileError struct {
	Path string
	Err  error
}

func (e FileError) Error() string { return fmt.Sprintf("%s: %v", e.Path, e.Err) }

// LocateRules walks a git checkout at root looking for Prometheus rule
// files -- any *.yml or *.yaml whose top level has a `groups:` sequence --
// and returns the location of every alerting rule found in them.
//
// Files that fail to parse, or parse but are not shaped like a rule file,
// are skipped and reported in the returned FileError slice rather than
// aborting the walk: see FileError. The returned error is reserved for
// root itself being unusable (missing, not a directory, unreadable) --
// exactly the "stale checkout or wrong path" case the caller (typically the
// scan command) needs to distinguish from "this one file is odd".
func LocateRules(root string) ([]RuleLocation, []FileError, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, fmt.Errorf("rules path %s: %w", root, err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("rules path %s: not a directory", root)
	}

	var locs []RuleLocation
	var fileErrs []FileError

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory entry that failed to stat mid-walk (permissions,
			// a broken symlink) is exactly the kind of single-file problem
			// FileError exists for, not a reason to give up on the rest of
			// the checkout.
			fileErrs = append(fileErrs, FileError{Path: path, Err: err})
			return nil
		}
		if d.IsDir() {
			// .git holds no rule files and can be large; skip it outright
			// rather than walking every object in the repository's history.
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yml" && ext != ".yaml" {
			return nil
		}

		found, ferr := parseRuleFile(path)
		if ferr != nil {
			fileErrs = append(fileErrs, FileError{Path: path, Err: ferr})
			return nil
		}
		locs = append(locs, found...)
		return nil
	})
	if walkErr != nil {
		return nil, nil, fmt.Errorf("walk %s: %w", root, walkErr)
	}

	fileErrs = append(fileErrs, markDuplicates(locs)...)
	return locs, fileErrs, nil
}

// markDuplicates flags every location whose (group, alertname) key is
// defined more than once in the scanned checkout, and reports each clash as
// a FileError so an operator sees it on stderr as well.
//
// Two rules with the same alert name in the SAME group is legal Prometheus
// and is exactly the case collect.SyncRules' AmbiguousNames cannot see (it
// compares names across groups). Left undetected, an index keyed by (group,
// alertname) silently keeps one of them and the bot edits whichever that
// happened to be.
func markDuplicates(locs []RuleLocation) []FileError {
	byKey := make(map[RuleKey][]int, len(locs))
	for i, l := range locs {
		byKey[l.Key] = append(byKey[l.Key], i)
	}

	// Walk locs, not the map: map order is random and these become warnings
	// a human reads, and diffs between runs.
	var errs []FileError
	seen := make(map[RuleKey]bool, len(byKey))
	for _, l := range locs {
		idx := byKey[l.Key]
		if len(idx) < 2 || seen[l.Key] {
			continue
		}
		seen[l.Key] = true

		where := make([]string, 0, len(idx))
		for _, i := range idx {
			locs[i].Duplicate = true
			where = append(where, fmt.Sprintf("%s:%d", locs[i].File, locs[i].StartLine))
		}
		errs = append(errs, FileError{
			Path: l.File,
			Err: fmt.Errorf(
				"alert %q is defined %d times in group %q (%s); noisefloor cannot tell which "+
					"definition the scored history belongs to and will not edit any of them",
				l.Key.AlertName, len(idx), l.Key.Group, strings.Join(where, ", ")),
		})
	}
	return errs
}

// LocationsByKey indexes a set of locations by RuleKey, the same way
// LinesByKey does for just the starting line. internal/scanner and
// internal/pr use this to attach a rule's full location (file, line span,
// for:/expr: field positions) to its score, which a remediation PR needs to
// edit the right lines.
//
// A duplicated key still collapses to one entry -- there is only one slot
// per key -- but never silently: LocateRules has already set Duplicate on
// every location sharing that key, so whichever one lands here carries the
// flag, and a caller refuses to edit it (see pr.ReasonDuplicate).
func LocationsByKey(locs []RuleLocation) map[RuleKey]RuleLocation {
	out := make(map[RuleKey]RuleLocation, len(locs))
	for _, l := range locs {
		out[l.Key] = l
	}
	return out
}

// LinesByKey indexes a set of locations by RuleKey for populating
// store.Rule.Line. A rule is expected to occupy at most one file in a
// correctly laid-out checkout; if two files define the same (group,
// alertname), the last one WalkDir visits wins, which is deterministic
// (lexical directory order) even though it is not necessarily "correct" --
// the ambiguity mirrors AmbiguousNames in collect.SyncRules, which is a
// property of the rule set, not something this package can resolve.
func LinesByKey(locs []RuleLocation) map[RuleKey]int {
	out := make(map[RuleKey]int, len(locs))
	for _, l := range locs {
		out[l.Key] = l.StartLine
	}
	return out
}

// parseRuleFile extracts alerting rule locations from one YAML file. It
// returns (nil, nil) for a YAML file that parses fine but has no `groups:`
// sequence at its top level -- that is simply not a rule file, not an
// error.
//
// A rule file holding more than one YAML document is refused outright. Rule
// positions are line numbers into the whole file, but only one document can
// be located at a time, so the last rule of the located document would take
// its end bound from the file's end -- i.e. from somewhere inside a
// document this function never looked at. A retire deleting that span would
// remove every later document wholesale. There is no useful partial answer
// here, so this is an error rather than a silent skip.
func parseRuleFile(path string) ([]RuleLocation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	docs, err := decodeDocuments(data)
	if err != nil {
		return nil, err
	}

	ruleDocs := 0
	for _, d := range docs {
		if _, g := mapEntry(d, "groups"); g != nil && g.Kind == yaml.SequenceNode {
			ruleDocs++
		}
	}
	if ruleDocs == 0 {
		return nil, nil // not a rule file (or an empty one)
	}
	if len(docs) > 1 {
		return nil, fmt.Errorf(
			"rule file contains %d YAML documents (%d of them define `groups:`); noisefloor "+
				"locates rules by line number within a single document and will not risk "+
				"editing a span that runs into another document -- split it into one file "+
				"per document",
			len(docs), ruleDocs)
	}

	root := docs[0]
	groupsIdx, groupsNode := mapEntry(root, "groups")
	_ = groupsIdx // root has no further sibling we need; groups: end is never asked for

	lines := fileLines(data)
	totalLines := len(lines)

	var locs []RuleLocation
	for gi, groupNode := range groupsNode.Content {
		if groupNode.Kind != yaml.MappingNode {
			continue
		}
		groupName := ""
		if _, nameNode := mapEntry(groupNode, "name"); nameNode != nil {
			groupName = nameNode.Value
		}

		rulesIdx, rulesNode := mapEntry(groupNode, "rules")
		if rulesNode == nil || rulesNode.Kind != yaml.SequenceNode {
			continue
		}

		// Boundary for the LAST rule in this group's `rules:` list: where
		// the `rules:` block itself ends. That is the next field in the
		// group mapping if `rules:` is not the group's last key, else the
		// next group, else the file's own end.
		rulesBlockEnd := nextSiblingLine(groupNode, rulesIdx)
		if rulesBlockEnd == 0 {
			rulesBlockEnd = nextSiblingLine(groupsNode, gi)
		}

		for ri, ruleNode := range rulesNode.Content {
			if ruleNode.Kind != yaml.MappingNode {
				continue
			}
			_, alertNode := mapEntry(ruleNode, "alert")
			if alertNode == nil {
				continue // a recording rule (`record:`), not scoreable
			}

			loc := RuleLocation{
				Key:       RuleKey{Group: groupName, AlertName: alertNode.Value},
				File:      path,
				StartLine: ruleNode.Line,
			}

			if end := nextSiblingLine(rulesNode, ri); end > 0 {
				loc.EndLine = end - 1
			} else if rulesBlockEnd > 0 {
				loc.EndLine = rulesBlockEnd - 1
			} else {
				loc.EndLine = totalLines
			}
			// Trim the next-sibling boundary back onto this rule's own last
			// line of content, never below what the parser attributes to the
			// rule itself. See RuleLocation.EndLine.
			loc.EndLine = trimSpan(lines, nodeEndLine(ruleNode), loc.EndLine)

			if _, forNode := mapEntry(ruleNode, "for"); forNode != nil {
				loc.ForLine = forNode.Line
				loc.ForValue = forNode.Value
			}

			if exprIdx, exprNode := mapEntry(ruleNode, "expr"); exprNode != nil {
				loc.ExprLine = exprNode.Line
				loc.ExprValue = exprNode.Value
				if end := nextSiblingLine(ruleNode, exprIdx); end > 0 {
					loc.ExprEndLine = end - 1
				} else {
					// expr is the rule's last field: its value cannot run
					// past the rule's own end.
					loc.ExprEndLine = loc.EndLine
				}
				loc.ExprEndLine = trimSpan(lines, nodeEndLine(exprNode), loc.ExprEndLine)
			}

			locs = append(locs, loc)
		}
	}
	return locs, nil
}

// mapEntry returns the index of key's VALUE node within a MappingNode's
// flat [k0,v0,k1,v1,...] Content, and the value node itself. The index is
// what nextSiblingLine needs: the entry immediately after a value in
// Content is always the next key, i.e. the start of whatever follows this
// field -- which is exactly the boundary a field's own span ends at.
func mapEntry(node *yaml.Node, key string) (int, *yaml.Node) {
	if node == nil || node.Kind != yaml.MappingNode {
		return -1, nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return i + 1, node.Content[i+1]
		}
	}
	return -1, nil
}

// nextSiblingLine returns the line of whatever comes right after
// container.Content[idx] within the same container -- the next rule in a
// `rules:` sequence, the next group in `groups:`, or the next key in a
// mapping (since a field's value sits one slot before the following key in
// yaml.Node's flat mapping representation). It returns 0 when idx is the
// container's last entry, meaning "no sibling here" -- callers escalate to
// an enclosing container, or fall back to the file's own end.
func nextSiblingLine(container *yaml.Node, idx int) int {
	if container == nil || idx < 0 || idx+1 >= len(container.Content) {
		return 0
	}
	return container.Content[idx+1].Line
}

// decodeDocuments parses every YAML document in data and returns each
// document's root node (empty documents dropped). Splitting this out is
// what lets parseRuleFile SEE that a file holds more than one document:
// yaml.Unmarshal silently decodes only the first, which is how a rule file
// shaped `groups: ...\n---\ngroups: ...` used to hand back positions valid
// in one document and an end bound taken from the whole file.
func decodeDocuments(data []byte) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		root := &doc
		if doc.Kind == yaml.DocumentNode {
			if len(doc.Content) == 0 {
				continue
			}
			root = doc.Content[0]
		}
		if root.Kind != yaml.MappingNode {
			// Not something a rule file is ever shaped like, but it is still
			// a document: it has to be counted, or a `---`-separated pair
			// would look like a single document again.
			out = append(out, root)
			continue
		}
		out = append(out, root)
	}
}

// fileLines splits data into its 1-indexed lines (lines[0] is line 1). A
// trailing newline is not itself a numbered line, so a file ending "...\n"
// has one fewer line than the raw split count.
func fileLines(data []byte) []string {
	lines := strings.Split(string(data), "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// trimSpan walks `end` back to the last line at or after `floor` that
// carries content -- skipping blank lines and whole-line comments, which sit
// between two rules and belong, to any human reading the file, to the one
// that follows.
//
// floor is the last line the YAML parser itself attributes to the node, so
// the trim can never cut into the node's own value: a literal block scalar
// whose final line happens to be a PromQL `#` comment keeps that line.
func trimSpan(lines []string, floor, end int) int {
	if end > len(lines) {
		end = len(lines)
	}
	if floor < 1 {
		floor = 1
	}
	if end <= floor {
		return end
	}
	for ln := end; ln > floor; ln-- {
		t := strings.TrimSpace(lines[ln-1])
		if t != "" && !strings.HasPrefix(t, "#") {
			return ln
		}
	}
	return floor
}

// nodeEndLine is the last line a node's own content occupies -- the deepest
// line the parser attributes to it or to anything nested inside it.
//
// yaml.Node carries a start position and no end, which is fine for every
// node whose value sits on one line. A literal or folded block scalar is
// the exception: its node Line is the `|`/`>` indicator, and its value then
// runs for as many lines as the value has. Counting those is what keeps
// trimSpan from mistaking a block scalar's last line for trailing filler.
func nodeEndLine(n *yaml.Node) int {
	if n == nil {
		return 0
	}
	end := n.Line
	if n.Kind == yaml.ScalarNode && (n.Style == yaml.LiteralStyle || n.Style == yaml.FoldedStyle) {
		if v := strings.TrimSuffix(n.Value, "\n"); v != "" {
			end = n.Line + strings.Count(v, "\n") + 1
		}
	}
	for _, c := range n.Content {
		if e := nodeEndLine(c); e > end {
			end = e
		}
	}
	return end
}
