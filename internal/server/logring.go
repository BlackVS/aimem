package server

// In-memory log ring for the admin GUI's Log tab: a slog.Handler that
// tees every record into a bounded ring while forwarding to the real
// handler (stderr/journald stays the durable log — the ring is a
// convenience window, lost on restart by design).

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// LogEntry is one captured record, flattened for JSON.
type LogEntry struct {
	TS    string `json:"ts"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
	Attrs string `json:"attrs,omitempty"`
}

// LogRing holds the last capacity records.
type LogRing struct {
	mu  sync.Mutex
	buf []LogEntry
	cap int
}

func NewLogRing(capacity int) *LogRing { return &LogRing{cap: capacity} }

func (r *LogRing) add(e LogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, e)
	if len(r.buf) > r.cap {
		r.buf = r.buf[len(r.buf)-r.cap:]
	}
}

// Tail returns up to n entries, newest last.
func (r *LogRing) Tail(n int) []LogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n <= 0 || n > len(r.buf) {
		n = len(r.buf)
	}
	out := make([]LogEntry, n)
	copy(out, r.buf[len(r.buf)-n:])
	return out
}

// ringHandler tees records into the ring and forwards to next.
type ringHandler struct {
	next  slog.Handler
	ring  *LogRing
	attrs string // pre-rendered WithAttrs context
}

// NewRingHandler wraps next so every record also lands in ring.
func NewRingHandler(next slog.Handler, ring *LogRing) slog.Handler {
	return &ringHandler{next: next, ring: ring}
}

func (h *ringHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *ringHandler) Handle(ctx context.Context, rec slog.Record) error {
	kv := h.attrs
	rec.Attrs(func(a slog.Attr) bool {
		if kv != "" {
			kv += " "
		}
		kv += fmt.Sprintf("%s=%v", a.Key, a.Value)
		return true
	})
	h.ring.add(LogEntry{
		TS: rec.Time.UTC().Format(time.RFC3339), Level: rec.Level.String(),
		Msg: rec.Message, Attrs: kv,
	})
	return h.next.Handle(ctx, rec)
}

func (h *ringHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	kv := h.attrs
	for _, a := range attrs {
		if kv != "" {
			kv += " "
		}
		kv += fmt.Sprintf("%s=%v", a.Key, a.Value)
	}
	return &ringHandler{next: h.next.WithAttrs(attrs), ring: h.ring, attrs: kv}
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	return &ringHandler{next: h.next.WithGroup(name), ring: h.ring, attrs: h.attrs}
}

// logs serves the ring to the admin GUI: GET /v1/logs?limit=N.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	if s.ring == nil {
		s.ok(w, map[string]any{"entries": []LogEntry{}})
		return
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 {
		n = 200
	}
	s.ok(w, map[string]any{"entries": s.ring.Tail(n)})
}
