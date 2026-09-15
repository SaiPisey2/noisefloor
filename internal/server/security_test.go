package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestSecurityHeadersOnHTML(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)
	rec := get(t, srv.Handler(), "/")
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got == "" {
		t.Errorf("Content-Security-Policy header is missing")
	} else if !strings.Contains(got, "default-src 'none'") {
		t.Errorf("Content-Security-Policy = %q, want a restrictive default-src", got)
	}
}

func TestSecurityHeadersOnJSON(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)
	rec := get(t, srv.Handler(), "/api/rules")
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := rec.Header().Get("Content-Security-Policy"); got == "" {
		t.Errorf("Content-Security-Policy header is missing on a JSON response")
	}
}

func TestStaticDirectoryListingDisabled(t *testing.T) {
	db := newTestDB(t)
	srv := newTestServer(t, db)
	rec := get(t, srv.Handler(), "/static/")
	if rec.Code == http.StatusOK {
		t.Fatalf("GET /static/ returned 200 (a directory listing); want it refused, got body:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "filter.js") || strings.Contains(rec.Body.String(), "style.css") {
		t.Errorf("response looks like a directory listing:\n%s", rec.Body.String())
	}
	// The actual assets must still be reachable directly by name -- only
	// the listing is disabled.
	rec = get(t, srv.Handler(), "/static/style.css")
	if rec.Code != http.StatusOK {
		t.Errorf("GET /static/style.css status = %d, want 200", rec.Code)
	}
}
