package webhook

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/SaiPisey2/noisefloor/internal/config"
)

// DefaultAddr is where `noisefloor collect` listens unless told otherwise.
// Loopback only, same posture and for the same reason as server.DefaultAddr:
// this is a write endpoint with no authentication unless the operator
// configures one, so it must not default to reachable-from-anywhere. A
// different port from server.DefaultAddr (9091) and the demo stack's own
// ports, so `noisefloor serve` and `noisefloor collect` can run side by
// side against the same database.
const DefaultAddr = "127.0.0.1:9094"

// Path is where the collector expects Alertmanager's webhook_configs POST.
const Path = "/webhook"

// Server is the webhook HTTP endpoint.
type Server struct {
	collector *Collector
	auth      config.WebhookAuth
	maxBody   int64
}

// NewServer builds a Server. maxBody bounds the request body via
// http.MaxBytesReader before the JSON decoder ever sees it -- see
// config.Webhook.MaxBodyBytes' doc comment for why that bound exists at
// all. auth is checked on every request; a zero-value WebhookAuth accepts
// every request unauthenticated, which is only safe bound to loopback.
func NewServer(db Store, auth config.WebhookAuth, maxBody int64) *Server {
	return &Server{collector: New(db), auth: auth, maxBody: maxBody}
}

// Handler builds the route table: POST Path receives notifications, GET
// /healthz answers liveness the same way `noisefloor serve` does.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+Path, s.handleWebhook)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	return securityHeaders(mux)
}

// securityHeaders mirrors internal/server's: this response is never
// rendered by a browser as HTML, but the header costs nothing and closes
// off content-sniffing regardless of who ends up curling this endpoint.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.authenticate(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.maxBody)

	var p Payload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		// http.MaxBytesReader's error, once the limit is hit, unwraps to a
		// *http.MaxBytesError with no fixed sentinel to match portably
		// across Go versions; a substring check on the standard library's
		// own message is the documented way to detect it.
		if strings.Contains(err.Error(), "http: request body too large") {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, fmt.Sprintf("malformed payload: %v", err), http.StatusBadRequest)
		return
	}

	if err := p.Validate(); err != nil {
		http.Error(w, fmt.Sprintf("invalid payload: %v", err), http.StatusBadRequest)
		return
	}

	res, err := s.collector.Ingest(r.Context(), p)
	if err != nil {
		// Never echo the internal error to the client: it can carry a
		// store-layer detail with no business leaving this process, and
		// this endpoint has no operator watching a terminal the way a CLI
		// command does.
		log.Printf("noisefloor collect: ingest failed: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

// authenticate reports whether r carries the configured shared secret. It
// always succeeds when no secret is configured -- the operator's own
// choice, made explicit by the loopback-vs-allow-remote decision in
// cmd/noisefloor, not a default this package silently downgrades to.
func (s *Server) authenticate(r *http.Request) bool {
	want, ok, err := s.auth.Token()
	if err != nil {
		log.Printf("noisefloor collect: read shared secret: %v", err)
		return false
	}
	if !ok {
		return true
	}
	got := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(got, prefix) {
		return false
	}
	got = strings.TrimPrefix(got, prefix)
	// Constant-time comparison: this endpoint's whole job is to check a
	// secret against attacker-reachable input, so a timing side-channel on
	// that comparison is exactly the kind of thing worth closing even
	// though the token is high-entropy.
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
