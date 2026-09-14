package pr

import (
	"context"
	"fmt"
	"sync"
)

// FakeProvider is an in-memory Provider for tests: no network call ever
// leaves the process. It is exported (not a _test.go type) so both this
// package's tests and cmd/noisefloor's can use one without duplicating it.
type FakeProvider struct {
	mu sync.Mutex

	// Branches maps "owner/repo/branch" to the base it was created from.
	Branches map[string]string
	// Files maps "owner/repo/branch/path" to its committed content.
	Files map[string]string
	// BaseFiles maps "owner/repo/path" to the file's content on the base
	// branch. A branch with no committed file of its own inherits this, the
	// same way a branch cut from base HEAD does. Set it to something other
	// than what the local checkout holds to exercise CommitFiles' refusal to
	// overwrite a remote that has moved on.
	BaseFiles map[string]string
	// PRs maps "owner/repo/branch" to the PR opened for it, open or not.
	PRs map[string]*PullRequest

	nextNumber int

	// FailEnsureBranch, FailCommit, FailOpenPR, when non-nil, are returned
	// by the matching method instead of succeeding -- for exercising
	// Runner's error handling without a real network failure.
	FailEnsureBranch error
	FailCommit       error
	FailOpenPR       error
}

func NewFakeProvider() *FakeProvider {
	return &FakeProvider{
		Branches:   map[string]string{},
		Files:      map[string]string{},
		BaseFiles:  map[string]string{},
		PRs:        map[string]*PullRequest{},
		nextNumber: 1,
	}
}

func key(parts ...string) string {
	s := parts[0]
	for _, p := range parts[1:] {
		s += "/" + p
	}
	return s
}

func (f *FakeProvider) FindPR(_ context.Context, owner, repo, _, branch string) (*PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.PRs[key(owner, repo, branch)]
	if !ok {
		return nil, nil
	}
	cp := *pr
	return &cp, nil
}

func (f *FakeProvider) EnsureBranch(_ context.Context, owner, repo, branch, base string) error {
	if f.FailEnsureBranch != nil {
		return f.FailEnsureBranch
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(owner, repo, branch)
	if _, exists := f.Branches[k]; exists {
		return nil
	}
	f.Branches[k] = base
	return nil
}

// CommitFiles enforces the same base-content precondition GitHubProvider
// does, because a fake that accepts a write a real provider would refuse
// makes every test built on it prove the wrong thing.
//
// The file it compares against is whatever the branch already holds, or --
// for a branch that has never been committed to, exactly like one just cut
// from base HEAD -- BaseFiles. A test that leaves BaseFiles empty is
// describing a remote whose file matches the local checkout, which is the
// ordinary case.
func (f *FakeProvider) CommitFiles(_ context.Context, owner, repo, base, branch string, changes []FileChange) error {
	if f.FailCommit != nil {
		return f.FailCommit
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Branches[key(owner, repo, branch)]; !ok {
		return fmt.Errorf("commit to nonexistent branch %s", branch)
	}
	for _, c := range changes {
		remote, committed := f.Files[key(owner, repo, branch, c.Path)]
		if !committed {
			remote, committed = f.BaseFiles[key(owner, repo, c.Path)]
		}
		if committed && remote != c.BaseContent {
			return fmt.Errorf("refusing to commit %s to %s: the file there is not the one this "+
				"edit was computed from (%d bytes remote, %d bytes locally)",
				c.Path, branch, len(remote), len(c.BaseContent))
		}
		if baseContent, ok := f.BaseFiles[key(owner, repo, c.Path)]; ok && baseContent != c.BaseContent {
			return fmt.Errorf("refusing to commit %s to %s: %s has moved on since this checkout",
				c.Path, branch, base)
		}
		f.Files[key(owner, repo, branch, c.Path)] = c.Content
	}
	return nil
}

func (f *FakeProvider) OpenPR(_ context.Context, spec PRSpec) (*PullRequest, error) {
	if f.FailOpenPR != nil {
		return nil, f.FailOpenPR
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(spec.Owner, spec.Repo, spec.Branch)
	if existing, ok := f.PRs[k]; ok && existing.State == "open" {
		return existing, nil
	}
	pr := &PullRequest{
		Number: f.nextNumber,
		URL:    fmt.Sprintf("https://fake.example/%s/%s/pull/%d", spec.Owner, spec.Repo, f.nextNumber),
		Branch: spec.Branch,
		State:  "open",
	}
	f.nextNumber++
	f.PRs[k] = pr
	return pr, nil
}

var _ Provider = (*FakeProvider)(nil)
