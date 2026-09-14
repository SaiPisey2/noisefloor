package pr

import (
	"os"
	"strings"
	"testing"
	"time"
)

const fixturePath = "testdata/fixture.yml"

// TestApplyDeletePreservesEverythingOutsideTheSpan exercises a retire edit
// against the fixture: RetireMe spans lines 9-18 (remediate.LocateRules'
// next-sibling boundary includes the blank line and comment before TuneMe),
// and every other line -- including TuneMe's odd for: spacing and inline
// comment, and every other comment in the file -- must survive untouched.
func TestApplyDeletePreservesEverythingOutsideTheSpan(t *testing.T) {
	e := Edit{File: fixturePath, DeleteStart: 9, DeleteEnd: 18}
	newContent, diff, err := e.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	orig, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	origLines := strings.Split(strings.TrimSuffix(string(orig), "\n"), "\n")
	wantLines := append(append([]string{}, origLines[:8]...), origLines[18:]...)
	want := strings.Join(wantLines, "\n") + "\n"

	if newContent != want {
		t.Errorf("delete edit changed unrelated content.\ngot:\n%s\nwant:\n%s", newContent, want)
	}
	if strings.Contains(newContent, "RetireMe") {
		t.Error("deleted rule's alert name still present")
	}
	if !strings.Contains(newContent, "TuneMe") || !strings.Contains(newContent, "NoForField") {
		t.Error("unrelated rules were removed along with RetireMe")
	}
	// The rest of the file's comments must be untouched, byte for byte.
	for _, must := range []string{
		"# Fixture rule file for internal/pr's diff-generation tests",
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
	original, err := readLine(fixturePath, 21)
	if err != nil {
		t.Fatal(err)
	}
	if original != "        for:    45s   # keep this pager quiet during a deploy" {
		t.Fatalf("fixture line 21 changed out from under this test: %q", original)
	}

	replaced, err := replaceForValue(original, 90*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := "        for:    1m30s   # keep this pager quiet during a deploy"
	if replaced != want {
		t.Errorf("replaceForValue = %q, want %q", replaced, want)
	}

	e := Edit{File: fixturePath, ReplaceLine: 21, ReplaceWith: replaced}
	newContent, diff, err := e.Apply()
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

	if !strings.Contains(diff, "-        for:    45s   # keep this pager quiet during a deploy") ||
		!strings.Contains(diff, "+        for:    1m30s   # keep this pager quiet during a deploy") {
		t.Errorf("diff does not show a clean one-line for: replacement:\n%s", diff)
	}
}

// TestApplyInsertAddsAForFieldAtMatchingIndentation covers a rule with no
// existing for: line (NoForField): the new line must be inserted right
// after expr:, indented to match it.
func TestApplyInsertAddsAForFieldAtMatchingIndentation(t *testing.T) {
	e := NewInsert(fixturePath, 28, "        for: 2m")
	newContent, diff, err := e.Apply()
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

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
