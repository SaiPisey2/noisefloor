package pr

import "context"

// PullRequest is the subset of an opened (or already-open) pull request
// this package needs: enough to report it and to check idempotence.
type PullRequest struct {
	Number int
	URL    string
	Branch string
	State  string // "open", "closed", "merged" -- provider-reported, not interpreted here
}

// PRSpec is everything Provider.OpenPR needs to open one PR. Base is the
// branch to merge into (e.g. "main"); Owner/Repo identify the repository.
type PRSpec struct {
	Owner, Repo string
	Base        string
	Branch      string
	Title       string
	Body        string
}

// FileChange is one file's new full content to commit onto a branch.
type FileChange struct {
	Path    string
	Content string
	Message string
}

// Provider is what a PR bot needs from a forge, kept narrow and
// forge-agnostic so GitHub (the only implementation today) is not baked
// into the rest of this package. A GitLab provider implements the same
// interface without any change to Proposal, Build or Runner.
type Provider interface {
	// FindOpenPR returns the currently open PR whose head is branch against
	// base, or nil if there is none. This is the idempotence check: Runner
	// calls it before opening anything.
	FindOpenPR(ctx context.Context, owner, repo, base, branch string) (*PullRequest, error)

	// EnsureBranch creates branch pointing at base's current HEAD if branch
	// does not already exist. Creating a branch that already exists
	// (pointing anywhere) is not an error -- Runner relies on that for its
	// own idempotence, and a branch a human has since pushed to must not be
	// silently reset.
	EnsureBranch(ctx context.Context, owner, repo, branch, base string) error

	// CommitFiles commits every change onto branch in one commit. Each
	// FileChange.Content is the file's complete new content, not a diff --
	// callers (Proposal.Render) already computed the minimal, surgical
	// result; the provider's job is only to put those bytes on the branch.
	CommitFiles(ctx context.Context, owner, repo, branch string, changes []FileChange) error

	// OpenPR opens a pull request from spec.Branch onto spec.Base.
	OpenPR(ctx context.Context, spec PRSpec) (*PullRequest, error)
}
