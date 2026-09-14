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

// FileChange is one file's new full content to commit onto a branch,
// together with the content that new content was computed from.
type FileChange struct {
	Path    string
	Content string
	// BaseContent is the file exactly as the local checkout held it when
	// Content was derived from it. A provider MUST fetch the file it is
	// about to overwrite and refuse the commit unless it matches this.
	//
	// Content is a whole file, not a patch, so writing it blind replaces
	// whatever the remote currently holds. If the checkout is one commit
	// behind, or a human pushed to the bot's branch, every change this bot
	// did not make silently disappears -- reverted by a PR titled "retire
	// X", in someone else's repository, with no mention of it in the body
	// or the diff. There is no way for a reviewer to catch that; the only
	// place it can be caught is here, before the write.
	BaseContent string
	Message     string
}

// Provider is what a PR bot needs from a forge, kept narrow and
// forge-agnostic so GitHub (the only implementation today) is not baked
// into the rest of this package. A GitLab provider implements the same
// interface without any change to Proposal, Build or Runner.
type Provider interface {
	// FindPR returns the most relevant existing PR whose head is branch
	// against base, or nil if the branch has never had one. An open PR is
	// preferred; failing that, the most recently closed one is returned,
	// with State reporting which.
	//
	// Closed PRs are returned, not filtered out, because a closed PR is an
	// answer. Somebody looked at this exact proposal, on this exact branch,
	// and decided against it. Listing only open PRs makes that decision
	// invisible and reopens the same rejected PR on the next run, and the
	// run after that -- which is how a bot gets blocked at the org level.
	// Runner calls this before opening anything.
	FindPR(ctx context.Context, owner, repo, base, branch string) (*PullRequest, error)

	// EnsureBranch creates branch pointing at base's current HEAD if branch
	// does not already exist. Creating a branch that already exists
	// (pointing anywhere) is not an error -- Runner relies on that for its
	// own idempotence, and a branch a human has since pushed to must not be
	// silently reset.
	EnsureBranch(ctx context.Context, owner, repo, branch, base string) error

	// CommitFiles commits every change onto branch in one commit. Each
	// FileChange.Content is the file's complete new content, not a diff --
	// callers (Proposal.Render) already computed the minimal, surgical
	// result.
	//
	// Because it is a whole file, an implementation MUST NOT write it
	// blind. It has to read the file it is about to replace and refuse the
	// commit unless that content is byte-for-byte FileChange.BaseContent --
	// the content the edit was actually computed against. Anything else
	// means the remote has moved and this write would silently revert
	// somebody's work. See FileChange.BaseContent.
	CommitFiles(ctx context.Context, owner, repo, base, branch string, changes []FileChange) error

	// OpenPR opens a pull request from spec.Branch onto spec.Base.
	OpenPR(ctx context.Context, spec PRSpec) (*PullRequest, error)
}
