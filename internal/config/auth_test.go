package config

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecret(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}
	return path
}

func TestAuthValidate(t *testing.T) {
	tokenFile := writeSecret(t, "tok-from-file")
	passFile := writeSecret(t, "pass-from-file")

	cases := []struct {
		name    string
		auth    Auth
		wantErr bool
	}{
		{"empty is valid", Auth{}, false},
		{"bearer token alone", Auth{BearerToken: "abc"}, false},
		{"bearer token file alone", Auth{BearerTokenFile: tokenFile}, false},
		{"basic auth alone", Auth{Username: "u", Password: "p"}, false},
		{"basic auth with password file", Auth{Username: "u", PasswordFile: passFile}, false},
		{"tls cert and key together", Auth{TLS: TLS{CertFile: "c", KeyFile: "k"}}, false},

		{"bearer token and file together", Auth{BearerToken: "abc", BearerTokenFile: tokenFile}, true},
		{"bearer and basic together", Auth{BearerToken: "abc", Username: "u", Password: "p"}, true},
		{"bearer file and basic together", Auth{BearerTokenFile: tokenFile, Username: "u", Password: "p"}, true},
		{"password and password file together", Auth{Username: "u", Password: "p", PasswordFile: passFile}, true},
		{"cert without key", Auth{TLS: TLS{CertFile: "c"}}, true},
		{"key without cert", Auth{TLS: TLS{KeyFile: "k"}}, true},
		{"missing bearer token file", Auth{BearerTokenFile: "/no/such/path"}, true},
		{"missing password file", Auth{Username: "u", PasswordFile: "/no/such/path"}, true},
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

func TestAuthValidateMissingBearerTokenFileNamesThePath(t *testing.T) {
	err := Auth{BearerTokenFile: "/no/such/path"}.Validate()
	if err == nil {
		t.Fatal("Validate succeeded, want error")
	}
	if !strings.Contains(err.Error(), "/no/such/path") {
		t.Errorf("error %q does not name the missing path", err.Error())
	}
}

func capturedAuthHeader(t *testing.T, auth Auth) string {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
	}))
	defer srv.Close()

	rt, err := auth.Transport()
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	client := &http.Client{Transport: rt}
	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return got
}

func TestTransportInjectsBearerToken(t *testing.T) {
	got := capturedAuthHeader(t, Auth{BearerToken: "s3cr3t"})
	if want := "Bearer s3cr3t"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

func TestTransportInjectsBearerTokenFromFile(t *testing.T) {
	path := writeSecret(t, "file-token\n")
	got := capturedAuthHeader(t, Auth{BearerTokenFile: path})
	if want := "Bearer file-token"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
}

func TestTransportInjectsBasicAuth(t *testing.T) {
	got := capturedAuthHeader(t, Auth{Username: "alice", Password: "hunter2"})
	req := &http.Request{Header: http.Header{"Authorization": []string{got}}}
	user, pass, ok := req.BasicAuth()
	if !ok {
		t.Fatalf("Authorization header %q is not basic auth", got)
	}
	if user != "alice" || pass != "hunter2" {
		t.Errorf("basic auth = %s:%s, want alice:hunter2", user, pass)
	}
}

func TestTransportNoAuthInjectsNoHeader(t *testing.T) {
	got := capturedAuthHeader(t, Auth{})
	if got != "" {
		t.Errorf("Authorization = %q, want empty for unauthenticated config", got)
	}
}

// TestTransportRotatesBearerTokenFile pins the file being re-read per
// request rather than cached at Transport() time -- otherwise a rotated
// token would not take effect until the process restarted.
func TestTransportRotatesBearerTokenFile(t *testing.T) {
	path := writeSecret(t, "first-token")
	auth := Auth{BearerTokenFile: path}

	var headers []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Get("Authorization"))
	}))
	defer srv.Close()

	rt, err := auth.Transport()
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	client := &http.Client{Transport: rt}

	mustGet := func() {
		t.Helper()
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	mustGet()
	if err := os.WriteFile(path, []byte("second-token"), 0o600); err != nil {
		t.Fatalf("rotate token file: %v", err)
	}
	mustGet()

	if len(headers) != 2 {
		t.Fatalf("got %d requests, want 2", len(headers))
	}
	if headers[0] != "Bearer first-token" {
		t.Errorf("first request Authorization = %q, want %q", headers[0], "Bearer first-token")
	}
	if headers[1] != "Bearer second-token" {
		t.Errorf("second request Authorization = %q, want %q; rotation not picked up", headers[1], "Bearer second-token")
	}
}

// TestFailedRequestErrorDoesNotLeakToken guards against a token or password
// ending up in an error string via URL or header echoing -- a real risk
// since both errors and URLs are frequently logged verbatim.
func TestFailedRequestErrorDoesNotLeakToken(t *testing.T) {
	const secret = "super-secret-token-value"
	auth := Auth{BearerToken: secret}

	rt, err := auth.Transport()
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	client := &http.Client{Transport: rt}

	// Nothing listens here: the request fails at the connection level, which
	// is exactly the kind of error most likely to get logged or wrapped
	// upstream without a second thought.
	_, getErr := client.Get("http://127.0.0.1:1/does-not-exist")
	if getErr == nil {
		t.Fatal("request to a closed port succeeded, want error")
	}
	if strings.Contains(getErr.Error(), secret) {
		t.Errorf("error leaks the bearer token: %v", getErr)
	}
}

func TestFailedAuthenticatedResponseErrorDoesNotLeakToken(t *testing.T) {
	const secret = "another-super-secret-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized: bad credentials", http.StatusUnauthorized)
	}))
	defer srv.Close()

	auth := Auth{BearerToken: secret}
	rt, err := auth.Transport()
	if err != nil {
		t.Fatalf("Transport: %v", err)
	}
	client := &http.Client{Transport: rt}

	resp, err := client.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if strings.Contains(string(body), secret) {
		t.Errorf("response body leaks the bearer token: %s", body)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}
