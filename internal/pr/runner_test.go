package pr

import (
	"context"
	"testing"

	"github.com/SaiPisey2/noisefloor/internal/scanner"
	"github.com/SaiPisey2/noisefloor/internal/score"
)

// TestRunOpensExactlyOnePRPerRule is the bulk-PR guard: two rules that both
// qualify must produce two separate branches/PRs, never one PR touching
// both.
func TestRunOpensExactlyOnePRPerRule(t *testing.T) {
	evals := []scanner.RuleEval{retireEval(t), tuneEval(t)}
	provider := NewFakeProvider()

	res, err := Run(context.Background(), provider, evals, RunOptions{
		Owner: "acme", Repo: "rules", Base: "main", RepoRoot: "testdata", Apply: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Opened) != 2 {
		t.Fatalf("opened %d PRs, want 2 (one per rule)", len(res.Opened))
	}
	if res.Opened[0].PR.Branch == res.Opened[1].PR.Branch {
		t.Error("both proposals opened on the same branch")
	}
	// Exactly one committed file per branch: never a PR touching more than
	// the one rule it is about.
	fp := provider
	if len(fp.Files) != 2 {
		t.Errorf("committed files span %d keys, want 2 (one file, one branch, per proposal)", len(fp.Files))
	}
}

// TestRunIsIdempotent is issue #8's idempotence requirement: running twice
// against the same evals must not open a second PR for the same rule and
// the same proposal.
func TestRunIsIdempotent(t *testing.T) {
	evals := []scanner.RuleEval{retireEval(t)}
	provider := NewFakeProvider()
	opt := RunOptions{Owner: "acme", Repo: "rules", Base: "main", RepoRoot: "testdata", Apply: true}

	first, err := Run(context.Background(), provider, evals, opt)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if len(first.Opened) != 1 {
		t.Fatalf("first run opened %d PRs, want 1", len(first.Opened))
	}

	second, err := Run(context.Background(), provider, evals, opt)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(second.Opened) != 0 {
		t.Errorf("second run opened %d new PRs, want 0", len(second.Opened))
	}
	if len(second.Skipped) != 1 {
		t.Errorf("second run skipped %d, want 1 (the already-open PR)", len(second.Skipped))
	}
	if second.Skipped[0].PR.URL != first.Opened[0].PR.URL {
		t.Errorf("second run's skip points at %s, want the first run's PR %s",
			second.Skipped[0].PR.URL, first.Opened[0].PR.URL)
	}

	fp := provider
	if fp.nextNumber != 2 {
		t.Errorf("provider issued %d PR numbers, want exactly 1 to have been issued", fp.nextNumber-1)
	}
}

// TestRunDryRunOpensNothing is the dry-run-by-default guard: Apply: false
// must never call EnsureBranch/CommitFiles/OpenPR, only render.
func TestRunDryRunOpensNothing(t *testing.T) {
	evals := []scanner.RuleEval{retireEval(t), tuneEval(t)}
	provider := NewFakeProvider()

	res, err := Run(context.Background(), provider, evals, RunOptions{
		Owner: "acme", Repo: "rules", Base: "main", RepoRoot: "testdata", Apply: false,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Opened) != 0 {
		t.Errorf("dry run opened %d PRs, want 0", len(res.Opened))
	}
	if len(res.Proposals) != 2 {
		t.Errorf("dry run rendered %d proposals, want 2", len(res.Proposals))
	}
	for _, p := range res.Proposals {
		if p.Diff == "" || p.NewContent == "" {
			t.Errorf("%s: dry run must still render the diff and new content", p.AlertName)
		}
	}
	fp := provider
	if len(fp.Branches) != 0 || len(fp.Files) != 0 || len(fp.PRs) != 0 {
		t.Error("dry run touched the provider's branches/files/PRs")
	}
}

// TestRunSkipsRefusedRules confirms a refused rule contributes no
// proposal and is reported, not silently dropped.
func TestRunSkipsRefusedRules(t *testing.T) {
	ambiguous := retireEval(t)
	ambiguous.Ambiguous = true
	keep := retireEval(t)
	keep.Verdict = score.VerdictKeep

	provider := NewFakeProvider()
	res, err := Run(context.Background(), provider, []scanner.RuleEval{ambiguous, keep, tuneEval(t)},
		RunOptions{Owner: "acme", Repo: "rules", Base: "main", RepoRoot: "testdata", Apply: false})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Proposals) != 1 {
		t.Errorf("got %d proposals, want 1 (only the tune eval qualifies)", len(res.Proposals))
	}
	if len(res.Refusals) != 1 {
		t.Fatalf("got %d refusals, want 1 (the ambiguous rule)", len(res.Refusals))
	}
	if res.Refusals[0].Reason != ReasonAmbiguous {
		t.Errorf("refusal reason = %q, want ambiguous", res.Refusals[0].Reason)
	}
}
