// Package pagerduty is noisefloor's reference enricher implementation
// (issue #14): it talks to PagerDuty's documented REST API v2
// (https://developer.pagerduty.com/api-reference) to fetch what actually
// happened to a rule's pages -- acknowledged, escalated, resolved by a
// human or automatically -- and turns that into enrich.Result for
// internal/score to weigh as measured evidence.
//
// This package has been exercised only against fixture responses shaped
// like PagerDuty's documented schema (see testdata/), never against a live
// PagerDuty account. It is built from PagerDuty's own published API
// reference and its official go-pagerduty client's field names, but "built
// against documented behavior" and "exercised against the real service"
// are different claims; see the README's Pager enrichers section for the
// honest version of that distinction.
package pagerduty

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/SaiPisey2/noisefloor/internal/config"
)

// DefaultBaseURL is PagerDuty's production REST API v2 endpoint.
const DefaultBaseURL = "https://api.pagerduty.com"

// maxPages hard-bounds how many pages ANY single list call will ever
// fetch, regardless of what the API's `more` flag says -- so a rule (or an
// account) with an enormous incident volume degrades to a partial,
// clearly-flagged fetch rather than issuing an unbounded number of
// requests. 500 pages at pageLimit 100 is 50,000 records, far beyond what
// any 30-day scan window should plausibly hold.
//
// A var, not a const, so client_test.go can shrink it and exercise the cap
// without a 500-page fixture round trip.
var maxPages = 500

const (
	// pageLimit is the page size for every paginated list call. 100 is
	// PagerDuty's documented maximum for list endpoints.
	pageLimit = 100

	// maxRetries bounds how many times ONE request is retried after a
	// retryable failure (a 429 or a 5xx) before the whole fetch gives up
	// and this package's caller (internal/enrich/pagerduty's Enricher, and
	// above it internal/scanner) degrades the scan to estimated scoring
	// for this rule rather than hanging indefinitely.
	maxRetries = 5

	// defaultBackoff is the base for exponential backoff when PagerDuty's
	// response carries no rate-limit-reset header to honor directly --
	// attempt N (1-indexed) waits defaultBackoff * 2^(N-1): 1s, 2s, 4s,
	// 8s, 16s.
	defaultBackoff = time.Second

	// maxBackoff caps any single wait, whether taken from a response
	// header or computed by exponential backoff -- a misbehaving or
	// malicious response naming an hours-long reset must not be allowed to
	// stall a scan that has its own overall timeout.
	maxBackoff = 30 * time.Second
)

// Client is a minimal PagerDuty REST API v2 client: only the two list
// endpoints (incidents, log entries) this enricher needs, not a general
// PagerDuty SDK.
type Client struct {
	baseURL string
	auth    config.PagerDutyAuth
	hc      *http.Client
}

// NewClient builds a Client. hc may be nil, in which case a client with a
// 30s timeout is used -- the same default every other outbound client in
// this codebase uses (see internal/collect's Alertmanager client).
func NewClient(cfg config.PagerDuty, hc *http.Client) *Client {
	base := cfg.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{baseURL: strings.TrimSuffix(base, "/"), auth: cfg.Auth, hc: hc}
}

// apiError is returned for a non-retryable (or retry-exhausted) HTTP
// response. It deliberately carries only the status code and a bounded,
// truncated snippet of the body -- never any request header -- so it can
// never echo the Authorization header back into a log line.
type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("pagerduty: unexpected status %d: %s", e.status, e.body)
}

// do issues one GET request against path with the given query, retrying a
// 429 or 5xx with backoff, and decodes a successful JSON response into out.
//
// The token is set as a header on every attempt (config.PagerDutyAuth.Token
// re-reads a token file per call, matching this codebase's existing
// rotation contract) and is never placed in the URL, so nothing in Go's own
// error formatting -- which includes the request URL, never its headers --
// can leak it. See client_test.go's TestErrorsNeverContainTheToken.
func (c *Client) do(ctx context.Context, path string, query url.Values, out interface{}) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var lastErr error
	var nextDelay time.Duration
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(nextDelay):
			}
		}

		resp, body, err := c.attempt(ctx, u)
		if err != nil {
			return err // build/transport failure: never retryable here, see attempt's doc comment
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("pagerduty: decode response from %s: %w", path, err)
			}
			return nil

		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = &apiError{status: resp.StatusCode, body: truncate(body, 256)}
			nextDelay = retryAfter(resp.Header, attempt)
			continue

		default:
			return &apiError{status: resp.StatusCode, body: truncate(body, 256)}
		}
	}
	return fmt.Errorf("pagerduty: giving up after %d attempts against %s: %w", maxRetries, path, lastErr)
}

// attempt performs exactly one HTTP round trip. A non-nil error here means
// the request never got a response at all (DNS, dial, TLS, or a canceled
// context) -- not retried by this function; do's caller decides whether to
// try again for a completed-but-unhappy response, but a transport failure
// on the first attempt is surfaced immediately rather than silently eating
// budget from the retry loop on failures that a 429/5xx backoff strategy
// isn't aimed at (see IsRetryable-style reasoning in
// internal/collect/prom/retry.go, which this mirrors for a different
// transport).
func (c *Client) attempt(ctx context.Context, u string) (*http.Response, []byte, error) {
	token, ok, err := c.auth.Token()
	if err != nil {
		return nil, nil, fmt.Errorf("pagerduty: %w", err)
	}
	if !ok {
		return nil, nil, fmt.Errorf("pagerduty: no api token configured")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("pagerduty: build request: %w", err)
	}
	// PagerDuty's REST API v2 token scheme, not Bearer -- see
	// config.PagerDutyAuth's doc comment for why this can't reuse
	// config.Auth's transport.
	req.Header.Set("Authorization", "Token token="+token)
	req.Header.Set("Accept", "application/vnd.pagerduty+json;version=2")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("pagerduty: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("pagerduty: read response: %w", err)
	}
	return resp, body, nil
}

// retryAfter computes how long to wait before the next attempt, preferring
// PagerDuty's own documented rate-limit headers over a blind exponential
// backoff. PagerDuty (see
// https://support.pagerduty.com/main/docs/rest-api-rate-limits) documents
// `RateLimit-Reset` (seconds until the limit resets) rather than the
// generic `Retry-After` some other APIs use; both are checked, in that
// order, since a 429 from an intermediary proxy in front of PagerDuty could
// plausibly carry the latter instead.
func retryAfter(h http.Header, attempt int) time.Duration {
	for _, name := range []string{"RateLimit-Reset", "Retry-After"} {
		if v := h.Get(name); v != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
				return clampBackoff(time.Duration(secs) * time.Second)
			}
		}
	}
	return clampBackoff(defaultBackoff * time.Duration(1<<uint(attempt)))
}

func clampBackoff(d time.Duration) time.Duration {
	if d > maxBackoff {
		return maxBackoff
	}
	if d < 0 {
		return 0
	}
	return d
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
