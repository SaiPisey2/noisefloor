package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// Auth carries credentials for one endpoint (Prometheus or Alertmanager).
// The two are configured independently because they frequently sit behind
// different gateways with different auth requirements.
type Auth struct {
	// BearerToken is an inline bearer token. Convenient, but the token then
	// lives in the config file; prefer BearerTokenFile for anything real.
	BearerToken string `yaml:"bearer_token"`

	// BearerTokenFile names a file holding a bearer token. It is read on
	// every request rather than once at startup, so a rotated token is
	// picked up without a restart, and the config file itself carries no
	// secret.
	BearerTokenFile string `yaml:"bearer_token_file"`

	// Username/Password (or PasswordFile) configure HTTP basic auth.
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	PasswordFile string `yaml:"password_file"`

	TLS TLS `yaml:"tls"`
}

// TLS configures certificate verification and mutual TLS for one endpoint.
type TLS struct {
	CAFile   string `yaml:"ca_file"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`

	// InsecureSkipVerify disables TLS certificate verification entirely.
	// This defeats the protection TLS gives against a machine-in-the-middle
	// reading or altering traffic, and must only be used against a host you
	// control -- for example a self-signed instance on your own machine,
	// never a shared or production endpoint.
	InsecureSkipVerify bool `yaml:"insecure_skip_verify"`
}

func (a Auth) hasBearer() bool {
	return a.BearerToken != "" || a.BearerTokenFile != ""
}

func (a Auth) hasBasic() bool {
	return a.Username != "" || a.Password != "" || a.PasswordFile != ""
}

// Validate rejects ambiguous or incomplete auth configuration, and confirms
// any referenced file exists. Checking file existence here, rather than at
// first request, means a typo'd path fails when "noisefloor scan" starts,
// not partway through a run that may already have done useful work.
func (a Auth) Validate() error {
	if a.BearerToken != "" && a.BearerTokenFile != "" {
		return fmt.Errorf("bearer_token and bearer_token_file are mutually exclusive")
	}
	if a.hasBearer() && a.hasBasic() {
		return fmt.Errorf("bearer token auth and basic auth are mutually exclusive")
	}
	if a.Password != "" && a.PasswordFile != "" {
		return fmt.Errorf("password and password_file are mutually exclusive")
	}
	if (a.TLS.CertFile != "") != (a.TLS.KeyFile != "") {
		return fmt.Errorf("tls.cert_file and tls.key_file must be set together")
	}
	if a.BearerTokenFile != "" {
		if _, err := os.Stat(a.BearerTokenFile); err != nil {
			return fmt.Errorf("bearer_token_file %q: %w", a.BearerTokenFile, err)
		}
	}
	if a.PasswordFile != "" {
		if _, err := os.Stat(a.PasswordFile); err != nil {
			return fmt.Errorf("password_file %q: %w", a.PasswordFile, err)
		}
	}
	return nil
}

// Transport builds the http.RoundTripper an endpoint's client should use: an
// auth layer that injects the configured credential on every request,
// wrapping a Transport carrying the configured TLS settings. It is safe to
// call on a zero-valued Auth -- the auth layer then injects nothing and the
// TLS settings fall back to Go's defaults, matching noisefloor's prior
// unauthenticated behavior exactly.
func (a Auth) Transport() (http.RoundTripper, error) {
	tlsConfig, err := a.TLS.build()
	if err != nil {
		return nil, err
	}

	base := http.DefaultTransport
	if tlsConfig != nil {
		t := base.(*http.Transport).Clone()
		t.TLSClientConfig = tlsConfig
		base = t
	}

	return authRoundTripper{auth: a, next: base}, nil
}

func (t TLS) build() (*tls.Config, error) {
	if t == (TLS{}) {
		return nil, nil
	}
	cfg := &tls.Config{InsecureSkipVerify: t.InsecureSkipVerify}

	if t.CAFile != "" {
		pem, err := os.ReadFile(t.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read tls.ca_file %q: %w", t.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls.ca_file %q: no certificates found", t.CAFile)
		}
		cfg.RootCAs = pool
	}

	if t.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load tls client cert/key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}

	return cfg, nil
}

// authRoundTripper injects the configured credential into every outgoing
// request. The bearer/password file variants are re-read per request, on
// purpose: it is what makes a rotated secret take effect without a restart.
type authRoundTripper struct {
	auth Auth
	next http.RoundTripper
}

func (rt authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if err := rt.auth.setCredential(req); err != nil {
		return nil, err
	}
	return rt.next.RoundTrip(req)
}

func (a Auth) setCredential(req *http.Request) error {
	switch {
	case a.BearerToken != "":
		req.Header.Set("Authorization", "Bearer "+a.BearerToken)

	case a.BearerTokenFile != "":
		token, err := readSecretFile(a.BearerTokenFile)
		if err != nil {
			return fmt.Errorf("read bearer token file: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)

	case a.hasBasic():
		pass := a.Password
		if a.PasswordFile != "" {
			p, err := readSecretFile(a.PasswordFile)
			if err != nil {
				return fmt.Errorf("read password file: %w", err)
			}
			pass = p
		}
		req.SetBasicAuth(a.Username, pass)
	}
	return nil
}

func readSecretFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
