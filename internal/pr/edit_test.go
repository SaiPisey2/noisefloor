package pr

import (
	"os"
	"strings"
	"testing"
	"time"
)

const fixturePath = "testdata/fixture.yml"
const crlfFixturePath = "testdata/crlf.yml"

// TestApplyDeletePreservesEverythingOutsideTheSpan exercises a retire edit
// against the fixture: RetireMe spans lines 9-14, and every other line --
// including the comments that introduce TuneMe, TuneMe's odd for: spacing
// and inline comment, and every other comment in the file -- must survive
// untouched.
func TestApplyDeletePreservesEverythingOutsideTheSpan(t *testing.T) {
	e := Edit{File: fixturePath, DeleteStart: 9, DeleteEnd: 14}
	_, newContent, diff, err := e.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	orig, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	origLines := strings.Split(strings.TrimSuffix(string(orig), "\n"), "\n")
	wantLines := append(append([]string{}, origLines[:8]...), origLines[14:]...)
	want := strings.Join(wantLines, "\n") + "\n"

	if newContent != want {
		t.Errorf("delete edit changed unrelated content.\ngot:\n%s\nwant:\n%s", newContent, want)
	}
	// The rule is gone; the comment that merely mentions it by name (and
	// belongs to TuneMe) is deliberately still there.
	if strings.Contains(newContent, "alert: RetireMe") {
		t.Error("deleted rule's alert declaration still present")
	}
	if !strings.Contains(newContent, "TuneMe") || !strings.Contains(newContent, "NoForField") {
		t.Error("unrelated rules were removed along with RetireMe")
	}
	// The rest of the file's comments must be untouched, byte for byte.
	for _, must := range []string{
		"# Fixture rule file for internal/pr's diff-generation tests",
		"# and TuneMe. They introduce TuneMe; they are not RetireMe's, and a",
		"# keep this pager quiet during a deploy",
	} {
		if !strings.Contains(newContent, must) {
			t.Errorf("expected surviving text %q not found in output", must)
		}
	}

	if !strings.Contains(diff, "-      - alert: RetireMe") {
		t.Errorf("diff does not show the deleted alert line:\n%s", diff)
	}
	if strings.Contains(diff, "-      - alert: TuneMe") {
		t.Errorf("diff touches an unrelated rule:\n%s", diff)
	}
}

// TestApplyReplacePreservesIndentationAndTrailingComment is the core
// surgical-diff guarantee for a tune edit: TuneMe's for: line has unusual
// internal spacing and a trailing inline comment, both of which must come
// through unchanged around the replaced value.
func TestApplyReplacePreservesIndentationAndTrailingComment(t *testing.T) {
	original, err := readLine(fixturePath, 22)
	if err != nil {
		t.Fatal(err)
	}
	if original != "        for:    1m   # keep this pager quiet during a deploy" {
		t.Fatalf("fixture line 22 changed out from under this test: %q", original)
	}

	replaced, err := replaceForValue(original, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := "        for:    1m30s   # keep this pager quiet during a deploy"
	if replaced != want {
		t.Errorf("replaceForValue = %q, want %q", replaced, want)
	}

	e := Edit{File: fixturePath, ReplaceLine: 22, ReplaceWith: replaced}
	_, newContent, diff, err := e.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(newContent, want) {
		t.Errorf("new content missing replaced line:\n%s", newContent)
	}
	if !strings.Contains(newContent, "labels:\n          team: payments") {
		t.Error("TuneMe's other fields were disturbed by the replace")
	}
	if strings.Count(newContent, "for:") != strings.Count(mustRead(t, fixturePath), "for:") {
		t.Error("replace changed the number of for: occurrences in the file")
	}

	if !strings.Contains(diff, "-        for:    1m   # keep this pager quiet during a deploy") ||
		!strings.Contains(diff, "+        for:    1m30s   # keep this pager quiet during a deploy") {
		t.Errorf("diff does not show a clean one-line for: replacement:\n%s", diff)
	}
}

// TestApplyInsertAddsAForFieldAtMatchingIndentation covers a rule with no
// existing for: line (NoForField): the new line must be inserted right
// after expr:, indented to match it.
func TestApplyInsertAddsAForFieldAtMatchingIndentation(t *testing.T) {
	e := NewInsert(fixturePath, 29, "        for: 2m")
	_, newContent, diff, err := e.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(newContent, "        expr: up == 0\n        for: 2m\n") {
		t.Errorf("inserted line not found in the expected place:\n%s", newContent)
	}
	if !strings.Contains(diff, "+        for: 2m") {
		t.Errorf("diff does not show the inserted line:\n%s", diff)
	}
}

// TestApplyReplaceMatchesCRLFLineEndings is the mixed-endings defect: a
// rewritten line built by this package is LF-terminated, and dropping it
// into a CRLF file leaves exactly one line with the wrong ending. Git
// reports that as a change the PR body never mentions.
func TestApplyReplaceMatchesCRLFLineEndings(t *testing.T) {
	original, err := readLine(crlfFixturePath, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(original, "\r") {
		t.Fatalf("crlf fixture line 9 is not CRLF-terminated: %q", original)
	}
	replaced, err := replaceForValue(original, 6*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	e := Edit{File: crlfFixturePath, ReplaceLine: 9, ReplaceWith: replaced}
	_, newContent, _, err := e.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertAllCRLF(t, newContent)
	if !strings.Contains(newContent, "        for: 6m\r\n") {
		t.Errorf("replaced for: line is not CRLF-terminated:\n%q", newContent)
	}
}

// TestApplyInsertMatchesCRLFLineEndings is the same defect on the insert
// path: a `for:` line added to a rule that had none.
func TestApplyInsertMatchesCRLFLineEndings(t *testing.T) {
	e := NewInsert(crlfFixturePath, 13, "        for: 2m")
	_, newContent, _, err := e.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertAllCRLF(t, newContent)
	if !strings.Contains(newContent, "        expr: up == 0\r\n        for: 2m\r\n") {
		t.Errorf("inserted line is not CRLF-terminated:\n%q", newContent)
	}
}

// TestApplyDeleteLeavesCRLFFileUntouched: a delete writes no new line at
// all, so every ending in the result must be one the file already had.
func TestApplyDeleteLeavesCRLFFileUntouched(t *testing.T) {
	e := Edit{File: crlfFixturePath, DeleteStart: 12, DeleteEnd: 13}
	_, newContent, _, err := e.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertAllCRLF(t, newContent)
	if strings.Contains(newContent, "CRLFNoFor") {
		t.Error("deleted rule still present")
	}
}

// TestApplyLeavesLFFileAlone guards the CRLF handling from doing the
// reverse damage: an LF file must never acquire a "\r".
func TestApplyLeavesLFFileAlone(t *testing.T) {
	e := NewInsert(fixturePath, 29, "        for: 2m")
	_, newContent, _, err := e.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if strings.Contains(newContent, "\r") {
		t.Errorf("an LF file gained a carriage return:\n%q", newContent)
	}
}

// assertAllCRLF fails unless every line ending in s is "\r\n" -- i.e. there
// is no bare "\n" anywhere, which is what "mixed endings" looks like.
func assertAllCRLF(t *testing.T, s string) {
	t.Helper()
	for i, line := range strings.SplitAfter(s, "\n") {
		if line == "" || !strings.HasSuffix(line, "\n") {
			continue // the trailing remainder after the last newline
		}
		if !strings.HasSuffix(line, "\r\n") {
			t.Errorf("line %d is LF-terminated in a CRLF file: %q", i+1, line)
		}
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
