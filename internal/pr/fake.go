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

func (f *FakeProvider) FindOpenPR(_ context.Context, owner, repo, _, branch string) (*PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr, ok := f.PRs[key(owner, repo, branch)]
	if !ok || pr.State != "open" {
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

func (f *FakeProvider) CommitFiles(_ context.Context, owner, repo, branch string, changes []FileChange) error {
	if f.FailCommit != nil {
		return f.FailCommit
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Branches[key(owner, repo, branch)]; !ok {
		return fmt.Errorf("commit to nonexistent branch %s", branch)
	}
	for _, c := range changes {
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
