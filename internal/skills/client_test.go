package skills

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeHub serves the two skills routes for one project from a set of
// entries, with knobs for the failures a member must handle.
type fakeHub struct {
	index     IndexAnswer
	sets      map[string]map[string][]byte // digest -> files
	status    int                          // when set, every route answers it
	lieDigest bool                         // claim the asked digest for other files
	cut       bool                         // cut the set body short
	calls     atomic.Int64
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.calls.Add(1)
	if r.Header.Get("Authorization") != "Bearer member" {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "token revoked"})
		return
	}
	if h.status != 0 {
		w.WriteHeader(h.status)
		json.NewEncoder(w).Encode(map[string]string{"error": "answered " + http.StatusText(h.status)})
		return
	}
	switch {
	case r.URL.Path == "/v1/projects/alpha/skills":
		json.NewEncoder(w).Encode(h.index)
	case r.URL.Path == "/v1/projects/alpha/skills/set":
		d := r.URL.Query().Get("digest")
		files, ok := h.sets[d]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "this hub keeps no skills set"})
			return
		}
		if h.lieDigest {
			files = map[string][]byte{"other/SKILL.md": []byte("other")}
		}
		raw, _ := json.Marshal(map[string]any{"project": "alpha", "digest": d, "files": files})
		if h.cut {
			raw = raw[:len(raw)/2]
		}
		w.Write(raw)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newFakeHub(t *testing.T) (*fakeHub, *Client, *Store) {
	t.Helper()
	rel := releaseFiles()
	d := (&Set{Files: rel}).Digest()
	h := &fakeHub{sets: map[string]map[string][]byte{d: rel}}
	h.index = IndexAnswer{Project: "alpha", Sources: map[string]*Entry{SourceRelease: {Version: "1.33.13", Digest: d, Skills: []string{"oh-code-review", "verify-delivery"}}}, Kept: map[string][]Entry{}}
	h.index.Effective = Effective(h.index.Sources)
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	root := t.TempDir()
	return h, &Client{BaseURL: ts.URL, Token: "member", HTTP: ts.Client(), Project: "alpha"}, NewStore(root)
}

func TestClientReadsVerifiesAndCachesAndNeedsNoGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no git anywhere; nothing but the hub is contacted
	h, c, cache := newFakeHub(t)
	ctx := context.Background()
	ix, out, err := c.Index(ctx)
	if out != OutcomeOK || err != nil || len(ix.Effective) != 2 {
		t.Fatalf("index: %v %v %+v", out, err, ix)
	}
	d := ix.Sources[SourceRelease].Digest
	files, out, err := c.Fetch(ctx, cache, d)
	if out != OutcomeOK || err != nil || string(files["verify-delivery/SKILL.md"]) == "" {
		t.Fatalf("fetch: %v %v", out, err)
	}
	if !cache.Has(d) {
		t.Fatal("the verified set was not cached")
	}
	// The second read is served from the cache: the hub is not asked, and
	// a hub that is down changes nothing.
	before := h.calls.Load()
	h.status = http.StatusBadGateway
	if _, out, err := c.Fetch(ctx, cache, d); out != OutcomeOK || err != nil || h.calls.Load() != before {
		t.Fatalf("cached read: %v %v (calls %d → %d)", out, err, before, h.calls.Load())
	}
}

func TestClientOutcomes(t *testing.T) {
	h, c, cache := newFakeHub(t)
	ctx := context.Background()
	d := h.index.Sources[SourceRelease].Digest
	// Denied: the credential or the grant; never masked.
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		h.status = status
		if _, out, _ := c.Index(ctx); out != OutcomeDenied {
			t.Fatalf("index %d: %v", status, out)
		}
		if _, out, _ := c.Fetch(ctx, cache, d); out != OutcomeDenied {
			t.Fatalf("fetch %d: %v", status, out)
		}
	}
	// Unsupported: an older hub without the route.
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		h.status = status
		if _, out, _ := c.Index(ctx); out != OutcomeUnsupported {
			t.Fatalf("index %d: %v", status, out)
		}
	}
	h.status = http.StatusBadGateway
	if _, out, _ := c.Index(ctx); out != OutcomeFailed {
		t.Fatal("502 must be a failure, not denied or unsupported")
	}
	h.status = 0
	// A set that does not digest to its digest is refused and not cached;
	// so is a body cut short.
	h.lieDigest = true
	if _, out, err := c.Fetch(ctx, cache, d); out != OutcomeFailed || err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("mismatch: %v %v", out, err)
	}
	h.lieDigest, h.cut = false, true
	if _, out, err := c.Fetch(ctx, cache, d); out != OutcomeFailed || err == nil || !strings.Contains(err.Error(), "not a complete set") {
		t.Fatalf("truncated: %v %v", out, err)
	}
	if entries, _ := os.ReadDir(filepath.Join(cache.Dir(), "sets")); len(entries) != 0 {
		t.Fatalf("a refused set was cached: %v", entries)
	}
	h.cut = false
	if _, out, err := c.Fetch(ctx, cache, d); out != OutcomeOK || err != nil {
		t.Fatalf("after the cut: %v %v", out, err)
	}
	// A digest the hub no longer keeps.
	if _, out, _ := c.Fetch(ctx, cache, strings.Repeat("1", 64)); out != OutcomeUnsupported {
		t.Fatalf("unknown digest: %v", out)
	}
	// A cached entry damaged under its marker is replaced by the hub's
	// verified answer, not served and not left in place.
	os.WriteFile(filepath.Join(SetDir(cache.root, d), "oh-code-review", "SKILL.md"), []byte("edited by hand"), 0o600)
	before := h.calls.Load()
	files, out, err := c.Fetch(ctx, cache, d)
	if out != OutcomeOK || err != nil || h.calls.Load() != before+1 || strings.Contains(string(files["oh-code-review/SKILL.md"]), "edited") {
		t.Fatalf("damaged cache: %v %v (calls %d → %d)", out, err, before, h.calls.Load())
	}
	if got, err := cache.Read(d); err != nil || strings.Contains(string(got["oh-code-review/SKILL.md"]), "edited") {
		t.Fatalf("the damaged entry was not healed: %v", err)
	}
}
