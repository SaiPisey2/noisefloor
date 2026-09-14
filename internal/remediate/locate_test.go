package remediate

import (
	"os"
	"path/filepath"
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
	// DemoSpiky ends the line before DemoFlapping starts (line 11), i.e. 10;
	// the blank line at 9 is counted as trailing part of DemoSpiky's span,
	// which is the documented next-sibling boundary behaviour.
	if spiky.EndLine != 10 {
		t.Errorf("DemoSpiky.EndLine = %d, want 10", spiky.EndLine)
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
	// the file's own last line.
	wantLast := countLines([]byte(sampleRuleFile))
	if other.EndLine != wantLast {
		t.Errorf("OtherRule.EndLine = %d, want %d (end of file)", other.EndLine, wantLast)
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
