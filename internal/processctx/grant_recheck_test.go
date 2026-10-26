package processctx

// A cached set is served Ready only while the hub vouches for it under a
// current grant (audit A-5): every load with a cache hit asks the set
// route conditionally on the digest. 304 serves the cache; a refusal is a
// denial and no cached set is used; any other answer leaves the cached set
// last observed, which never authorizes a task write.

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aimem/internal/process"
)

// revokable is a checkout whose hub served the set once, so this machine
// holds it in its cache by digest.
func revokable(t *testing.T) (h *fakeHub, dir, root string, sel process.Ref) {
	t.Helper()
	noGit(t)
	b := bundle("# Handbook\n\ngranted rules\n")
	sel = selection
	sel.Digest, sel.Name = b.Digest(), "proc"
	h = &fakeHub{}
	h.change(func(h *fakeHub) { h.tasksEnabled, h.ref, h.set = true, &sel, b })
	dir, root, _ = checkout(t, h, true)
	if r := Load(dir, root, ""); r.State != Ready || r.Source != SourceHub || h.setBodies.Load() != 1 {
		t.Fatalf("first load: %+v (set bodies %d)", r, h.setBodies.Load())
	}
	return h, dir, root, sel
}

func TestACachedSetIsServedOnlyWhileTheHubVouchesForIt(t *testing.T) {
	h, dir, root, sel := revokable(t)

	// The grant is revoked: the set route refuses, and the cached set is
	// not used. No set body is sent.
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		h.change(func(h *fakeHub) { h.setStatus = status })
		r := Load(dir, root, "")
		if r.State != Denied || r.Set != nil || r.Unit != "" {
			t.Fatalf("revoked (%d): %+v", status, r)
		}
		if hook, _ := Bootstrap(dir, root, "", false); !strings.Contains(hook, "DENIED") || strings.Contains(hook, "granted rules") {
			t.Fatalf("revoked (%d) hook: %q", status, hook)
		}
	}
	if h.setBodies.Load() != 1 {
		t.Fatalf("a refused read sent the set: %d bodies", h.setBodies.Load())
	}

	// A refusal whose body cannot be read whole is still a refusal: the
	// status decides, the body only gives the reason.
	for _, body := range []string{"cut", "huge"} {
		for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized} {
			h.change(func(h *fakeHub) { h.setStatus, h.setRefusalBody = status, body })
			r := Load(dir, root, "")
			if r.State != Denied || r.Set != nil || r.Unit != "" {
				t.Fatalf("revoked (%d, %s body): %+v", status, body, r)
			}
			if hook, _ := Bootstrap(dir, root, "", false); !strings.Contains(hook, "DENIED") || strings.Contains(hook, "granted rules") {
				t.Fatalf("revoked (%d, %s body) hook: %q", status, body, hook)
			}
			if r := LoadRef(dir, root, "", sel); r.State != Denied || r.Set != nil {
				t.Fatalf("pinned, revoked (%d, %s body): %+v", status, body, r)
			}
		}
	}
	h.change(func(h *fakeHub) { h.setRefusalBody = "" })

	// The grant again: 304, the cache, Ready, after a conditional read on
	// the digest in the form internal/server's set route sends as its ETag
	// (strong, quoted); the cache is not rewritten.
	h.change(func(h *fakeHub) { h.setStatus = 0 })
	handbook := filepath.Join(process.SetDir(root, sel.Digest), "proc", "handbook.md")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(handbook, old, old); err != nil {
		t.Fatal(err)
	}
	r := Load(dir, root, "")
	if r.State != Ready || r.Source != SourceCache || r.Unconfirmed != "" || !strings.Contains(r.Unit, "granted rules") {
		t.Fatalf("304: %+v", r)
	}
	if got := h.setIfNoneMatch; got != `"`+sel.Digest+`"` {
		t.Fatalf("If-None-Match = %q, want the strong ETag %q", got, `"`+sel.Digest+`"`)
	}
	if h.setBodies.Load() != 1 {
		t.Fatalf("the hub sent the set again: %d bodies", h.setBodies.Load())
	}
	if fi, err := os.Stat(handbook); err != nil || !fi.ModTime().Equal(old) {
		t.Fatalf("the 304 rewrote the cache: %v %v", fi.ModTime(), err)
	}

	// Any other answer: the cached set, last observed, saying so.
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		h.change(func(h *fakeHub) { h.setStatus = status })
		r := Load(dir, root, "")
		if r.State != LastObserved || r.Set == nil || r.Unconfirmed == "" || !strings.Contains(r.Unit, "granted rules") || !strings.Contains(r.Detail, "did not confirm") {
			t.Fatalf("status %d: %+v", status, r)
		}
		text, err := r.Deliver()
		if err != nil || !strings.Contains(text, "state last_observed") || !strings.Contains(text, "did not confirm this machine's cached process set") {
			t.Fatalf("status %d delivery: %v\n%.600s", status, err, text)
		}
		if hook, _ := Bootstrap(dir, root, "", false); !strings.HasPrefix(hook, "NOTE: the hub did not confirm") {
			t.Fatalf("status %d hook: %.200q", status, hook)
		}
	}

	// The set route's connection drops while the selection still answers:
	// last observed, not Ready.
	h.change(func(h *fakeHub) { h.setStatus, h.setDrop = 0, true })
	if r := Load(dir, root, ""); r.State != LastObserved || r.Set == nil || !strings.Contains(r.Unconfirmed, "hub unreachable") {
		t.Fatalf("set read dropped: %+v", r)
	}
}

// The pinned path asks the same way: by version for an earlier selection,
// for the current set when the pinned one is current.
func TestAPinnedCachedSetIsServedOnlyWhileTheHubVouchesForIt(t *testing.T) {
	h, dir, root, sel := revokable(t)
	for _, byVersion := range []bool{false, true} {
		h.change(func(h *fakeHub) {
			h.setStatus, h.kept = 0, map[string]*process.Ref{sel.Commit: &sel}
			if byVersion {
				h.ref = &process.Ref{Repo: sel.Repo, Commit: strings.Repeat("cd", 20), Manifest: sel.Manifest, Digest: strings.Repeat("ef", 32)}
			} else {
				h.ref = &sel
			}
		})
		r := LoadRef(dir, root, "", sel)
		if r.State != Ready || r.Source != SourceCache || h.setIfNoneMatch != `"`+sel.Digest+`"` || h.setBodies.Load() != 1 {
			t.Fatalf("pinned (by version %v), 304: %+v (If-None-Match %q, bodies %d)", byVersion, r, h.setIfNoneMatch, h.setBodies.Load())
		}
		h.change(func(h *fakeHub) { h.setStatus = http.StatusForbidden })
		if r := LoadRef(dir, root, "", sel); r.State != Denied || r.Set != nil {
			t.Fatalf("pinned (by version %v), revoked: %+v", byVersion, r)
		}
		h.change(func(h *fakeHub) { h.setStatus = http.StatusNotFound })
		r = LoadRef(dir, root, "", sel)
		if r.State != LastObserved || r.Set == nil {
			t.Fatalf("pinned (by version %v), not kept: %+v", byVersion, r)
		}
		if text, err := r.Deliver(); err != nil || !strings.Contains(text, "The hub did not confirm this cached copy") {
			t.Fatalf("pinned (by version %v) delivery: %v\n%.600s", byVersion, err, text)
		}
	}
}

// dropConn ends a request without an answer, as a connection reset does.
func dropConn(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		if c, _, err := hj.Hijack(); err == nil {
			if tc, ok := c.(*net.TCPConn); ok {
				tc.SetLinger(0)
			}
			c.Close()
		}
	}
}
