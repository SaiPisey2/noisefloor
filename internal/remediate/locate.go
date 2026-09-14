// Package remediate locates the rule a score belongs to in its source file
// and computes what a proposed change to it would have done to its past
// firing history. It is the evidence layer a pull request needs: a verdict
// is only reviewable once it points at a file and line and states, in a
// checkable sentence, what would have happened under the proposed fix.
package remediate

import (
	"fmt"
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
	// last line that belongs to it, before the next rule, the next group,
	// or the next top-level key begins. The span is a next-sibling
	// boundary, not a tight fit: trailing blank lines or comments between
	// this rule and whatever follows are counted as part of it, which is
	// the same ambiguity a human reading the file has.
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
	return locs, fileErrs, nil
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
func parseRuleFile(path string) ([]RuleLocation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil // empty file
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil
	}

	groupsIdx, groupsNode := mapEntry(root, "groups")
	if groupsNode == nil || groupsNode.Kind != yaml.SequenceNode {
		return nil, nil
	}
	_ = groupsIdx // root has no further sibling we need; groups: end is never asked for

	totalLines := countLines(data)

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

			if _, forNode := mapEntry(ruleNode, "for"); forNode != nil {
				loc.ForLine = forNode.Line
			}

			if exprIdx, exprNode := mapEntry(ruleNode, "expr"); exprNode != nil {
				loc.ExprLine = exprNode.Line
				if end := nextSiblingLine(ruleNode, exprIdx); end > 0 {
					loc.ExprEndLine = end - 1
				} else {
					// expr is the rule's last field: its value cannot run
					// past the rule's own end.
					loc.ExprEndLine = loc.EndLine
				}
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

// countLines is the fallback end-of-file boundary for the last field of the
// last rule of the last group. A trailing newline is not itself a numbered
// line, so a file ending "...\n" has one fewer line than raw split count.
func countLines(data []byte) int {
	lines := strings.Split(string(data), "\n")
	n := len(lines)
	if n > 0 && lines[n-1] == "" {
		n--
	}
	if n < 1 {
		n = 1
	}
	return n
}
