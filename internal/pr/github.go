package pr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponseBytes bounds every response body this provider reads. GitHub's
// Contents API already refuses to return base64 content for a file over
// 1 MB, and every other response read here -- a page of pull requests, a
// single git ref -- is a small, bounded JSON object even at GitHub's own
// page-size ceiling. The limit exists so a misbehaving or compromised
// endpoint (a wrong BaseURL, a MITM) produces a clear error instead of
// either exhausting memory or, worse, handing json.Unmarshal a silently
// truncated body -- see the lr.N == 0 check in do(), which catches
// truncation before any parsing is attempted.
const maxResponseBytes = 10 << 20 // 10 MiB

// safeURL renders a URL for an error message without anything that could
// be a credential: no userinfo (url.URL.Host never includes it) and no
// query string, which is where this provider's own list-PR parameters sit
// today and where a token would sit if it were ever passed as a query
// parameter instead of a header. Do not "simplify" this back to
// u.String() -- that would put the full request, including any secret,
// into stderr, logs and CI output.
func safeURL(u *url.URL) string {
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// apiError is a non-2xx response from the API, carrying the status code as
// a number.
//
// The code has to be a field, not something a caller digs back out of the
// message. Every "was this a 404?" decision in this file used to be
// strings.Contains(err.Error(), "404") over the formatted message -- and
// that message embeds both the request path and the response body, so any
// of these made an unrelated failure look like a missing file:
//
//   - a repository, branch or file path whose NAME contains "404"
//     (github.com/acme/status-404-page is an ordinary repository), which
//     made EVERY error on that repository read as a 404;
//   - a 5xx from a proxy whose body quotes an upstream 404;
//   - any error body that happens to contain those three digits.
//
// That is not cosmetic. getContent maps "404" to found=false, and
// CommitFiles reads found=false on the BASE ref as "the file does not
// exist on base at all", which is the one branch that skips the
// base-moved-on check entirely -- so a 502 with the wrong three digits in
// it silently disabled the guard that exists to stop this tool reverting
// somebody's work. Compare codes, never message text.
type apiError struct {
	Method     string
	Path       string
	StatusCode int
	Status     string
	Body       string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s: %s: %s", e.Method, e.Path, e.Status, e.Body)
}

// statusCode returns the HTTP status carried by err, or 0 if err is not an
// API status error (a network failure, a read error, a decode error).
func statusCode(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.StatusCode
	}
	return 0
}

// pathSegment escapes one path segment of a REST URL and rejects the
// segments that would change which resource the request addresses rather
// than name one.
//
// These values -- owner, repo, branch, and the file path from the config's
// prometheus_file -- were interpolated raw into a format string. Three
// things followed, all demonstrable:
//
//   - a "?" anywhere in them started the query string early, so a path of
//     "conf/a?ref=attacker&x.yml" made getContent read the file at
//     attacker's ref while believing it had read the caller's. That is the
//     exact blob CommitFiles compares against BaseContent before deciding
//     the write is safe.
//   - ".." segments traversed: repo "../../user" addressed /user.
//   - a "#" truncated the rest of the URL into a fragment the client never
//     sends, so the request silently addressed something other than what
//     was asked for.
//
// Escaping alone does not fix "..", because dots are unreserved and
// survive escaping, so dot segments are rejected outright.
func pathSegment(kind, s string) (string, error) {
	if s == "" {
		return "", fmt.Errorf("%s is empty", kind)
	}
	if s == "." || s == ".." {
		return "", fmt.Errorf("%s %q is a path traversal, not a name", kind, s)
	}
	return url.PathEscape(s), nil
}

// filePath escapes a repository-relative file path segment by segment,
// keeping "/" as the separator (url.PathEscape would otherwise encode it)
// and rejecting anything that is not a plain relative path.
func filePath(p string) (string, error) {
	if p == "" {
		return "", errors.New("file path is empty")
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("file path %q must be relative to the repository root", p)
	}
	parts := strings.Split(p, "/")
	for i, seg := range parts {
		esc, err := pathSegment("file path segment", seg)
		if err != nil {
			return "", fmt.Errorf("file path %q: %w", p, err)
		}
		parts[i] = esc
	}
	return strings.Join(parts, "/"), nil
}

// repoPath returns "/repos/<owner>/<repo>" with both segments escaped and
// validated, the prefix every call in this file builds on.
func repoPath(owner, repo string) (string, error) {
	o, err := pathSegment("owner", owner)
	if err != nil {
		return "", err
	}
	r, err := pathSegment("repo", repo)
	if err != nil {
		return "", err
	}
	return "/repos/" + o + "/" + r, nil
}

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
		// http.Client.Do wraps failures in a *url.Error whose Error()
		// embeds the complete request URL, query string included. This
		// provider sends its token only via the Authorization header, but
		// the query string already carries owner/repo/branch names and is
		// exactly where a token would sit if that ever changed -- report
		// the underlying cause plus a scheme/host/path-only location
		// instead of the wrapper's own message.
		var uerr *url.Error
		cause := error(err)
		if errors.As(err, &uerr) {
			cause = uerr.Err
		}
		return nil, fmt.Errorf("%s %s: %w", method, safeURL(req.URL), cause)
	}
	defer resp.Body.Close()

	// Bound the read: see maxResponseBytes. Reading one byte past the
	// limit, rather than exactly at it, lets us tell "body was exactly the
	// limit" apart from "body was longer and got cut off" -- the latter
	// must be a clear error, never a silently truncated body handed to
	// json.Unmarshal.
	lr := &io.LimitedReader{R: resp.Body, N: maxResponseBytes + 1}
	data, err := io.ReadAll(lr)
	if err != nil {
		return resp, fmt.Errorf("%s %s: read response: %w", method, path, err)
	}
	if lr.N == 0 {
		return resp, fmt.Errorf("%s %s: response body exceeds %d byte limit", method, path, maxResponseBytes)
	}
	if resp.StatusCode >= 300 {
		// A reverse proxy, WAF or misconfigured GitHub Enterprise gateway
		// can reflect the incoming Authorization header back into an error
		// body. This error is printed to stderr, and in CI stderr gets
		// archived and pasted into tickets -- so redact the token before
		// it ever reaches the error.
		body := strings.TrimSpace(string(data))
		if g.Token != "" {
			body = strings.ReplaceAll(body, g.Token, "[REDACTED]")
		}
		return resp, &apiError{Method: method, Path: path, StatusCode: resp.StatusCode, Status: resp.Status, Body: body}
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
	rp, err := repoPath(owner, repo)
	if err != nil {
		return nil, err
	}
	q := url.Values{
		"state":     []string{"all"},
		"base":      []string{base},
		"head":      []string{owner + ":" + branch},
		"sort":      []string{"created"},
		"direction": []string{"desc"},
	}
	path := rp + "/pulls?" + q.Encode()
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
	rp, err := repoPath(owner, repo)
	if err != nil {
		return err
	}
	br, err := pathSegment("branch", branch)
	if err != nil {
		return err
	}
	bs, err := pathSegment("base branch", base)
	if err != nil {
		return err
	}

	_, err = g.do(ctx, http.MethodGet, rp+"/git/ref/heads/"+br, nil, nil)
	if err == nil {
		// Already exists. Never reset it: a human may have pushed to it.
		return nil
	}
	// Only an actual 404 means "this branch does not exist yet". Anything
	// else -- a 401, a rate limit, a proxy 502 -- must not be read as
	// permission to create it. See apiError.
	if statusCode(err) != http.StatusNotFound {
		return fmt.Errorf("check branch %s: %w", branch, err)
	}

	var baseRef ghRef
	if _, err := g.do(ctx, http.MethodGet, rp+"/git/ref/heads/"+bs, nil, &baseRef); err != nil {
		return fmt.Errorf("resolve base branch %s: %w", base, err)
	}

	_, err = g.do(ctx, http.MethodPost, rp+"/git/refs", map[string]string{
		"ref": "refs/heads/" + branch,
		"sha": baseRef.Object.SHA,
	}, nil)
	if err != nil && statusCode(err) != http.StatusUnprocessableEntity {
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
	rp, err := repoPath(owner, repo)
	if err != nil {
		return ghContent{}, false, err
	}
	fp, err := filePath(path)
	if err != nil {
		return ghContent{}, false, err
	}
	q := url.Values{"ref": []string{ref}}
	_, err = g.do(ctx, http.MethodGet, rp+"/contents/"+fp+"?"+q.Encode(), nil, &c)
	if err != nil {
		// Only an actual 404 is "the file is not at this ref". Every other
		// failure is propagated: reporting one as found=false hands
		// CommitFiles the one answer that skips its base-moved-on check.
		// See apiError.
		if statusCode(err) == http.StatusNotFound {
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
		//
		// This must fail closed: a guard that silently skips itself on a
		// transient error is the exact defect it exists to prevent, and it
		// is most likely to be skipped in precisely the moment it matters
		// -- an API hiccup on the one run where base really has moved on.
		// So an error fetching or decoding the base blob is propagated, not
		// swallowed.
		onBase, baseFound, err := g.getContent(ctx, owner, repo, c.Path, base)
		if err != nil {
			return fmt.Errorf("read %s on %s: %w", c.Path, base, err)
		}
		if baseFound {
			baseContent, derr := onBase.decoded()
			if derr != nil {
				// An encoding this tool cannot read is not evidence that
				// base agrees with the checkout -- refuse rather than
				// assume.
				return fmt.Errorf("read %s on %s: %w", c.Path, base, derr)
			}
			if baseContent != c.BaseContent {
				return fmt.Errorf("refusing to commit %s to %s: %s has moved on since this "+
					"checkout (%d bytes on %s, %d bytes locally), so the proposed content would "+
					"revert changes made there. Re-run against a fresh checkout",
					c.Path, branch, base, len(baseContent), base, len(c.BaseContent))
			}
		}
		// else: the file does not exist on base at all. That is a
		// legitimate case -- this proposal may be adding the rule file for
		// the first time -- and not, on its own, evidence that base has
		// moved on out from under this checkout. There is nothing to
		// compare it against, so no refusal here; the branch-blob check
		// above already covers the file that does exist.

		putRepo, err := repoPath(owner, repo)
		if err != nil {
			return err
		}
		putFile, err := filePath(c.Path)
		if err != nil {
			return err
		}
		body := map[string]any{
			"message": c.Message,
			"content": base64.StdEncoding.EncodeToString([]byte(c.Content)),
			"branch":  branch,
			"sha":     onBranch.SHA,
		}
		if _, err := g.do(ctx, http.MethodPut,
			putRepo+"/contents/"+putFile, body, nil); err != nil {
			return fmt.Errorf("commit %s to %s: %w", c.Path, branch, err)
		}
	}
	return nil
}

func (g *GitHubProvider) OpenPR(ctx context.Context, spec PRSpec) (*PullRequest, error) {
	var p ghPull
	rp, err := repoPath(spec.Owner, spec.Repo)
	if err != nil {
		return nil, err
	}
	_, err = g.do(ctx, http.MethodPost, rp+"/pulls", map[string]string{
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
