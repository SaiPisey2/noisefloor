package remediate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleRuleFile = `groups:
  - name: demo
    interval: 15s
    rules:
      # Fires and resolves itself.
      - alert: DemoSpiky
        expr: demo_spiky_gauge > 0
        labels: {severity: warning}
        annotations: {summary: "Spiky signal breached"}

      - alert: DemoFlapping
        expr: demo_flapping_gauge > 0
        for: 30s
        labels: {severity: warning}
        annotations: {summary: "Flapping signal breached"}

      - alert: DemoMultiline
        expr: |
          sum(rate(demo_errors_total[5m]))
          /
          sum(rate(demo_requests_total[5m])) > 0.1
        for: 2m
  - name: other
    rules:
      - alert: OtherRule
        expr: up == 0
`

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestLocateRules_positions(t *testing.T) {
	dir := t.TempDir()
	path := writeTemp(t, dir, "demo.yml", sampleRuleFile)

	locs, fileErrs, err := LocateRules(dir)
	if err != nil {
		t.Fatalf("LocateRules: %v", err)
	}
	if len(fileErrs) != 0 {
		t.Fatalf("unexpected file errors: %v", fileErrs)
	}

	byName := map[string]RuleLocation{}
	for _, l := range locs {
		byName[l.Key.Group+"/"+l.Key.AlertName] = l
	}
	if len(byName) != 4 {
		t.Fatalf("got %d rules, want 4: %+v", len(byName), locs)
	}

	spiky, ok := byName["demo/DemoSpiky"]
	if !ok {
		t.Fatal("DemoSpiky not found")
	}
	if spiky.File != path {
		t.Errorf("DemoSpiky.File = %q, want %q", spiky.File, path)
	}
	if spiky.StartLine != 6 {
		t.Errorf("DemoSpiky.StartLine = %d, want 6", spiky.StartLine)
	}
	if spiky.ForLine != 0 {
		t.Errorf("DemoSpiky.ForLine = %d, want 0 (no for:)", spiky.ForLine)
	}
	if spiky.ExprLine != 7 {
		t.Errorf("DemoSpiky.ExprLine = %d, want 7", spiky.ExprLine)
	}
	if spiky.ExprEndLine != spiky.ExprLine {
		t.Errorf("DemoSpiky.ExprEndLine = %d, want %d (single-line expr)", spiky.ExprEndLine, spiky.ExprLine)
	}
	// DemoSpiky's last field is its `annotations:` on line 9. The next
	// sibling (DemoFlapping) starts on line 11, so the untrimmed span would
	// reach line 10 -- the blank line separating the two rules, which is not
	// DemoSpiky's to delete.
	if spiky.EndLine != 9 {
		t.Errorf("DemoSpiky.EndLine = %d, want 9 (trimmed off the trailing blank line)", spiky.EndLine)
	}
	if spiky.ExprValue != "demo_spiky_gauge > 0" {
		t.Errorf("DemoSpiky.ExprValue = %q", spiky.ExprValue)
	}
	if spiky.ForValue != "" {
		t.Errorf("DemoSpiky.ForValue = %q, want empty (no for:)", spiky.ForValue)
	}

	flapping, ok := byName["demo/DemoFlapping"]
	if !ok {
		t.Fatal("DemoFlapping not found")
	}
	if flapping.StartLine != 11 {
		t.Errorf("DemoFlapping.StartLine = %d, want 11", flapping.StartLine)
	}
	if flapping.ForLine != 13 {
		t.Errorf("DemoFlapping.ForLine = %d, want 13", flapping.ForLine)
	}
	if flapping.ForValue != "30s" {
		t.Errorf("DemoFlapping.ForValue = %q, want 30s", flapping.ForValue)
	}

	multiline, ok := byName["demo/DemoMultiline"]
	if !ok {
		t.Fatal("DemoMultiline not found")
	}
	if multiline.ExprLine != 18 {
		t.Errorf("DemoMultiline.ExprLine = %d, want 18", multiline.ExprLine)
	}
	// The block scalar spans lines 19-21; ExprEndLine must reach past
	// ExprLine to cover it, bounded by the next field (`for:`, line 22).
	if multiline.ExprEndLine <= multiline.ExprLine {
		t.Errorf("DemoMultiline.ExprEndLine = %d, want > ExprLine (%d) for a block scalar",
			multiline.ExprEndLine, multiline.ExprLine)
	}
	if multiline.ExprEndLine != 21 {
		t.Errorf("DemoMultiline.ExprEndLine = %d, want 21", multiline.ExprEndLine)
	}
	if multiline.ForLine != 22 {
		t.Errorf("DemoMultiline.ForLine = %d, want 22", multiline.ForLine)
	}
	// DemoMultiline is the last rule of the first group; its end is bounded
	// by the next group ("other", line 23), so EndLine = 22.
	if multiline.EndLine != 22 {
		t.Errorf("DemoMultiline.EndLine = %d, want 22", multiline.EndLine)
	}

	other, ok := byName["other/OtherRule"]
	if !ok {
		t.Fatal("OtherRule not found")
	}
	// OtherRule is the very last rule in the file; its end is bounded by
	// the file's own last line, which here is its own `expr:`.
	wantLast := len(fileLines([]byte(sampleRuleFile)))
	if other.EndLine != wantLast {
		t.Errorf("OtherRule.EndLine = %d, want %d (end of file)", other.EndLine, wantLast)
	}
}

// TestLocateRules_multiDocumentFileRefused is the destructive case behind
// issue "multi-document YAML destroys the rest of the file": decoding only
// the first document while bounding the last rule by the whole file's line
// count makes that rule's span swallow every later document.
func TestLocateRules_multiDocumentFileRefused(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "multi.yml", `groups:
  - name: first
    rules:
      - alert: FirstDoc
        expr: up == 0
---
groups:
  - name: second
    rules:
      - alert: SecondDoc
        expr: up == 1
      - alert: AlsoSecond
        expr: up == 2
`)
	locs, fileErrs, err := LocateRules(dir)
	if err != nil {
		t.Fatalf("LocateRules: %v", err)
	}
	if len(locs) != 0 {
		t.Fatalf("a multi-document rule file must yield no locations at all, got %+v", locs)
	}
	if len(fileErrs) != 1 {
		t.Fatalf("got %d file errors, want 1: %v", len(fileErrs), fileErrs)
	}
	msg := fileErrs[0].Error()
	if !strings.Contains(msg, "multi.yml") {
		t.Errorf("error does not name the offending file: %q", msg)
	}
	if !strings.Contains(msg, "document") {
		t.Errorf("error does not explain the multi-document refusal: %q", msg)
	}
}

// TestLocateRules_singleDocumentWithLeadingSeparator guards the refusal
// against being over-eager: a leading `---` is one document, not two, and is
// an entirely ordinary way to write a rule file.
func TestLocateRules_singleDocumentWithLeadingSeparator(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "lead.yml", "---\n"+sampleRuleFile)
	locs, fileErrs, err := LocateRules(dir)
	if err != nil {
		t.Fatalf("LocateRules: %v", err)
	}
	if len(fileErrs) != 0 {
		t.Fatalf("a leading --- is a single document, not a refusal: %v", fileErrs)
	}
	if len(locs) != 4 {
		t.Fatalf("got %d locations, want 4", len(locs))
	}
}

// TestLocateRules_multiDocumentNonRuleFileStaysSilent keeps the refusal off
// files that are none of noisefloor's business. A checkout of Kubernetes
// manifests is full of `---`-separated documents, and warning about every
// one of them would bury the warnings that matter.
func TestLocateRules_multiDocumentNonRuleFileStaysSilent(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "manifests.yaml", "kind: Service\nmetadata:\n  name: a\n---\nkind: Deployment\nmetadata:\n  name: b\n")
	locs, fileErrs, err := LocateRules(dir)
	if err != nil {
		t.Fatalf("LocateRules: %v", err)
	}
	if len(locs) != 0 || len(fileErrs) != 0 {
		t.Fatalf("got locs=%+v errs=%v, want both empty", locs, fileErrs)
	}
}

// TestLocateRules_duplicateNameInOneGroup is the "bot edits the wrong rule"
// case. Two rules with the same alert name in the SAME group is legal
// Prometheus; collect.SyncRules' AmbiguousNames only sees the same name
// across DIFFERENT groups, so nothing else in the pipeline catches this.
func TestLocateRules_duplicateNameInOneGroup(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "dupe.yml", `groups:
  - name: demo
    rules:
      - alert: SameName
        expr: up == 0
        for: 1m
      - alert: SameName
        expr: up == 1
        for: 9m
      - alert: Unique
        expr: up == 2
`)
	locs, fileErrs, err := LocateRules(dir)
	if err != nil {
		t.Fatalf("LocateRules: %v", err)
	}
	if len(locs) != 3 {
		t.Fatalf("got %d locations, want 3 (both duplicates must still be reported)", len(locs))
	}

	dupes := 0
	for _, l := range locs {
		switch l.Key.AlertName {
		case "SameName":
			if !l.Duplicate {
				t.Errorf("SameName at line %d is not marked Duplicate", l.StartLine)
			}
			dupes++
		case "Unique":
			if l.Duplicate {
				t.Error("Unique must not be marked Duplicate")
			}
		}
	}
	if dupes != 2 {
		t.Errorf("found %d SameName locations, want 2", dupes)
	}

	// Whichever copy the index keeps still carries the flag, so a caller
	// indexing by key cannot lose the warning.
	kept, ok := LocationsByKey(locs)[RuleKey{Group: "demo", AlertName: "SameName"}]
	if !ok || !kept.Duplicate {
		t.Errorf("LocationsByKey dropped the duplicate flag: %+v", kept)
	}

	if len(fileErrs) != 1 {
		t.Fatalf("got %d file errors, want 1 naming the clash: %v", len(fileErrs), fileErrs)
	}
	if msg := fileErrs[0].Error(); !strings.Contains(msg, "SameName") || !strings.Contains(msg, "dupe.yml") {
		t.Errorf("error does not name the rule and file: %q", msg)
	}
}

// TestLocateRules_spanStopsBeforeFollowingComments is finding 7: the span
// must not absorb the blank line and doc comments that introduce the NEXT
// rule, nor trailing top-level comments at the end of a file.
func TestLocateRules_spanStopsBeforeFollowingComments(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "commented.yml", `groups:
  - name: demo
    rules:
      - alert: First
        expr: up == 0

      # This comment documents Second, and a commented-out draft of a rule
      # nobody has deleted yet lives underneath it:
      #  - alert: Draft
      #    expr: up == 3
      - alert: Second
        expr: up == 1

# A trailing note about the file as a whole.
# Not Second's to delete either.
`)
	locs, _, err := LocateRules(dir)
	if err != nil {
		t.Fatalf("LocateRules: %v", err)
	}
	byName := map[string]RuleLocation{}
	for _, l := range locs {
		byName[l.Key.AlertName] = l
	}

	first := byName["First"]
	if first.StartLine != 4 || first.EndLine != 5 {
		t.Errorf("First spans %d..%d, want 4..5 (Second's doc comments are not First's)",
			first.StartLine, first.EndLine)
	}
	second := byName["Second"]
	if second.StartLine != 11 || second.EndLine != 12 {
		t.Errorf("Second spans %d..%d, want 11..12 (the trailing file comments are not Second's)",
			second.StartLine, second.EndLine)
	}
}

// TestLocateRules_blockScalarEndingInAComment guards the trim from cutting
// into a rule's own value: `#` starts a comment in PromQL too, so the last
// line of a literal block scalar can legitimately look like filler.
func TestLocateRules_blockScalarEndingInAComment(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "block.yml", `groups:
  - name: demo
    rules:
      - alert: Blocky
        expr: |
          sum(rate(errors_total[5m])) > 0
          # the ratio is deliberately not normalised here
      - alert: After
        expr: up == 0
`)
	locs, _, err := LocateRules(dir)
	if err != nil {
		t.Fatalf("LocateRules: %v", err)
	}
	var blocky RuleLocation
	for _, l := range locs {
		if l.Key.AlertName == "Blocky" {
			blocky = l
		}
	}
	if blocky.EndLine != 7 {
		t.Errorf("Blocky.EndLine = %d, want 7: the last line of a block scalar is the rule's own "+
			"value, not a trailing comment", blocky.EndLine)
	}
	if blocky.ExprEndLine != 7 {
		t.Errorf("Blocky.ExprEndLine = %d, want 7", blocky.ExprEndLine)
	}
}

func TestLocateRules_recordingRuleSkipped(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "rec.yml", `groups:
  - name: g
    rules:
      - record: some:recording:rule
        expr: sum(rate(x[5m]))
      - alert: RealAlert
        expr: up == 0
`)
	locs, fileErrs, err := LocateRules(dir)
	if err != nil {
		t.Fatalf("LocateRules: %v", err)
	}
	if len(fileErrs) != 0 {
		t.Fatalf("unexpected file errors: %v", fileErrs)
	}
	if len(locs) != 1 || locs[0].Key.AlertName != "RealAlert" {
		t.Fatalf("got %+v, want only RealAlert", locs)
	}
}

func TestLocateRules_nonRuleFileSkippedSilently(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "values.yaml", "replicaCount: 3\nimage:\n  repo: foo\n")
	locs, fileErrs, err := LocateRules(dir)
	if err != nil {
		t.Fatalf("LocateRules: %v", err)
	}
	if len(locs) != 0 {
		t.Fatalf("got %d locations from a non-rule file, want 0", len(locs))
	}
	if len(fileErrs) != 0 {
		t.Fatalf("a valid-YAML-but-not-a-rule-file must not be reported as an error: %v", fileErrs)
	}
}

func TestLocateRules_malformedYAMLIsSoftError(t *testing.T) {
	dir := t.TempDir()
	writeTemp(t, dir, "broken.yml", "groups:\n  - name: [unterminated\n")
	writeTemp(t, dir, "good.yml", sampleRuleFile)

	locs, fileErrs, err := LocateRules(dir)
	if err != nil {
		t.Fatalf("a malformed file must not fail the whole walk: %v", err)
	}
	if len(fileErrs) != 1 {
		t.Fatalf("got %d file errors, want 1: %v", len(fileErrs), fileErrs)
	}
	if len(locs) != 4 {
		t.Fatalf("got %d locations from good.yml, want 4 (broken.yml must not block it)", len(locs))
	}
}

func TestLocateRules_skipsDotGit(t *testing.T) {
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A file that would fail to parse if it were ever visited.
	writeTemp(t, gitDir, "config.yml", "not: [valid")
	writeTemp(t, dir, "demo.yml", sampleRuleFile)

	_, fileErrs, err := LocateRules(dir)
	if err != nil {
		t.Fatalf("LocateRules: %v", err)
	}
	if len(fileErrs) != 0 {
		t.Fatalf(".git must be skipped entirely, got errors: %v", fileErrs)
	}
}

func TestLocateRules_missingRoot(t *testing.T) {
	_, _, err := LocateRules(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("want an error for a missing rules path")
	}
}

func TestLinesByKey(t *testing.T) {
	locs := []RuleLocation{
		{Key: RuleKey{Group: "demo", AlertName: "A"}, StartLine: 5},
		{Key: RuleKey{Group: "demo", AlertName: "B"}, StartLine: 12},
	}
	lines := LinesByKey(locs)
	if lines[RuleKey{Group: "demo", AlertName: "A"}] != 5 {
		t.Errorf("A line = %d, want 5", lines[RuleKey{Group: "demo", AlertName: "A"}])
	}
	if lines[RuleKey{Group: "demo", AlertName: "B"}] != 12 {
		t.Errorf("B line = %d, want 12", lines[RuleKey{Group: "demo", AlertName: "B"}])
	}
	if _, ok := lines[RuleKey{Group: "demo", AlertName: "C"}]; ok {
		t.Error("unexpected entry for a rule never located")
	}
}
