package server

// The hub's public face. A hub listens on a routable name, so anything
// served without the token is served to the internet: this page and its
// endpoint answer only "is it up, which build, how long" — never project
// names, spend, or log lines, which stay behind the token at /admin.

import (
	_ "embed"
	"net/http"
	"os"
	"time"
)

//go:embed status.html
var statusHTML []byte

// startedAt is process start, for the uptime line. Package-level because
// there is one server per process and the page predates any Server value
// being reachable from the handler that reports it.
var startedAt = time.Now()

func (s *Server) statusPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(statusHTML)
}

// status is the unauthenticated liveness document. Every field here is
// public by construction; adding one is a disclosure decision.
func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	body := map[string]any{
		"status":     "ok",
		"uptime_s":   int64(time.Since(startedAt).Seconds()),
		"started_at": startedAt.UTC().Format(time.RFC3339),
	}
	if Version != "" {
		body["version"] = Version
	}
	if n := os.Getenv("AIMEM_HUB_NAME"); n != "" {
		body["hub_name"] = n
	}
	if hn, err := os.Hostname(); err == nil {
		body["host"] = hn
	}
	s.ok(w, body)
}
