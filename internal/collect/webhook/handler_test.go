package webhook

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/server"
)

func TestDefaultAddrIsLoopback(t *testing.T) {
	if !server.IsLoopback(DefaultAddr) {
		t.Fatalf("DefaultAddr %q is not loopback -- this is a write endpoint with no "+
			"authentication by default and must not default to a wider bind", DefaultAddr)
	}
}

func postFixture(t *testing.T, h http.Handler, fixture string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := loadFixture(t, fixture)
	req := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandlerAcceptsGroupedMultiAlert(t *testing.T) {
	fs := newFakeStore()
	srv := NewServer(fs, config.WebhookAuth{}, config.DefaultMaxBodyBytes)
	rec := postFixture(t, srv.Handler(), "grouped_multialert.json", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(fs.upserts) != 2 {
		t.Errorf("got %d upserts, want 2", len(fs.upserts))
	}
}

func TestHandlerAcceptsResolvedNotification(t *testing.T) {
	fs := newFakeStore()
	srv := NewServer(fs, config.WebhookAuth{}, config.DefaultMaxBodyBytes)
	rec := postFixture(t, srv.Handler(), "resolved.json", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerRejectsMalformedPayload(t *testing.T) {
	fs := newFakeStore()
	srv := NewServer(fs, config.WebhookAuth{}, config.DefaultMaxBodyBytes)

	rec := postFixture(t, srv.Handler(), "malformed_missing_alertname.json", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing alertname: status = %d, want 400", rec.Code)
	}
	if len(fs.upserts) != 0 {
		t.Errorf("malformed payload must not write any episode, got %d upserts", len(fs.upserts))
	}

	rec = postFixture(t, srv.Handler(), "malformed_truncated.json", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("truncated JSON: status = %d, want 400", rec.Code)
	}
}

func TestHandlerRejectsWrongMethod(t *testing.T) {
	fs := newFakeStore()
	srv := NewServer(fs, config.WebhookAuth{}, config.DefaultMaxBodyBytes)
	req := httptest.NewRequest(http.MethodGet, Path, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET %s: status = %d, want 405", Path, rec.Code)
	}
}

func TestHandlerHealthz(t *testing.T) {
	fs := newFakeStore()
	srv := NewServer(fs, config.WebhookAuth{}, config.DefaultMaxBodyBytes)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestHandlerAuthenticationAcceptedWithCorrectToken(t *testing.T) {
	fs := newFakeStore()
	auth := config.WebhookAuth{BearerToken: "s3cr3t"}
	srv := NewServer(fs, auth, config.DefaultMaxBodyBytes)
	rec := postFixture(t, srv.Handler(), "grouped_multialert.json", map[string]string{
		"Authorization": "Bearer s3cr3t",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

func TestHandlerAuthenticationRejectedWithoutToken(t *testing.T) {
	fs := newFakeStore()
	auth := config.WebhookAuth{BearerToken: "s3cr3t"}
	srv := NewServer(fs, auth, config.DefaultMaxBodyBytes)
	rec := postFixture(t, srv.Handler(), "grouped_multialert.json", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(fs.upserts) != 0 {
		t.Errorf("unauthenticated request must not write any episode")
	}
}

func TestHandlerAuthenticationRejectedWithWrongToken(t *testing.T) {
	fs := newFakeStore()
	auth := config.WebhookAuth{BearerToken: "s3cr3t"}
	srv := NewServer(fs, auth, config.DefaultMaxBodyBytes)
	rec := postFixture(t, srv.Handler(), "grouped_multialert.json", map[string]string{
		"Authorization": "Bearer wrong-token",
	})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestHandlerNoAuthConfiguredAcceptsAnyRequest(t *testing.T) {
	fs := newFakeStore()
	srv := NewServer(fs, config.WebhookAuth{}, config.DefaultMaxBodyBytes)
	rec := postFixture(t, srv.Handler(), "grouped_multialert.json", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when no auth is configured", rec.Code)
	}
}

func TestHandlerRejectsOversizedBody(t *testing.T) {
	fs := newFakeStore()
	const maxBody = 128 // far smaller than the real fixture
	srv := NewServer(fs, config.WebhookAuth{}, maxBody)

	body := loadFixture(t, "grouped_multialert.json")
	if len(body) <= maxBody {
		t.Fatalf("fixture (%d bytes) must be larger than maxBody (%d) for this test to mean anything", len(body), maxBody)
	}
	req := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
	if len(fs.upserts) != 0 {
		t.Errorf("oversized request must not write any episode")
	}
}

func TestHandlerRejectsOversizedBodyEvenWhenContentLengthLies(t *testing.T) {
	// A chunked or otherwise streamed body carries no trustworthy
	// Content-Length; the bound must be enforced as bytes are read, not by
	// trusting a header an attacker controls.
	fs := newFakeStore()
	const maxBody = 128
	srv := NewServer(fs, config.WebhookAuth{}, maxBody)

	body := loadFixture(t, "grouped_multialert.json")
	req := httptest.NewRequest(http.MethodPost, Path, bytes.NewReader(body))
	req.ContentLength = -1 // unknown length, as a streamed request would report
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestHandlerSecurityHeaders(t *testing.T) {
	fs := newFakeStore()
	srv := NewServer(fs, config.WebhookAuth{}, config.DefaultMaxBodyBytes)
	rec := postFixture(t, srv.Handler(), "grouped_multialert.json", nil)
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}

func TestHandlerBearerTokenFileIsReReadPerRequest(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/token"
	if err := os.WriteFile(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
	fs := newFakeStore()
	auth := config.WebhookAuth{BearerTokenFile: path}
	srv := NewServer(fs, auth, config.DefaultMaxBodyBytes)

	rec := postFixture(t, srv.Handler(), "grouped_multialert.json", map[string]string{"Authorization": "Bearer first"})
	if rec.Code != http.StatusOK {
		t.Fatalf("first token: status = %d, want 200", rec.Code)
	}

	if err := os.WriteFile(path, []byte("second"), 0o600); err != nil {
		t.Fatalf("rotate token file: %v", err)
	}
	rec = postFixture(t, srv.Handler(), "grouped_multialert.json", map[string]string{"Authorization": "Bearer first"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("stale token after rotation: status = %d, want 401", rec.Code)
	}
	rec = postFixture(t, srv.Handler(), "grouped_multialert.json", map[string]string{"Authorization": "Bearer second"})
	if rec.Code != http.StatusOK {
		t.Fatalf("rotated token: status = %d, want 200", rec.Code)
	}
}
