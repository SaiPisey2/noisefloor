package config

import (
	"fmt"
	"os"
	"time"
)

// PagerDuty configures the optional pager enricher (issue #14): given a
// rule and a window, it fetches PagerDuty's own record of what actually
// happened to that rule's pages -- acknowledged, escalated, resolved by a
// human or automatically -- which noisefloor treats as MEASURED evidence,
// distinct from every signal derived by inference from firing duration.
//
// Entirely optional and off by default. Leaving this unset (ServiceIDs
// empty) changes nothing about how noisefloor scores or scans: scan,
// remediate and everything downstream of them run exactly as they did
// before this feature -- see internal/score's PagerOutcomes for exactly
// how (and how conservatively) measured evidence is allowed to move a
// verdict once it IS configured.
type PagerDuty struct {
	Auth PagerDutyAuth `yaml:"auth"`

	// ServiceIDs are the PagerDuty service IDs to pull incidents from.
	// Required to enable the enricher at all -- an empty list means "no
	// pager enrichment configured", the same posture as an unset
	// alertmanager.url.
	ServiceIDs []string `yaml:"service_ids"`

	// BaseURL overrides the API base, for testing against a fixture
	// server. Defaults to https://api.pagerduty.com.
	BaseURL string `yaml:"base_url"`

	// MatchWindow bounds how far from an episode's own [started_at,
	// ended_at) span a PagerDuty incident's created_at may fall and still
	// be considered a match for it. See
	// internal/enrich/pagerduty's matching doc comment for why this is a
	// best-effort correlation (alert name plus time proximity, not a
	// guaranteed dedup-key match) and why that is stated as such
	// everywhere the result surfaces.
	MatchWindow Duration `yaml:"match_window"`
}

// PagerDutyAuth carries the REST API token noisefloor authenticates to
// PagerDuty with. Shaped like Auth's bearer-token fields on purpose (same
// inline-vs-file trade-off, same re-read-per-request rotation story) but
// kept as its own type: PagerDuty's REST API v2 authenticates with
// `Authorization: Token token=<key>`, not a bearer token, so reusing Auth
// verbatim would produce a header PagerDuty rejects.
type PagerDutyAuth struct {
	// APIToken is an inline REST API token. Convenient, but the token then
	// lives in the config file; prefer APITokenFile for anything real.
	APIToken string `yaml:"api_token"`

	// APITokenFile names a file holding the token. Read on every request
	// rather than once at startup, so a rotated token takes effect without
	// a restart -- same contract as Auth.BearerTokenFile.
	APITokenFile string `yaml:"api_token_file"`
}

// Configured reports whether the operator has turned this enricher on at
// all. internal/scanner checks this, not Auth alone, because ServiceIDs is
// what actually scopes which incidents get fetched -- a token with no
// service to query is not a usable configuration.
func (p PagerDuty) Configured() bool {
	return len(p.ServiceIDs) > 0
}

func (a PagerDutyAuth) hasToken() bool {
	return a.APIToken != "" || a.APITokenFile != ""
}

// Validate rejects ambiguous configuration and confirms a referenced file
// exists, so a typo'd path fails when a scan starts, not on the first
// request that needed it -- the same posture as Auth.Validate.
func (a PagerDutyAuth) Validate() error {
	if a.APIToken != "" && a.APITokenFile != "" {
		return fmt.Errorf("pagerduty.auth.api_token and api_token_file are mutually exclusive")
	}
	if a.APITokenFile != "" {
		if _, err := os.Stat(a.APITokenFile); err != nil {
			return fmt.Errorf("pagerduty.auth.api_token_file %q: %w", a.APITokenFile, err)
		}
	}
	return nil
}

// Token returns the currently configured API token. ok is false when
// neither field is set. APITokenFile is re-read on every call -- see its
// doc comment -- and the token itself is never logged or included in any
// error this package returns; see internal/enrich/pagerduty's client tests
// for the guard on that.
func (a PagerDutyAuth) Token() (token string, ok bool, err error) {
	switch {
	case a.APIToken != "":
		return a.APIToken, true, nil
	case a.APITokenFile != "":
		t, err := readSecretFile(a.APITokenFile)
		if err != nil {
			return "", false, fmt.Errorf("read pagerduty.auth.api_token_file: %w", err)
		}
		return t, true, nil
	default:
		return "", false, nil
	}
}

func defaultPagerDutyMatchWindow() Duration { return Duration(5 * time.Minute) }
