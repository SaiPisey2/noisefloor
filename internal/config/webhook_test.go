package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultMaxBodyBytesIsPositive(t *testing.T) {
	if got := Default().Webhook.MaxBodyBytes; got != DefaultMaxBodyBytes {
		t.Errorf("Webhook.MaxBodyBytes = %d, want default %d", got, DefaultMaxBodyBytes)
	}
}

func TestLoadOverridesWebhookMaxBodyBytes(t *testing.T) {
	path := writeTemp(t, "prometheus:\n  url: http://prom:9090\nwebhook:\n  max_body_bytes: 4096\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Webhook.MaxBodyBytes != 4096 {
		t.Errorf("Webhook.MaxBodyBytes = %d, want 4096", cfg.Webhook.MaxBodyBytes)
	}
}

func TestLoadRejectsNonPositiveMaxBodyBytes(t *testing.T) {
	path := writeTemp(t, "prometheus:\n  url: http://prom:9090\nwebhook:\n  max_body_bytes: 0\n")
	if _, err := Load(path); err == nil {
		t.Fatal("Load succeeded with webhook.max_body_bytes: 0, want error")
	}
}

func TestWebhookAuthValidate(t *testing.T) {
	tokenFile := writeSecret(t, "tok")

	cases := []struct {
		name    string
		auth    WebhookAuth
		wantErr bool
	}{
		{"empty is valid", WebhookAuth{}, false},
		{"bearer token alone", WebhookAuth{BearerToken: "abc"}, false},
		{"bearer token file alone", WebhookAuth{BearerTokenFile: tokenFile}, false},
		{"both set is ambiguous", WebhookAuth{BearerToken: "abc", BearerTokenFile: tokenFile}, true},
		{"missing bearer token file", WebhookAuth{BearerTokenFile: "/no/such/path"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.auth.Validate()
			if c.wantErr && err == nil {
				t.Fatal("Validate succeeded, want error")
			}
			if !c.wantErr && err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestWebhookAuthConfigured(t *testing.T) {
	if (WebhookAuth{}).Configured() {
		t.Error("zero-value WebhookAuth reports Configured() = true, want false")
	}
	if !(WebhookAuth{BearerToken: "x"}).Configured() {
		t.Error("WebhookAuth with a bearer token reports Configured() = false, want true")
	}
}

func TestWebhookAuthTokenFromInline(t *testing.T) {
	tok, ok, err := WebhookAuth{BearerToken: "abc"}.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if !ok || tok != "abc" {
		t.Errorf("Token() = %q, %v; want abc, true", tok, ok)
	}
}

func TestWebhookAuthTokenNotConfigured(t *testing.T) {
	_, ok, err := WebhookAuth{}.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if ok {
		t.Error("Token() ok = true for an unconfigured WebhookAuth, want false")
	}
}

func TestWebhookAuthTokenFileIsReReadPerCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	auth := WebhookAuth{BearerTokenFile: path}

	tok, ok, err := auth.Token()
	if err != nil || !ok || tok != "first" {
		t.Fatalf("Token() = %q, %v, %v; want first, true, nil", tok, ok, err)
	}

	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("rotate token file: %v", err)
	}
	tok, ok, err = auth.Token()
	if err != nil || !ok || tok != "second" {
		t.Fatalf("Token() after rotation = %q, %v, %v; want second, true, nil", tok, ok, err)
	}
}
