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
	InsertWith  string // full new line, no trailing newline

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

// Apply reads e.File, applies the edit, and returns the resulting full file
// content plus a unified-diff-style patch for human review.
func (e Edit) Apply() (newContent, diff string, err error) {
	data, err := os.ReadFile(e.File)
	if err != nil {
		return "", "", fmt.Errorf("read %s: %w", e.File, err)
	}
	text := string(data)
	trailingNewline := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")

	switch {
	case e.DeleteStart > 0:
		return e.applyDelete(lines, trailingNewline)
	case e.ReplaceLine > 0:
		return e.applyReplace(lines, trailingNewline)
	case e.hasInsert:
		return e.applyInsert(lines, trailingNewline)
	default:
		return "", "", fmt.Errorf("edit for %s specifies no change", e.File)
	}
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

	var out []string
	out = append(out, lines[:e.InsertAfter]...)
	out = append(out, e.InsertWith)
	out = append(out, lines[e.InsertAfter:]...)

	lo := clampLo(e.InsertAfter - diffContext + 1)
	hi := clampHi(e.InsertAfter+diffContext, len(lines))
	oldCount := hi - lo + 1
	newCount := oldCount + 1

	var body strings.Builder
	for ln := lo; ln <= hi; ln++ {
		fmt.Fprintf(&body, " %s\n", lines[ln-1])
		if ln == e.InsertAfter {
			fmt.Fprintf(&body, "+%s\n", e.InsertWith)
		}
	}
	if e.InsertAfter == 0 {
		// Inserting before line 1: the body loop above never visits ln==0,
		// so the "+" line has to be emitted up front instead.
		body.Reset()
		fmt.Fprintf(&body, "+%s\n", e.InsertWith)
		for ln := lo; ln <= hi; ln++ {
			fmt.Fprintf(&body, " %s\n", lines[ln-1])
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
