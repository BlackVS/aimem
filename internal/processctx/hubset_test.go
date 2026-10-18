package processctx

// The hub-served set is the default path: a member with no Git and no
// access to the process repository is ready for work from the hub's stored
// set, verified by the selection's digest and cached by it; a hub that
// stores no set leaves Git as the fallback; a refusal is a denial; a set
// that does not match the digest is never cached; nothing agent-facing
// names the repository URL.

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/process"
)

// bundle is the set the cache helper writes, as the hub would store it.
func bundle(handbook string) *process.Bundle {
	return &process.Bundle{Version: 1, Manifest: selection.Manifest, Files: map[string][]byte{
		selection.Manifest: []byte(`{"version":1,"handbook":"proc/handbook.md","checklists":{"READY":"proc/ready.json"},"templates":{"task":"proc/task.json"},"skills":["oh-code-review"]}`),
		"proc/handbook.md": []byte(handbook),
		"proc/ready.json":  []byte(`{"state":"READY","items":[{"id":"ready.outcome","text":"Objective and acceptance criteria are concrete."}]}`),
		"proc/task.json":   []byte(`{"title":"","objective":""}`),
	}}
}

// noGit removes Git from the lookup path for the test, so any Git use is
// a failure the test sees.
func noGit(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestHubServedSetNeedsNoGitAndNoRepositoryAccess(t *testing.T) {
	noGit(t)
	b := bundle("# Handbook\n\nRules from the hub.\n")
	sel := selection
	sel.Digest, sel.Name = b.Digest(), "proc"
	h := &fakeHub{}
	h.change(func(h *fakeHub) { h.tasksEnabled, h.ref, h.set = true, &sel, b })
	dir, root, _ := checkout(t, h, true)

	r := Load(dir, root, "")
	if r.State != Ready || r.Source != SourceHub || r.Set == nil || !strings.Contains(r.Unit, "Rules from the hub.") {
		t.Fatalf("hub-served set: %+v", r)
	}
	for _, text := range []string{r.Unit, r.Fix} {
		if strings.Contains(text, selection.Repo) || strings.Contains(text, "127.0.0.1") {
			t.Fatalf("agent-facing text names the repository URL:\n%s", text)
		}
	}
	if !strings.Contains(r.Unit, "Process set: proc @ "+selection.Commit[:12]+" (manifest proc/manifest.json).") {
		t.Fatalf("the unit does not name the set by name, commit and manifest:\n%s", r.Unit)
	}
	text, err := r.Deliver()
	if err != nil || !strings.Contains(text, "selection proc @ "+selection.Commit) || !strings.Contains(text, "from the hub's stored set, verified by its digest") || strings.Contains(text, selection.Repo) {
		t.Fatalf("delivery: %v\n%.600s", err, text)
	}
	if _, err := os.Stat(filepath.Join(process.SetDir(root, b.Digest()), ".complete")); err != nil {
		t.Fatal("the set was not cached by its digest")
	}
	if _, err := os.Stat(process.CacheDir(root, selection)); err == nil {
		t.Fatal("a Git cache entry appeared")
	}

	// The second load is served from the cache by digest: the set route
	// is not asked again, and the hub failing it changes nothing.
	h.change(func(h *fakeHub) { h.setStatus = http.StatusInternalServerError })
	before := h.setCalls.Load()
	r = Load(dir, root, "")
	if r.State != Ready || r.Source != SourceCache || h.setCalls.Load() != before || !strings.Contains(r.Unit, "from this machine's cache") {
		t.Fatalf("second load: %+v (set calls %d → %d)", r, before, h.setCalls.Load())
	}
	if hook, set := Bootstrap(dir, root, "", false); set == nil || strings.Contains(hook, selection.Repo) {
		t.Fatalf("hook: %q", hook)
	}
}

// A hub that refuses the set is a denial, not an outage, and Git is not
// tried in its place.
func TestHubRefusingTheSetIsDeniedNotMaskedByGit(t *testing.T) {
	noGit(t)
	b := bundle("# Handbook\n")
	sel := selection
	sel.Digest = b.Digest()
	h := &fakeHub{}
	h.change(func(h *fakeHub) { h.tasksEnabled, h.ref, h.set = true, &sel, b })
	dir, root, _ := checkout(t, h, true)
	for _, status := range []int{http.StatusForbidden, http.StatusUnauthorized} {
		h.change(func(h *fakeHub) { h.setStatus = status })
		r := Load(dir, root, "")
		if r.State != Denied || r.Set != nil || !strings.Contains(r.Fix, "credential") || strings.Contains(r.Fix+r.Unit, selection.Repo) {
			t.Fatalf("status %d: %+v", status, r)
		}
		if hook, _ := Bootstrap(dir, root, "", false); !strings.Contains(hook, "DENIED") || strings.Contains(hook, selection.Repo) {
			t.Fatalf("status %d hook: %q", status, hook)
		}
	}
	// A hub error on the set is unavailable, and nothing is cached.
	h.change(func(h *fakeHub) { h.setStatus = http.StatusBadGateway })
	if r := Load(dir, root, ""); r.State != Unavailable || r.Set != nil {
		t.Fatalf("502 on the set: %+v", r)
	}
	if _, err := os.Stat(process.SetDir(root, b.Digest())); err == nil {
		t.Fatal("a failed read left a cache entry")
	}
}

// A set that does not digest to the selection's digest is not the
// selection: unavailable, and never cached; the same when the hub's own
// claim and its files disagree.
func TestHubSetDigestMismatchIsRefusedAndNotCached(t *testing.T) {
	noGit(t)
	b := bundle("# Handbook\n")
	sel := selection
	sel.Digest = strings.Repeat("ab", 32) // not the files' digest
	h := &fakeHub{}
	h.change(func(h *fakeHub) { h.tasksEnabled, h.ref, h.set = true, &sel, b })
	dir, root, _ := checkout(t, h, true)
	if r := Load(dir, root, ""); r.State != Unavailable || r.Set != nil || !strings.Contains(r.Detail, "not the selection") {
		t.Fatalf("hub answers another digest: %+v", r)
	}
	// The hub claims the selection's digest for files that are not it.
	h.change(func(h *fakeHub) { h.setDigest = sel.Digest })
	if r := Load(dir, root, ""); r.State != Unavailable || r.Set != nil || !strings.Contains(r.Detail, "digest mismatch") {
		t.Fatalf("hub lies about the digest: %+v", r)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "process", "sets")); len(entries) != 0 {
		t.Fatalf("a mismatching set was cached: %v", entries)
	}
	// A body cut short is not a set: unavailable, nothing cached, and the
	// next load asks again.
	sel.Digest = b.Digest()
	h.change(func(h *fakeHub) { h.setDigest, h.setCut = "", true })
	if r := Load(dir, root, ""); r.State != Unavailable || r.Set != nil || !strings.Contains(r.Detail, "not a complete set") {
		t.Fatalf("truncated set: %+v", r)
	}
	if entries, _ := os.ReadDir(filepath.Join(root, "process", "sets")); len(entries) != 0 {
		t.Fatalf("a truncated set was cached: %v", entries)
	}
	h.change(func(h *fakeHub) { h.setCut = false })
	if r := Load(dir, root, ""); r.State != Ready || r.Source != SourceHub {
		t.Fatalf("after the cut: %+v", r)
	}
}

// A selection without a digest (an older hub's, or one recorded before
// sets) is served by the Git path without asking the set route; a
// selection with a digest whose set the hub no longer keeps (404) falls
// back to Git too. Here the commit's cache answers both, as a finished
// fetch leaves it.
func TestSelectionsTheHubStoresNoSetForUseGit(t *testing.T) {
	h := &fakeHub{}
	h.change(ready) // no digest, no set
	dir, root, _ := checkout(t, h, true)
	cache(t, root, selection, "# Handbook\n\nfetched earlier\n")
	r := Load(dir, root, "")
	if r.State != Ready || r.Source != SourceCache || h.setCalls.Load() != 0 {
		t.Fatalf("no digest: %+v (set calls %d)", r, h.setCalls.Load())
	}
	pruned := selection
	pruned.Digest = strings.Repeat("ab", 32)
	h.change(func(h *fakeHub) { h.ref = &pruned }) // the hub names a digest but keeps no set: 404
	r = Load(dir, root, "")
	if r.State != Ready || r.Source != SourceCache || h.setCalls.Load() != 1 {
		t.Fatalf("digest without a kept set: %+v (set calls %d)", r, h.setCalls.Load())
	}
	text, _ := r.Deliver()
	if strings.Contains(text, selection.Repo) {
		t.Fatalf("delivery names the URL:\n%.400s", text)
	}
}

// Offline, a selection with a digest is served from the set cached by that
// digest, as last observed, exactly as a commit's cache entry was.
func TestOfflineUsesTheCachedSetByDigest(t *testing.T) {
	noGit(t)
	b := bundle("# Handbook\n\ncached rules\n")
	sel := selection
	sel.Digest = b.Digest()
	h := &fakeHub{}
	h.change(func(h *fakeHub) { h.tasksEnabled, h.ref, h.set = true, &sel, b })
	dir, root, ts := checkout(t, h, false)
	if r := Load(dir, root, ""); r.State != Ready || r.Source != SourceHub {
		t.Fatalf("first load: %+v", r)
	}
	ts.Close()
	r := Load(dir, root, "")
	if r.State != LastObserved || !strings.Contains(r.Unit, "cached rules") || r.Set == nil || !r.Set.FromCache {
		t.Fatalf("offline: %+v", r)
	}
	if err := os.RemoveAll(process.SetDir(root, b.Digest())); err != nil {
		t.Fatal(err)
	}
	if r := Load(dir, root, ""); r.State != Unavailable || r.Set != nil {
		t.Fatalf("offline without the cached set: %+v", r)
	}
}

// A pinned version that is no longer the current selection is read from
// the hub's kept sets by commit and manifest, when this machine has no
// cache of it; the current selection is named, never delivered.
func TestPinnedVersionIsReadFromTheHubsKeptSets(t *testing.T) {
	noGit(t)
	older := bundle("# Handbook\n\nolder rules\n")
	newer := bundle("# Handbook\n\nnewer rules\n")
	pinned := selection
	pinned.Digest = older.Digest()
	current := process.Ref{Repo: selection.Repo, Commit: strings.Repeat("cd", 20), Manifest: selection.Manifest, Digest: newer.Digest()}
	h := &fakeHub{}
	h.change(func(h *fakeHub) {
		h.tasksEnabled, h.ref = true, &current
		h.set = older // what the kept lookup answers for the pinned commit
		h.kept = map[string]*process.Ref{pinned.Commit: &pinned}
	})
	dir, root, ts := checkout(t, h, false) // per-hub credential: offline is last observed, not unvalidated
	r := LoadRef(dir, root, "", pinned)
	if r.State != Ready || !strings.Contains(r.Unit, "older rules") || strings.Contains(r.Unit, "newer rules") || r.Source != SourceHub {
		t.Fatalf("pinned from the kept sets: %+v", r)
	}
	text, err := r.Deliver()
	if err != nil || !strings.Contains(text, "PINNED") || !strings.Contains(text, "The project now selects proc @ "+current.Commit) || strings.Contains(text, selection.Repo) {
		t.Fatalf("pinned delivery: %v\n%.600s", err, text)
	}
	// Pinned without a digest (an accept recorded before the record
	// carried one): the set route is not asked; the Git path serves it, or
	// here, with no Git, nothing does.
	pinnedOld := selection
	calls := h.setCalls.Load()
	if r := LoadRef(dir, root, "", pinnedOld); r.State == Ready || h.setCalls.Load() != calls {
		t.Fatalf("pinned without a digest asked the hub for a set: %+v (set calls %d)", r, h.setCalls.Load()-calls)
	}
	// Offline, the pinned version with its digest comes from the set
	// cached by it (the accept record carries the digest for this).
	ts.Close()
	if r := LoadRef(dir, root, "", pinned); r.State != LastObserved || !strings.Contains(r.Unit, "older rules") {
		t.Fatalf("pinned offline by digest: %+v", r)
	}
	if r := LoadRef(dir, root, "", pinnedOld); r.State != Unavailable {
		t.Fatalf("pinned offline without a digest cannot name its cache: %+v", r)
	}
}

// An older hub does not know the set route and refuses it with 403 (its
// ordinary-token allowlist), which must not read as a denial: a selection
// without a digest never asks the route, and the Git path serves it.
func TestOlderHubForbidsTheSetRouteAndGitStillServes(t *testing.T) {
	h := &fakeHub{}
	h.change(func(h *fakeHub) { ready(h); h.setStatus = http.StatusForbidden })
	dir, root, _ := checkout(t, h, true)
	cache(t, root, selection, "# Handbook\n\nfetched earlier\n")
	r := Load(dir, root, "")
	if r.State != Ready || r.Source != SourceCache || h.setCalls.Load() != 0 || !strings.Contains(r.Unit, "fetched earlier") {
		t.Fatalf("older hub answering 403: %+v (set calls %d)", r, h.setCalls.Load())
	}
	// The same older hub with a pinned version: Git as well.
	if r := LoadRef(dir, root, "", selection); r.State != Ready || h.setCalls.Load() != 0 {
		t.Fatalf("pinned against an older hub: %+v (set calls %d)", r, h.setCalls.Load())
	}
}

// Git names the repository in its errors; no notice an agent sees keeps
// the URL or the host, and the failure's category stays.
func TestGitErrorsAreRedactedOfTheRepository(t *testing.T) {
	cases := []struct{ repo, in, want string }{
		{"https://example.com/team/process.git", "git access denied: fatal: Authentication failed for 'https://example.com/team/process.git/'",
			"git access denied: fatal: Authentication failed for 'the process repository'"},
		{"https://git.example.com:4403/r.git", "git unreachable: fatal: unable to access 'https://git.example.com:4403/r.git/': Could not resolve host: git.example.com",
			"git unreachable: fatal: unable to access 'the process repository': Could not resolve host: its host"},
		{"git@forge.example.org:team/process.git", "git access denied: forge.example.org: Permission denied (publickey).",
			"git access denied: its host: Permission denied (publickey)."},
		{"https://example.com/team/process", "git: fatal: repository 'https://example.com/team/process/' not found",
			"git: fatal: repository 'the process repository' not found"},
		{"https://x/", "git: something exact and unrelated", "git: something exact and unrelated"},
	}
	for _, c := range cases {
		if got := redactRepo(errors.New(c.in), c.repo); got != c.want {
			t.Errorf("redactRepo(%q, %q):\n got %q\nwant %q", c.in, c.repo, got, c.want)
		}
	}
}
