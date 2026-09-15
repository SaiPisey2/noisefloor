package config

import (
	"fmt"
	"os"
)

// Webhook configures `noisefloor collect` (issue #13), the write endpoint
// that ingests Alertmanager webhook notifications. scan, remediate,
// coverage and serve never read it.
type Webhook struct {
	Auth WebhookAuth `yaml:"auth"`

	// MaxBodyBytes bounds the size of one incoming request body. This is a
	// write endpoint that accepts unauthenticated POSTs by default (see
	// Auth), so an unbounded JSON decode is a memory-exhaustion primitive;
	// this is enforced with http.MaxBytesReader before the decoder ever
	// sees the body. A legitimate grouped notification -- even a large
	// one -- is nowhere near this size; Alertmanager's own group_by keeps
	// notifications to the alerts sharing a cause.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
}

// DefaultMaxBodyBytes is generous for any real Alertmanager notification
// (grouped notifications are typically dozens of alerts at most) while
// still bounding what an attacker reaching this endpoint can make the
// collector hold in memory for one request.
const DefaultMaxBodyBytes = 1 << 20 // 1 MiB

// WebhookAuth is the shared secret an Alertmanager webhook_configs entry
// must present, via a bearer token, to be accepted by `noisefloor collect`.
// Empty means no authentication is required at all -- acceptable only
// because the collector binds to loopback by default, exactly like
// `serve`; see cmd/noisefloor's collect command and its -allow-remote
// flag.
//
// Shaped like Auth's bearer fields on purpose, but kept separate: Auth
// configures credentials this binary SENDS to Prometheus/Alertmanager,
// while WebhookAuth configures a credential this binary VERIFIES on
// requests it RECEIVES. Reusing Auth (which also carries basic-auth and
// outbound TLS client settings that make no sense for a receiver) would
// blur that distinction for no real gain -- Alertmanager's own
// webhook_configs only ever sends a bearer token or a custom header.
type WebhookAuth struct {
	// BearerToken is an inline shared secret. Convenient, but it then lives
	// in the config file; prefer BearerTokenFile for anything real.
	BearerToken string `yaml:"bearer_token"`

	// BearerTokenFile names a file holding the shared secret. Re-read on
	// every request, so a rotated secret takes effect without a restart
	// and the config file itself carries no secret -- same contract as
	// Auth.BearerTokenFile.
	BearerTokenFile string `yaml:"bearer_token_file"`
}

func (w WebhookAuth) Configured() bool {
	return w.BearerToken != "" || w.BearerTokenFile != ""
}

// Validate rejects ambiguous configuration and confirms a referenced file
// exists, so a typo'd path fails when `noisefloor collect` starts, not on
// the first request that needed it.
func (w WebhookAuth) Validate() error {
	if w.BearerToken != "" && w.BearerTokenFile != "" {
		return fmt.Errorf("webhook.auth.bearer_token and bearer_token_file are mutually exclusive")
	}
	if w.BearerTokenFile != "" {
		if _, err := os.Stat(w.BearerTokenFile); err != nil {
			return fmt.Errorf("webhook.auth.bearer_token_file %q: %w", w.BearerTokenFile, err)
		}
	}
	return nil
}

// Token returns the currently expected bearer token. ok is false when no
// authentication is configured. BearerTokenFile is re-read on every call
// -- see its doc comment.
func (w WebhookAuth) Token() (token string, ok bool, err error) {
	switch {
	case w.BearerToken != "":
		return w.BearerToken, true, nil
	case w.BearerTokenFile != "":
		t, err := readSecretFile(w.BearerTokenFile)
		if err != nil {
			return "", false, fmt.Errorf("read webhook.auth.bearer_token_file: %w", err)
		}
		return t, true, nil
	default:
		return "", false, nil
	}
}
