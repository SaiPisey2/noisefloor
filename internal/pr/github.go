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
	Number   int     `json:"number"`
	HTMLURL  string  `json:"html_url"`
	State    string  `json:"state"`
	MergedAt *string `json:"merged_at"`
	Head     struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

// FindPR lists this branch's PRs in EVERY state, not just open ones.
//
// Filtering on state=open was a silent "reopen anything a maintainer
// closed": the branch name is deterministic per proposal, so a PR a
// maintainer read and rejected looked, on the next run, exactly like a
// branch that had never had a PR at all. The bot would open it again. And
// again. In someone else's repository, on a schedule.
//
// An open PR wins when there is one (that is the ordinary idempotence
// case). Otherwise the most recent closed PR is returned so Runner can
// treat it as the decision it is. GitHub reports a merged PR as closed with
// merged_at set; State distinguishes them, and neither is a reason to open
// a second one.
func (g *GitHubProvider) FindPR(ctx context.Context, owner, repo, base, branch string) (*PullRequest, error) {
	path := fmt.Sprintf("/repos/%s/%s/pulls?state=all&base=%s&head=%s:%s&sort=created&direction=desc",
		owner, repo, base, owner, branch)
	var pulls []ghPull
	if _, err := g.do(ctx, http.MethodGet, path, nil, &pulls); err != nil {
		return nil, fmt.Errorf("list PRs for branch %s: %w", branch, err)
	}
	if len(pulls) == 0 {
		return nil, nil
	}

	best := pulls[0] // most recently created, whatever its state
	for _, p := range pulls {
		if p.State == "open" {
			best = p
			break
		}
	}

	state := best.State
	if best.MergedAt != nil && *best.MergedAt != "" {
		state = "merged"
	}
	return &PullRequest{Number: best.Number, URL: best.HTMLURL, Branch: best.Head.Ref, State: state}, nil
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
	SHA      string `json:"sha"`
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// decoded returns the file's bytes as GitHub sent them. The Contents API
// base64-encodes with embedded newlines, which base64.StdEncoding rejects.
func (c ghContent) decoded() (string, error) {
	if c.Encoding != "" && c.Encoding != "base64" {
		return "", fmt.Errorf("unsupported content encoding %q", c.Encoding)
	}
	raw := strings.NewReplacer("\n", "", "\r", "").Replace(c.Content)
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return "", fmt.Errorf("decode content: %w", err)
	}
	return string(data), nil
}

// getContent fetches one file at one ref. found is false for a 404 (the
// file does not exist there), which is not an error on its own.
func (g *GitHubProvider) getContent(ctx context.Context, owner, repo, path, ref string) (c ghContent, found bool, err error) {
	_, err = g.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", owner, repo, path, ref), nil, &c)
	if err != nil {
		if strings.Contains(err.Error(), "404") {
			return ghContent{}, false, nil
		}
		return ghContent{}, false, err
	}
	return c, true, nil
}

// CommitFiles writes each change onto branch, but only after proving the
// bytes it is about to replace are the bytes the change was computed from.
//
// The Contents API PUTs a whole file. That makes every commit a potential
// silent revert of everything the local checkout has not seen: a base that
// moved on after EnsureBranch read it, or a human who pushed a fix onto the
// bot's own branch. Both cases produce a clean-looking PR titled "retire X"
// that also quietly undoes unrelated work, and neither the diff this tool
// prints nor the PR body mentions it, because neither knows.
//
// So both refs are checked, and each answers a different question:
//
//   - branch: what this PUT will actually overwrite. If a human pushed to
//     the bot's branch, this is where it shows up.
//   - base: what the proposal was supposed to be built on. A branch created
//     fresh from base HEAD matches base by definition, so a mismatch here
//     means the checkout is behind (or ahead of) what is deployed.
//
// A mismatch is refused rather than resolved. Merging someone else's change
// into a file this tool only understands one rule of is not something it
// can do safely, and overwriting is exactly the defect. The fix is to
// re-run against a fresh checkout, which is a thing an operator can do in
// one command.
func (g *GitHubProvider) CommitFiles(ctx context.Context, owner, repo, base, branch string, changes []FileChange) error {
	for _, c := range changes {
		onBranch, branchFound, err := g.getContent(ctx, owner, repo, c.Path, branch)
		if err != nil {
			return fmt.Errorf("read %s on %s: %w", c.Path, branch, err)
		}
		if !branchFound {
			return fmt.Errorf("refusing to commit %s to %s: the file does not exist there, "+
				"but this edit was computed against %d bytes of it",
				c.Path, branch, len(c.BaseContent))
		}
		current, err := onBranch.decoded()
		if err != nil {
			return fmt.Errorf("read %s on %s: %w", c.Path, branch, err)
		}
		if current != c.BaseContent {
			return fmt.Errorf("refusing to commit %s to %s: the file there is not the one this "+
				"edit was computed from (%d bytes on the branch, %d bytes locally). Committing "+
				"would overwrite changes this run never saw. Re-run against a fresh checkout",
				c.Path, branch, len(current), len(c.BaseContent))
		}

		// The branch may predate the current base. Check base too, so a
		// checkout that has fallen behind what is deployed is caught even
		// when the branch still agrees with it.
		onBase, baseFound, err := g.getContent(ctx, owner, repo, c.Path, base)
		if err == nil && baseFound {
			if baseContent, derr := onBase.decoded(); derr == nil && baseContent != c.BaseContent {
				return fmt.Errorf("refusing to commit %s to %s: %s has moved on since this "+
					"checkout (%d bytes on %s, %d bytes locally), so the proposed content would "+
					"revert changes made there. Re-run against a fresh checkout",
					c.Path, branch, base, len(baseContent), base, len(c.BaseContent))
			}
		}

		body := map[string]any{
			"message": c.Message,
			"content": base64.StdEncoding.EncodeToString([]byte(c.Content)),
			"branch":  branch,
			"sha":     onBranch.SHA,
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
