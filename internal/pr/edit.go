// Package pr turns a scored rule into a proposed pull request: a minimal,
// line-precise edit to its source file, a deterministic branch name, and a
// body a reviewer who does not trust the tool can check by hand. It stays
// GitHub-agnostic behind Provider so another forge can implement the same
// interface without restructuring this package.
package pr

import (
	"fmt"
	"os"
	"strings"
)

// diffContext is how many unchanged lines surround a change in the
// human-readable patch this package prints. It is display-only -- the
// actual edit applied is exactly the lines named by the Edit, nothing more.
const diffContext = 3

// Edit describes one minimal, line-precise change to a single rule file.
// Exactly one of the three shapes is populated:
//
//   - Delete (DeleteStart/DeleteEnd set): remove a contiguous, inclusive,
//     1-indexed line range entirely. Used by a retire proposal to remove a
//     whole rule block -- remediate.RuleLocation.StartLine/EndLine already
//     bound exactly that span.
//   - Replace (ReplaceLine set): replace one existing line in place. Used by
//     a tune proposal to change an existing `for:` line's value while
//     preserving its indentation and any trailing comment.
//   - Insert (InsertAfter set, possibly to 0 for "at the top of the file"):
//     add one new line immediately after an existing line. Used by a tune
//     proposal when the rule has no `for:` field to replace.
//
// In every case, every line the Edit does not name -- comments, unusual
// indentation, unrelated rules -- passes through completely unchanged: this
// type has no notion of "reformat" or "reorder", only "this line goes",
// "this line becomes that line" or "this line is new".
type Edit struct {
	File string

	DeleteStart, DeleteEnd int // 1-indexed inclusive; 0,0 = no deletion

	ReplaceLine int    // 1-indexed; 0 = no replacement
	ReplaceWith string // full replacement line, no trailing newline

	InsertAfter int    // 1-indexed, or 0 for "before the first line"
	InsertWith  string // full new line, no trailing newline -- single-line insert (a tune's for:)

	// InsertLines is the multi-line form of InsertWith: a whole block of new
	// lines (a starter rule's `- alert: ... / expr: ... / for: ...`),
	// inserted as a unit immediately after InsertAfter. Exactly one of
	// InsertWith and InsertLines is set on an insert Edit; each line is
	// already fully indented and contains no embedded newline of its own,
	// so the diff can show one "+" per line the way a human reviewer
	// expects, rather than one "+" in front of an opaque multi-line blob.
	InsertLines []string

	// hasInsert distinguishes "insert before line 1" (InsertAfter == 0,
	// deliberately) from "no insert configured" (also InsertAfter == 0, by
	// zero value), since Delete/Replace can't be ambiguous like this: their
	// zero value (0) is never a valid 1-indexed line number, but 0 IS a
	// valid InsertAfter.
	hasInsert bool
}

// NewInsert builds an Edit that inserts newLine immediately after line
// afterLine (0 for the top of the file).
func NewInsert(file string, afterLine int, newLine string) Edit {
	return Edit{File: file, InsertAfter: afterLine, InsertWith: newLine, hasInsert: true}
}

// NewInsertBlock builds an Edit that inserts several new lines, as a unit,
// immediately after line afterLine (0 for the top of the file). Used by a
// starter proposal (internal/pr's coverage-blind-spot templates) to add a
// whole new rule -- alert/expr/for/labels/annotations -- to an existing
// group's rules: list in one edit.
func NewInsertBlock(file string, afterLine int, lines []string) Edit {
	return Edit{File: file, InsertAfter: afterLine, InsertLines: lines, hasInsert: true}
}

// lines returns this Edit's insert content as a slice, whichever of
// InsertWith/InsertLines was set.
func (e Edit) lines() []string {
	if len(e.InsertLines) > 0 {
		return e.InsertLines
	}
	return []string{e.InsertWith}
}

// Apply reads e.File, applies the edit, and returns the file's content as
// it was read, the resulting full content, and a unified-diff-style patch
// for human review.
//
// oldContent is returned rather than left for the caller to read again
// because it is what the forge provider compares against the blob it is
// about to overwrite (see Provider.CommitFiles and FileChange.BaseContent).
// Two separate reads would leave a window in which the file changed between
// them, which is precisely the state that check exists to detect.
func (e Edit) Apply() (oldContent, newContent, diff string, err error) {
	data, err := os.ReadFile(e.File)
	if err != nil {
		return "", "", "", fmt.Errorf("read %s: %w", e.File, err)
	}
	text := string(data)
	trailingNewline := strings.HasSuffix(text, "\n")

	// Split on "\n" only, which leaves a CRLF file's "\r" on the end of each
	// line. That is deliberate: every line this edit does not name then
	// round-trips byte for byte, including its ending. The one line the edit
	// DOES write is the one that has to be made to match -- see matchEOL.
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	crlf := dominantCRLF(text)

	var out string
	switch {
	case e.DeleteStart > 0:
		out, diff, err = e.applyDelete(lines, trailingNewline)
	case e.ReplaceLine > 0:
		e.ReplaceWith = matchEOL(e.ReplaceWith, crlf)
		out, diff, err = e.applyReplace(lines, trailingNewline)
	case e.hasInsert:
		inserted := e.lines()
		for i, l := range inserted {
			inserted[i] = matchEOL(l, crlf)
		}
		e.InsertLines = inserted
		out, diff, err = e.applyInsert(lines, trailingNewline)
	default:
		err = fmt.Errorf("edit for %s specifies no change", e.File)
	}
	if err != nil {
		return "", "", "", err
	}
	return text, out, diff, nil
}

// dominantCRLF reports whether the file is predominantly CRLF-terminated.
//
// Predominantly, not exclusively: a file that is already mixed still has one
// ending that belongs there, and a single stray line is not a reason to
// write the minority ending into it. Ties go to LF, which is what a file
// with no line endings at all (a single line, no trailing newline) should
// get.
func dominantCRLF(text string) bool {
	crlf := strings.Count(text, "\r\n")
	lf := strings.Count(text, "\n") - crlf
	return crlf > lf
}

// matchEOL gives a line this package constructed -- a rewritten `for:` line,
// or a newly inserted one -- the file's own line ending.
//
// Without it, every replaced or inserted line in a CRLF rule file arrives
// LF-terminated while its neighbours stay CRLF. Git shows that as the line
// having changed in a way the PR body never mentions, some YAML tooling
// treats the leftover "\r" of the surrounding lines inconsistently, and a
// reviewer is left explaining a whitespace-only mystery in someone else's
// repository. The edit is supposed to be invisible apart from the value.
func matchEOL(line string, crlf bool) string {
	line = strings.TrimSuffix(line, "\r")
	if crlf {
		return line + "\r"
	}
	return line
}

func join(lines []string, trailingNewline bool) string {
	out := strings.Join(lines, "\n")
	if trailingNewline {
		out += "\n"
	}
	return out
}

func (e Edit) applyDelete(lines []string, trailingNewline bool) (string, string, error) {
	if e.DeleteEnd < e.DeleteStart {
		return "", "", fmt.Errorf("%s: delete range %d..%d is empty or inverted", e.File, e.DeleteStart, e.DeleteEnd)
	}
	if e.DeleteEnd > len(lines) {
		return "", "", fmt.Errorf("%s: delete range %d..%d exceeds the file's %d lines", e.File, e.DeleteStart, e.DeleteEnd, len(lines))
	}

	var out []string
	out = append(out, lines[:e.DeleteStart-1]...)
	out = append(out, lines[e.DeleteEnd:]...)

	lo := clampLo(e.DeleteStart - diffContext)
	hi := clampHi(e.DeleteEnd+diffContext, len(lines))
	oldCount := hi - lo + 1
	newCount := oldCount - (e.DeleteEnd - e.DeleteStart + 1)

	var body strings.Builder
	for ln := lo; ln <= hi; ln++ {
		if ln >= e.DeleteStart && ln <= e.DeleteEnd {
			fmt.Fprintf(&body, "-%s\n", lines[ln-1])
			continue
		}
		fmt.Fprintf(&body, " %s\n", lines[ln-1])
	}

	diff := unifiedDiffHeader(e.File, lo, oldCount, lo, newCount) + body.String()
	return join(out, trailingNewline), diff, nil
}

func (e Edit) applyReplace(lines []string, trailingNewline bool) (string, string, error) {
	if e.ReplaceLine > len(lines) {
		return "", "", fmt.Errorf("%s: replace line %d exceeds the file's %d lines", e.File, e.ReplaceLine, len(lines))
	}

	out := append([]string(nil), lines...)
	out[e.ReplaceLine-1] = e.ReplaceWith

	lo := clampLo(e.ReplaceLine - diffContext)
	hi := clampHi(e.ReplaceLine+diffContext, len(lines))
	count := hi - lo + 1

	var body strings.Builder
	for ln := lo; ln <= hi; ln++ {
		if ln == e.ReplaceLine {
			fmt.Fprintf(&body, "-%s\n", lines[ln-1])
			fmt.Fprintf(&body, "+%s\n", e.ReplaceWith)
			continue
		}
		fmt.Fprintf(&body, " %s\n", lines[ln-1])
	}

	// A replace does not change the file's line count, so old and new
	// counts are identical.
	diff := unifiedDiffHeader(e.File, lo, count, lo, count) + body.String()
	return join(out, trailingNewline), diff, nil
}

func (e Edit) applyInsert(lines []string, trailingNewline bool) (string, string, error) {
	if e.InsertAfter > len(lines) {
		return "", "", fmt.Errorf("%s: insert-after line %d exceeds the file's %d lines", e.File, e.InsertAfter, len(lines))
	}
	newLines := e.InsertLines

	var out []string
	out = append(out, lines[:e.InsertAfter]...)
	out = append(out, newLines...)
	out = append(out, lines[e.InsertAfter:]...)

	lo := clampLo(e.InsertAfter - diffContext + 1)
	hi := clampHi(e.InsertAfter+diffContext, len(lines))
	oldCount := hi - lo + 1
	newCount := oldCount + len(newLines)

	insertedBlock := func(b *strings.Builder) {
		for _, l := range newLines {
			fmt.Fprintf(b, "+%s\n", l)
		}
	}

	var body strings.Builder
	if e.InsertAfter == 0 {
		// Inserting before line 1: the loop below never visits ln==0, so the
		// "+" lines have to be emitted up front instead.
		insertedBlock(&body)
		for ln := lo; ln <= hi; ln++ {
			fmt.Fprintf(&body, " %s\n", lines[ln-1])
		}
	} else {
		for ln := lo; ln <= hi; ln++ {
			fmt.Fprintf(&body, " %s\n", lines[ln-1])
			if ln == e.InsertAfter {
				insertedBlock(&body)
			}
		}
	}

	diff := unifiedDiffHeader(e.File, lo, oldCount, lo, newCount) + body.String()
	return join(out, trailingNewline), diff, nil
}

func clampLo(i int) int {
	if i < 1 {
		return 1
	}
	return i
}

func clampHi(i, max int) int {
	if i > max {
		return max
	}
	return i
}

func unifiedDiffHeader(path string, oldStart, oldCount, newStart, newCount int) string {
	return fmt.Sprintf("--- a/%s\n+++ b/%s\n@@ -%d,%d +%d,%d @@\n",
		path, path, oldStart, oldCount, newStart, newCount)
}
