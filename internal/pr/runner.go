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

		existing, err := provider.FindOpenPR(ctx, opt.Owner, opt.Repo, opt.Base, proposal.Branch)
		if err != nil {
			return res, fmt.Errorf("%s/%s: check existing PR: %w", eval.Rule.GroupName, eval.Rule.AlertName, err)
		}
		if existing != nil {
			res.Skipped = append(res.Skipped, Skipped{Proposal: proposal, PR: existing})
			continue
		}
		if !opt.Apply {
			continue
		}

		relPath := proposal.File
		if opt.RepoRoot != "" {
			if rel, err := filepath.Rel(opt.RepoRoot, proposal.File); err == nil {
				relPath = rel
			}
		}

		if err := provider.EnsureBranch(ctx, opt.Owner, opt.Repo, proposal.Branch, opt.Base); err != nil {
			return res, fmt.Errorf("%s/%s: ensure branch: %w", eval.Rule.GroupName, eval.Rule.AlertName, err)
		}
		if err := provider.CommitFiles(ctx, opt.Owner, opt.Repo, proposal.Branch, []FileChange{
			{Path: relPath, Content: proposal.NewContent, Message: proposal.Title},
		}); err != nil {
			return res, fmt.Errorf("%s/%s: commit: %w", eval.Rule.GroupName, eval.Rule.AlertName, err)
		}
		opened, err := provider.OpenPR(ctx, PRSpec{
			Owner: opt.Owner, Repo: opt.Repo, Base: opt.Base, Branch: proposal.Branch,
			Title: proposal.Title, Body: proposal.Body,
		})
		if err != nil {
			return res, fmt.Errorf("%s/%s: open PR: %w", eval.Rule.GroupName, eval.Rule.AlertName, err)
		}
		res.Opened = append(res.Opened, Opened{Proposal: proposal, PR: opened})
	}

	return res, nil
}
