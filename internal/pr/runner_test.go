package pr

import (
	"context"
	"os"
	"strings"
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

// TestRunRefusesToOverwriteAMovedBaseFile is the "a PR can revert unrelated
// work" defect. CommitFiles PUTs a whole file. If the remote holds anything
// other than what the local checkout held when the edit was computed, that
// PUT silently reverts every change in between -- under a PR titled
// "noisefloor: retire RetireMe", in a repository the bot does not own.
func TestRunRefusesToOverwriteAMovedBaseFile(t *testing.T) {
	provider := NewFakeProvider()
	// The base branch's copy of the file has a line the local checkout has
	// never seen: somebody committed to it after this checkout was taken.
	local, err := os.ReadFile("testdata/fixture.yml")
	if err != nil {
		t.Fatal(err)
	}
	provider.BaseFiles[key("acme", "rules", "fixture.yml")] =
		string(local) + "        annotations:\n          owner: someone-else\n"

	_, err = Run(context.Background(), provider, []scanner.RuleEval{retireEval(t)}, RunOptions{
		Owner: "acme", Repo: "rules", Base: "main", RepoRoot: "testdata", Apply: true,
	})
	if err == nil {
		t.Fatal("Run overwrote a file that had moved on since the checkout was taken")
	}
	if !strings.Contains(err.Error(), "refusing to commit") {
		t.Errorf("error does not explain the refusal: %v", err)
	}
	if len(provider.Files) != 0 {
		t.Errorf("a refused commit still wrote %d file(s)", len(provider.Files))
	}
}

// TestRunCommitsWhenTheRemoteMatchesTheCheckout is the other half: the
// base-content check must not block the ordinary case, where the remote
// holds exactly what was edited.
func TestRunCommitsWhenTheRemoteMatchesTheCheckout(t *testing.T) {
	provider := NewFakeProvider()
	local, err := os.ReadFile("testdata/fixture.yml")
	if err != nil {
		t.Fatal(err)
	}
	provider.BaseFiles[key("acme", "rules", "fixture.yml")] = string(local)

	res, err := Run(context.Background(), provider, []scanner.RuleEval{retireEval(t)}, RunOptions{
		Owner: "acme", Repo: "rules", Base: "main", RepoRoot: "testdata", Apply: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Opened) != 1 {
		t.Fatalf("opened %d PRs, want 1", len(res.Opened))
	}
}

// TestRunDoesNotReopenAClosedPR: a closed PR on this branch is a person's
// answer to this exact proposal. Reopening it every run is how a bot gets
// blocked at the org level.
func TestRunDoesNotReopenAClosedPR(t *testing.T) {
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

	// A maintainer reads it and closes it.
	branch := first.Opened[0].Proposal.Branch
	provider.PRs[key("acme", "rules", branch)].State = "closed"

	second, err := Run(context.Background(), provider, evals, opt)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(second.Opened) != 0 {
		t.Errorf("second run reopened %d PRs a maintainer had closed, want 0", len(second.Opened))
	}
	if len(second.Skipped) != 0 {
		t.Errorf("a closed PR is not an 'already open' skip, got %d", len(second.Skipped))
	}
	if len(second.Declined) != 1 {
		t.Fatalf("second run declined %d, want 1 (the closed PR)", len(second.Declined))
	}
	if second.Declined[0].PR.Number != first.Opened[0].PR.Number {
		t.Errorf("declined PR #%d, want the closed one #%d",
			second.Declined[0].PR.Number, first.Opened[0].PR.Number)
	}
	if provider.nextNumber != 2 {
		t.Errorf("provider issued %d PR numbers, want exactly 1", provider.nextNumber-1)
	}

	// And it is said out loud, not silently dropped.
	var out strings.Builder
	WriteResult(&out, second, true)
	if !strings.Contains(out.String(), "will not reopen") {
		t.Errorf("the declined proposal is not explained in the output:\n%s", out.String())
	}
}

// TestRunDoesNotReopenAMergedPR: merged is equally an answer, and the edit
// it carried is already in the base branch.
func TestRunDoesNotReopenAMergedPR(t *testing.T) {
	evals := []scanner.RuleEval{retireEval(t)}
	provider := NewFakeProvider()
	opt := RunOptions{Owner: "acme", Repo: "rules", Base: "main", RepoRoot: "testdata", Apply: true}

	first, err := Run(context.Background(), provider, evals, opt)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	provider.PRs[key("acme", "rules", first.Opened[0].Proposal.Branch)].State = "merged"

	second, err := Run(context.Background(), provider, evals, opt)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(second.Opened) != 0 || len(second.Declined) != 1 {
		t.Errorf("opened=%d declined=%d, want 0 and 1", len(second.Opened), len(second.Declined))
	}
}
