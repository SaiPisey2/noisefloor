// Package server is the read-only HTTP server and embedded UI for
// noisefloor (issue #12): it renders what `noisefloor scan` and
// `noisefloor coverage` already wrote to the database, and nothing else.
//
// Three rules this package holds itself to, because the design doc for
// issue #12 asks for them explicitly:
//
//  1. Every byte of untrusted data -- an alert name, a label value, an
//     annotation, a PromQL expression -- is rendered through html/template,
//     which escapes by default. Nothing in this package ever reaches for
//     template.HTML, template.JS or template.URL to push a string past that
//     escaping; where a page needs structure around untrusted text (the
//     episode-timeline tooltip, the label list), the template does the
//     building, not string concatenation in Go.
//
//  2. This server binds to loopback by default (DefaultAddr) and stays
//     there unless the operator running `noisefloor serve` passes an
//     explicit flag to bind wider -- see cmd/noisefloor's serve command,
//     which prints a warning when they do. There is no authentication here
//     at all; a wider bind is reconnaissance-grade exposure of a team's
//     whole alerting posture.
//
//  3. Nothing in this package writes to the database or talks to
//     Prometheus. It has no Prometheus client at all. A "rescan" button
//     would be an unauthenticated way to fire a hundred range queries at
//     production Prometheus; the fix is to not have the button.
package server

import (
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/SaiPisey2/noisefloor/internal/config"
	"github.com/SaiPisey2/noisefloor/internal/store"
)

// DefaultAddr is where `noisefloor serve` listens unless told otherwise.
// Loopback only: see the package doc comment.
const DefaultAddr = "127.0.0.1:9091"

// IsLoopback reports whether addr (host, or host:port) resolves to the
// loopback interface or the literal hostname "localhost". cmd/noisefloor's
// serve command uses this to decide whether a -addr the operator passed
// needs -allow-remote and its warning.
func IsLoopback(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Server is the read-only HTTP server. It holds only a database handle and
// the current scoring config (used solely to display the weights behind a
// stored noise score -- it never runs Score or NoiseScore against live
// data).
type Server struct {
	db      *store.SQLite
	cfg     config.Config
	tmpl    *template.Template
	reg     *prometheusRegistry
	nowFunc func() time.Time
}

var staticSub = mustSub(staticFS, "static")

func mustSub(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		// Only reachable if the embed directive above stopped matching the
		// static/ directory at build time -- a build-breaking programmer
		// error, not a runtime condition.
		panic(fmt.Sprintf("server: embedded static assets missing: %v", err))
	}
	return sub
}

// New builds a Server reading db, formatting scores against cfg's weights.
func New(cfg config.Config, db *store.SQLite) (*Server, error) {
	tmpl, err := parseTemplates()
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{
		db: db, cfg: cfg, tmpl: tmpl, reg: newPrometheusRegistry(db),
		nowFunc: time.Now,
	}, nil
}

// Handler builds the full route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /", s.instrument("/", s.handleLeaderboard))
	mux.Handle("GET /rules/{id}", s.instrument("/rules/{id}", s.handleRuleDetail))
	mux.Handle("GET /coverage", s.instrument("/coverage", s.handleCoverage))

	mux.Handle("GET /api/rules", s.instrument("/api/rules", s.handleAPIRules))
	mux.Handle("GET /api/rules/{id}", s.instrument("/api/rules/{id}", s.handleAPIRuleDetail))
	mux.Handle("GET /api/scores", s.instrument("/api/scores", s.handleAPIScores))
	mux.Handle("GET /api/coverage", s.instrument("/api/coverage", s.handleAPICoverage))

	mux.Handle("GET /healthz", s.instrument("/healthz", s.handleHealthz))
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.reg.registry, promhttp.HandlerOpts{}))

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticSub)))

	return mux
}

// statusRecorder captures the status code a handler wrote, for the request
// metrics below -- http.ResponseWriter has no getter of its own.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// instrument wraps a handler with the noisefloor_http_* metrics every
// route reports. route is the pattern (e.g. "/rules/{id}"), not the
// literal path, so a metrics label cardinality does not grow with the
// number of rules in the database.
func (s *Server) instrument(route string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		h(rec, r)
		dur := time.Since(start).Seconds()
		s.reg.httpRequests.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		s.reg.httpDuration.WithLabelValues(r.Method, route).Observe(dur)
	})
}

// idParam parses the {id} path value as a rule ID. ok is false for a
// malformed (non-numeric) ID, which the caller turns into a 400.
func idParam(r *http.Request) (int64, bool) {
	v := r.PathValue("id")
	id, err := strconv.ParseInt(v, 10, 64)
	return id, err == nil
}
