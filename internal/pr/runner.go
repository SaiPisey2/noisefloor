package pr

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/SaiPisey2/noisefloor/internal/scanner"
)

// Opened is one proposal that resulted in a newly-opened PR.
type Opened struct {
	Proposal *Proposal
	PR       *PullRequest
}

// Skipped is one proposal for which an open PR already existed on its
// branch -- the idempotence path: running the bot twice must not open a
// second PR for the same rule and the same proposal.
type Skipped struct {
	Proposal *Proposal
	PR       *PullRequest
}

// Declined is one proposal whose branch already carries a CLOSED (or
// merged) pull request.
//
// A closed PR is a person's answer to this exact proposal. Branch names are
// deterministic per proposal, so the PR that was closed proposed the same
// edit to the same rule; reopening it would be re-asking a question that
// has been answered, on a schedule, in a repository the bot is a guest in.
// The rule stays refused until the proposal itself changes -- which changes
// the branch name, and legitimately earns a new PR.
type Declined struct {
	Proposal *Proposal
	PR       *PullRequest
}

// RunResult is everything one Run produced, across every rule evaluated.
type RunResult struct {
	// Proposals is every rule that would get a PR, rendered (diff and new
	// file content computed) regardless of -apply. This is what a dry run
	// prints.
	Proposals []*Proposal
	// Opened is the subset of Proposals actually opened as a PR this run
	// (only populated when RunOptions.Apply is true).
	Opened []Opened
	// Skipped is the subset that already had an open PR on their branch.
	Skipped []Skipped
	// Declined is the subset whose branch already carries a closed or merged
	// PR -- a decision already taken, not repeated.
	Declined []Declined
	// Refusals is every rule Build declined to propose a change for, with
	// why.
	Refusals []Refusal
}

// RunOptions configures one Run.
type RunOptions struct {
	Owner, Repo, Base string
	// RepoRoot is the git checkout root proposals' file paths are inside
	// (config.Rules.Path) -- used to compute the repo-relative path
	// Provider.CommitFiles needs. Proposal.File itself stays a real
	// filesystem path (Edit.Apply reads it directly), so this is only
	// consulted when actually committing.
	RepoRoot string
	// Apply opts into actually opening PRs. False is the dry-run default:
	// every proposal is still built and rendered (so its diff and body can
	// be printed), but no branch, commit or PR is created.
	Apply bool
}

// Run builds a proposal for every rule in evals, in order, and -- when
// opt.Apply is set -- opens a PR for each one that does not already have an
// open PR on its branch. It never touches a rule Build refuses, and it
// never bulk-commits: each opened PR is exactly one branch, one commit, one
// file.
func Run(ctx context.Context, provider Provider, evals []scanner.RuleEval, opt RunOptions) (RunResult, error) {
	var res RunResult

	for _, eval := range evals {
		proposal, refusal, err := Build(eval)
		if err != nil {
			return res, fmt.Errorf("%s/%s: %w", eval.Rule.GroupName, eval.Rule.AlertName, err)
		}
		if refusal != nil {
			res.Refusals = append(res.Refusals, *refusal)
			continue
		}
		if proposal == nil {
			continue
		}

		if err := proposal.Render(); err != nil {
			return res, fmt.Errorf("%s/%s: render edit: %w", eval.Rule.GroupName, eval.Rule.AlertName, err)
		}
		res.Proposals = append(res.Proposals, proposal)

		if err := openProposal(ctx, provider, proposal, opt, &res); err != nil {
			return res, err
		}
	}

	return res, nil
}

// openProposal is the forge-facing half of Run, factored out so
// RunStarters (coverage blind-spot proposals) can reuse it exactly rather
// than re-implementing "check for an existing PR, then branch/commit/open
// if -apply" a second time. It appends to res (Skipped/Declined/Opened) and
// returns only on a real error -- a proposal that is skipped or declined is
// not an error, so callers must not treat a nil return as "opened".
func openProposal(ctx context.Context, provider Provider, proposal *Proposal, opt RunOptions, res *RunResult) error {
	existing, err := provider.FindPR(ctx, opt.Owner, opt.Repo, opt.Base, proposal.Branch)
	if err != nil {
		return fmt.Errorf("%s/%s: check existing PR: %w", proposal.Group, proposal.AlertName, err)
	}
	switch {
	case existing == nil:
		// No PR has ever been opened for this proposal. Carry on.
	case existing.State == "open":
		res.Skipped = append(res.Skipped, Skipped{Proposal: proposal, PR: existing})
		return nil
	default:
		// Closed or merged: somebody already decided. See Declined.
		res.Declined = append(res.Declined, Declined{Proposal: proposal, PR: existing})
		return nil
	}
	if !opt.Apply {
		return nil
	}

	relPath := proposal.File
	if opt.RepoRoot != "" {
		if rel, err := filepath.Rel(opt.RepoRoot, proposal.File); err == nil {
			relPath = rel
		}
	}

	if err := provider.EnsureBranch(ctx, opt.Owner, opt.Repo, proposal.Branch, opt.Base); err != nil {
		return fmt.Errorf("%s/%s: ensure branch: %w", proposal.Group, proposal.AlertName, err)
	}
	if err := provider.CommitFiles(ctx, opt.Owner, opt.Repo, opt.Base, proposal.Branch, []FileChange{
		{
			Path: relPath, Content: proposal.NewContent,
			BaseContent: proposal.BaseContent, Message: proposal.Title,
		},
	}); err != nil {
		return fmt.Errorf("%s/%s: commit: %w", proposal.Group, proposal.AlertName, err)
	}
	opened, err := provider.OpenPR(ctx, PRSpec{
		Owner: opt.Owner, Repo: opt.Repo, Base: opt.Base, Branch: proposal.Branch,
		Title: proposal.Title, Body: proposal.Body,
	})
	if err != nil {
		return fmt.Errorf("%s/%s: open PR: %w", proposal.Group, proposal.AlertName, err)
	}
	res.Opened = append(res.Opened, Opened{Proposal: proposal, PR: opened})
	return nil
}
