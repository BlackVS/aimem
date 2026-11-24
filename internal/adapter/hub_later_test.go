package adapter

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// laterHub answers every request with the status set, and counts each
// body it sees.
type laterHub struct {
	mu         sync.Mutex
	status     int
	retryAfter string
	seen       map[string]int
}

func newLaterHub(t *testing.T, root string, status int, retryAfter string) (*laterHub, *httptest.Server) {
	t.Helper()
	h := &laterHub{status: status, retryAfter: retryAfter, seen: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.seen[b.String()]++
		if h.retryAfter != "" {
			w.Header().Set("Retry-After", h.retryAfter)
		}
		w.WriteHeader(h.status)
	}))
	t.Cleanup(srv.Close)
	if err := SaveHubs(root, map[string]*HubConfig{"default": {URL: srv.URL, Token: "t"}}, "default"); err != nil {
		t.Fatal(err)
	}
	return h, srv
}

func (h *laterHub) answer(status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status, h.retryAfter = status, ""
}

func (h *laterHub) count(body string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seen[body]
}

func hubSpoolLines(t *testing.T, root string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "spool", "hub.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// A 408 or a 429 asks the client to come back later: the record stays
// queued, the run stops asking that hub, the run's line names the status
// and the Retry-After, and the next run delivers the kept record once.
func TestHubComeBackLaterKeepsTheRecordQueued(t *testing.T) {
	for _, tc := range []struct {
		status     int
		retryAfter string
		want       string
	}{
		{http.StatusTooManyRequests, "120", "the hub delivery (hub HTTP 429, Retry-After 120 s)"},
		{http.StatusRequestTimeout, "Wed, 25 Nov 2026 07:28:00 GMT", "the hub delivery (hub HTTP 408, Retry-After Wed, 25 Nov 2026 07:28:00 GMT)"},
		{http.StatusTooManyRequests, "", "the hub delivery (hub HTTP 429)"},
		{http.StatusTooManyRequests, "soon, see https://example.invalid", "the hub delivery (hub HTTP 429, Retry-After unreadable)"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			root := t.TempDir()
			hub, _ := newLaterHub(t, root, tc.status, tc.retryAfter)
			kept := `{"n":1}`
			c := NewClient(root)
			c.pushHub("", []byte(kept))
			if got := hubSpoolLines(t, root); !slices.Equal(got, []string{kept}) {
				t.Fatalf("queued %q, want the record", got)
			}
			if !slices.Contains(c.deferredList, tc.want) {
				t.Fatalf("deferred %q, want %q", c.deferredList, tc.want)
			}
			// The run's delivery to that hub ended: a second record is
			// queued without a request.
			c.pushHub("", []byte(`{"n":2}`))
			if n := hub.count(`{"n":2}`); n != 0 {
				t.Fatalf("the run asked the hub again (%d)", n)
			}
			notes, _ := os.ReadFile(filepath.Join(root, "adapter.log"))
			if !strings.Contains(string(notes), "asked to come back later") || strings.Contains(string(notes), "example.invalid") {
				t.Fatalf("adapter.log: %s", notes)
			}

			// The next run, with the hub answering, delivers each kept
			// record exactly once and empties the queue.
			hub.answer(http.StatusOK)
			next := NewClient(root)
			next.pushHub("", []byte(`{"n":3}`))
			for _, b := range []string{`{"n":2}`, `{"n":3}`} {
				if n := hub.count(b); n != 1 {
					t.Fatalf("%s reached the hub %d time(s), want 1", b, n)
				}
			}
			if n := hub.count(kept); n != 2 { // the refused try, then the delivery
				t.Fatalf("the kept record was asked %d time(s), want the refused try and one delivery", n)
			}
			if got := hubSpoolLines(t, root); len(got) != 0 {
				t.Fatalf("still queued: %q", got)
			}
		})
	}
}

// Every other 4xx but 401 and 403 keeps today's answer: the record is
// taken as invalid and dropped from the hub path, without a line.
func TestHubOtherClientErrorsStillDropTheRecord(t *testing.T) {
	for _, status := range []int{400, 404, 409, 413, 422} {
		root := t.TempDir()
		hub, _ := newLaterHub(t, root, status, "60")
		c := NewClient(root)
		c.pushHub("", []byte(`{"n":1}`))
		c.pushHub("", []byte(`{"n":2}`))
		if got := hubSpoolLines(t, root); len(got) != 0 {
			t.Fatalf("HTTP %d: queued %q", status, got)
		}
		if hub.count(`{"n":2}`) != 1 || len(c.deferredList) != 0 {
			t.Fatalf("HTTP %d: second record asked %d time(s), deferred %q", status, hub.count(`{"n":2}`), c.deferredList)
		}
	}
}

func TestRetryAfterText(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "",
		" 30 ":                              "30 s",
		"0":                                 "0 s",
		"99999999999":                       "unreadable",
		"-5":                                "unreadable",
		"1.5":                               "unreadable",
		"Wed, 25 Nov 2026 07:28:00 GMT":     "Wed, 25 Nov 2026 07:28:00 GMT",
		"Wednesday, 25-Nov-26 07:28:00 GMT": "Wed, 25 Nov 2026 07:28:00 GMT",
		"\x1b[31mred":                       "unreadable",
	} {
		if got := retryAfterText(in); got != want {
			t.Errorf("retryAfterText(%q) = %q, want %q", in, got, want)
		}
	}
}

// A 429 in the middle of a backlog flush stops the flush; the record it
// stopped at and the rest stay queued, in order.
func TestHubComeBackLaterMidFlushKeepsTheRest(t *testing.T) {
	root := t.TempDir()
	var mu sync.Mutex
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		asked++
		if asked > 2 { // the run's own record and the first queued one
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	if err := SaveHubs(root, map[string]*HubConfig{"default": {URL: srv.URL, Token: "t"}}, "default"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "spool"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "spool", "hub.jsonl"), []byte("{\"q\":1}\n{\"q\":2}\n{\"q\":3}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewClient(root)
	c.pushHub("", []byte(`{"live":1}`))
	if got := hubSpoolLines(t, root); !slices.Equal(got, []string{`{"q":2}`, `{"q":3}`}) {
		t.Fatalf("queued %q, want the record the flush stopped at and the rest", got)
	}
	if asked != 3 || !slices.Contains(c.deferredList, "the hub delivery (hub HTTP 429, Retry-After 30 s)") {
		t.Fatalf("asked %d, deferred %q", asked, c.deferredList)
	}
}
