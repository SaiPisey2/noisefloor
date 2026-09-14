package pr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// GitHubProvider implements Provider against the GitHub REST API (v3),
// using only net/http and encoding/json -- no SDK dependency, so this is
// the entire surface a future GitLabProvider needs to match.
type GitHubProvider struct {
	// BaseURL defaults to https://api.github.com; overridable for GitHub
	// Enterprise and for tests (an httptest.Server).
	BaseURL string
	// Token authenticates every request. GitHub's REST API accepts a
	// classic or fine-grained personal access token, or an installation
	// token, identically via this header.
	Token  string
	Client *http.Client
}

func NewGitHubProvider(token string) *GitHubProvider {
	return &GitHubProvider{Token: token, Client: &http.Client{Timeout: 30 * time.Second}}
}

func (g *GitHubProvider) baseURL() string {
	if g.BaseURL != "" {
		return strings.TrimSuffix(g.BaseURL, "/")
	}
	return "https://api.github.com"
}

func (g *GitHubProvider) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (g *GitHubProvider) do(ctx context.Context, method, path string, body any, out any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.baseURL()+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.Token != "" {
		req.Header.Set("Authorization", "Bearer "+g.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp, fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if resp.StatusCode >= 300 {
		return resp, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp, fmt.Errorf("%s %s: decode response: %w", method, path, err)
		}
	}
	return resp, nil
}

type ghPull struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

func (g *GitHubProvider) FindOpenPR(ctx context.Context, owner, repo, base, branch string) (*PullRequest, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=open&base=%s&head=%s:%s",
		owner, repo, base, owner, branch)
	var pulls []ghPull
	if _, err := g.do(ctx, http.MethodGet, path, nil, &pulls); err != nil {
		return nil, fmt.Errorf("list open PRs for branch %s: %w", branch, err)
	}
	if len(pulls) == 0 {
		return nil, nil
	}
	p := pulls[0]
	return &PullRequest{Number: p.Number, URL: p.HTMLURL, Branch: p.Head.Ref, State: p.State}, nil
}

type ghRef struct {
	Object struct {
		SHA string `json:"sha"`
	} `json:"object"`
}

func (g *GitHubProvider) EnsureBranch(ctx context.Context, owner, repo, branch, base string) error {
	_, err := g.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, repo, branch), nil, nil)
	if err == nil {
		// Already exists. Never reset it: a human may have pushed to it.
		return nil
	}
	if !strings.Contains(err.Error(), "404") {
		return fmt.Errorf("check branch %s: %w", branch, err)
	}

	var baseRef ghRef
	if _, err := g.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, repo, base), nil, &baseRef); err != nil {
		return fmt.Errorf("resolve base branch %s: %w", base, err)
	}

	_, err = g.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo), map[string]string{
		"ref": "refs/heads/" + branch,
		"sha": baseRef.Object.SHA,
	}, nil)
	if err != nil && !strings.Contains(err.Error(), "422") {
		// 422 here means the ref was created concurrently between our GET
		// and this POST -- not our error to report.
		return fmt.Errorf("create branch %s: %w", branch, err)
	}
	return nil
}

type ghContent struct {
	SHA string `json:"sha"`
}

func (g *GitHubProvider) CommitFiles(ctx context.Context, owner, repo, branch string, changes []FileChange) error {
	for _, c := range changes {
		path := fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", owner, repo, c.Path, branch)
		var existing ghContent
		sha := ""
		if _, err := g.do(ctx, http.MethodGet, path, nil, &existing); err == nil {
			sha = existing.SHA
		}
		// A GET error (including 404, a new file) is not fatal here: sha
		// simply stays empty, which the Contents API treats as "create".

		body := map[string]any{
			"message": c.Message,
			"content": base64.StdEncoding.EncodeToString([]byte(c.Content)),
			"branch":  branch,
		}
		if sha != "" {
			body["sha"] = sha
		}
		if _, err := g.do(ctx, http.MethodPut,
			fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repo, c.Path), body, nil); err != nil {
			return fmt.Errorf("commit %s to %s: %w", c.Path, branch, err)
		}
	}
	return nil
}

func (g *GitHubProvider) OpenPR(ctx context.Context, spec PRSpec) (*PullRequest, error) {
	var p ghPull
	_, err := g.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", spec.Owner, spec.Repo), map[string]string{
		"title": spec.Title,
		"head":  spec.Branch,
		"base":  spec.Base,
		"body":  spec.Body,
	}, &p)
	if err != nil {
		return nil, fmt.Errorf("open PR for branch %s: %w", spec.Branch, err)
	}
	return &PullRequest{Number: p.Number, URL: p.HTMLURL, Branch: spec.Branch, State: p.State}, nil
}

var _ Provider = (*GitHubProvider)(nil)
