package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPagerDutyConfiguredRequiresServiceIDs(t *testing.T) {
	if (PagerDuty{}).Configured() {
		t.Error("empty PagerDuty must not be Configured")
	}
	if !(PagerDuty{ServiceIDs: []string{"P1"}}).Configured() {
		t.Error("PagerDuty with service_ids must be Configured")
	}
}

func TestPagerDutyAuthValidateRejectsBothTokenForms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := PagerDutyAuth{APIToken: "inline", APITokenFile: path}
	if err := a.Validate(); err == nil {
		t.Error("want an error when both api_token and api_token_file are set")
	}
}

func TestPagerDutyAuthValidateMissingFileNamesThePath(t *testing.T) {
	a := PagerDutyAuth{APITokenFile: "/no/such/path"}
	err := a.Validate()
	if err == nil {
		t.Fatal("want an error for a missing api_token_file")
	}
	if want := "/no/such/path"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the missing path", err.Error())
	}
}

func TestPagerDutyAuthTokenRotatesFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := PagerDutyAuth{APITokenFile: path}

	tok, ok, err := a.Token()
	if err != nil || !ok || tok != "first" {
		t.Fatalf("Token() = %q, %v, %v; want \"first\", true, nil", tok, ok, err)
	}

	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, ok, err = a.Token()
	if err != nil || !ok || tok != "second" {
		t.Fatalf("Token() after rotation = %q, %v, %v; want \"second\", true, nil", tok, ok, err)
	}
}

func TestPagerDutyAuthTokenAbsentWhenUnconfigured(t *testing.T) {
	_, ok, err := (PagerDutyAuth{}).Token()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("Token() ok = true for a PagerDutyAuth with neither field set")
	}
}

// TestConfigValidateRequiresTokenWhenServiceIDsSet pins the "fail at
// startup, not partway through a scan" contract every other endpoint's
// auth already has: a typo'd or missing token must be caught by
// config.Load, not discovered when the enricher's first request 401s.
func TestConfigValidateRequiresTokenWhenServiceIDsSet(t *testing.T) {
	path := writeTemp(t, "prometheus:\n  url: http://localhost:9090\n"+
		"pagerduty:\n  service_ids: [P1]\n")
	if _, err := Load(path); err == nil {
		t.Fatal("want an error: pagerduty.service_ids set with no api_token or api_token_file")
	}
}

func TestConfigValidateAcceptsConfiguredPagerDuty(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("tok"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeTemp(t, "prometheus:\n  url: http://localhost:9090\n"+
		"pagerduty:\n  service_ids: [P1]\n  auth:\n    api_token_file: "+tokenPath+"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.PagerDuty.Configured() {
		t.Error("Configured() = false, want true")
	}
	if cfg.PagerDuty.MatchWindow.Std() <= 0 {
		t.Error("match_window must default to a positive duration")
	}
}
