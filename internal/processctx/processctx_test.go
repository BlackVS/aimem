package processctx

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/process"
	"aimem/internal/taskcred"
)

var projectSecret = "aimem_user_" + strings.Repeat("e", 64)

// fakeHub answers the two routes the process context reads. Its behavior
// can change between loads; calls counts every request.
type fakeHub struct {
	mu             sync.Mutex
	identityStatus int  // 0 = 200
	processStatus  int  // 0 = the selection below
	gatewayStatus  int  // when set, every route answers it
	tasksEnabled   bool // identity's tasks_enabled
	ref            *process.Ref
	calls          atomic.Int64
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.calls.Add(1)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.gatewayStatus != 0 {
		w.WriteHeader(h.gatewayStatus)
		return
	}
	switch {
	case r.URL.Path == "/v1/access/identity":
		if h.identityStatus != 0 {
			w.WriteHeader(h.identityStatus)
			json.NewEncoder(w).Encode(map[string]string{"error": "token revoked"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"scope": "project", "task_write": true, "tasks_enabled": h.tasksEnabled})
	case strings.HasSuffix(r.URL.Path, "/process"):
		if h.processStatus != 0 {
			w.WriteHeader(h.processStatus)
			json.NewEncoder(w).Encode(map[string]string{"error": "token revoked"})
			return
		}
		if h.ref == nil {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]string{"error": "no process reference selected for this project; an admin selects one (aimem process select … on the hub host)"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"project": "alpha", "current": h.ref})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (h *fakeHub) change(f func(h *fakeHub)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f(h)
}

// The selection points at an address nothing listens on: a cache miss can
// only fail, so every set these tests see comes from the exact cache.
var selection = process.Ref{Repo: "https://127.0.0.1:1/process.git", Commit: strings.Repeat("ab", 20), Manifest: "proc/manifest.json"}

// cache writes a complete exact-commit cache entry for ref, the way a
// finished fetch leaves it.
func cache(t *testing.T, root string, ref process.Ref, handbook string) {
	t.Helper()
	dir := process.CacheDir(root, ref)
	files := map[string]string{
		ref.Manifest:       `{"version":1,"handbook":"proc/handbook.md","checklists":{"READY":"proc/ready.json"},"templates":{"task":"proc/task.json"},"skills":["oh-code-review"]}`,
		"proc/handbook.md": handbook,
		"proc/ready.json":  `{"state":"READY","items":[{"id":"ready.outcome","text":"Objective and acceptance criteria are concrete."}]}`,
		"proc/task.json":   `{"title":"","objective":""}`,
		".complete":        ref.Manifest + "\n",
	}
	for p, body := range files {
		fp := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(fp), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// checkout binds a temporary directory (not a Git repository, not aimem's)
// to the fake hub. local installs a required project-local credential;
// otherwise the hub's own token is what the lookup presents.
func checkout(t *testing.T, h *fakeHub, local bool) (dir, root string, ts *httptest.Server) {
	t.Helper()
	ts = httptest.NewServer(h)
	t.Cleanup(ts.Close)
	root, dir = t.TempDir(), t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, Token: "checkpoint", TaskToken: "aimem_user_global"}}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aimem.json"), []byte(`{"project":"alpha"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if local {
		if err := taskcred.Set(context.Background(), dir, root, projectSecret); err != nil {
			t.Fatal(err)
		}
	}
	return dir, root, ts
}

func ready(h *fakeHub) {
	h.tasksEnabled, h.ref, h.identityStatus, h.gatewayStatus = true, &selection, 0, 0
}

func TestLoadDeliversTheCompleteSelectedUnitFromTheExactCache(t *testing.T) {
	h := &fakeHub{}
	h.change(ready)
	dir, root, _ := checkout(t, h, true)
	cache(t, root, selection, "# Handbook\n\nWork only on READY tasks.\n")

	r := Load(dir, root, "")
	if r.State != Ready || r.Source != SourceCache || r.Project != "alpha" || r.Ref.Commit != selection.Commit || r.Problem() != nil {
		t.Fatalf("load: %+v", r)
	}
	for _, want := range []string{"Work only on READY tasks.", "[ready.outcome]", "oh-code-review", "Process set: " + selection.Repo} {
		if !strings.Contains(r.Unit, want) {
			t.Errorf("unit lacks %q:\n%s", want, r.Unit)
		}
	}
	text, err := r.Deliver()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(r.Unit))
	terminator := "=== end aimem process context unit project alpha commit " + selection.Commit + " manifest " + selection.Manifest + " sha256 " + hex.EncodeToString(sum[:]) + " ==="
	if !strings.HasSuffix(text, terminator+"\n") || !strings.Contains(text, r.Unit) || !strings.Contains(text, "state ready") {
		t.Fatalf("delivery:\n%s", text)
	}
	tmpl, err := r.Template("task")
	if err != nil || !strings.Contains(tmpl, `{"title":"","objective":""}`) || !strings.Contains(tmpl, "=== end aimem process context template task project alpha") {
		t.Fatalf("template: %v\n%s", err, tmpl)
	}
	for _, kind := range []string{"../proc/task.json", "proc/task.json", "https://example.com/t", "TASK"} {
		if _, err := r.Template(kind); err == nil || !strings.Contains(err.Error(), "kinds: task") {
			t.Errorf("kind %q: %v", kind, err)
		}
	}
	if _, err := r.Template("investigation"); err == nil || !strings.Contains(err.Error(), `unknown template kind "investigation"; kinds: task`) {
		t.Errorf("unknown kind: %v", err)
	}
	// The hook's text is unchanged for a ready project.
	if hook, set := Bootstrap(dir, root, "", false); set == nil || !strings.Contains(hook, "Work only on READY tasks.") || strings.Contains(hook, "=== end") {
		t.Errorf("hook text: %q", hook)
	}
}

func TestLoadStatesWhenThereIsNothingToDeliver(t *testing.T) {
	h := &fakeHub{}
	h.change(ready)
	dir, root, _ := checkout(t, h, true)

	h.change(func(h *fakeHub) { h.tasksEnabled = false })
	if r := Load(dir, root, ""); r.State != Disabled || r.Set != nil {
		t.Errorf("tasks off: %+v", r)
	}
	if text, _ := Bootstrap(dir, root, "", false); text != "" {
		t.Errorf("tasks off must stay silent in the hook: %q", text)
	}

	h.change(func(h *fakeHub) { h.tasksEnabled, h.ref = true, nil })
	r := Load(dir, root, "")
	if r.State != NotSelected || !strings.Contains(r.Fix, "aimem process select") {
		t.Errorf("no selection: %+v", r)
	}
	if _, err := r.Deliver(); err == nil || !strings.Contains(err.Error(), "state not_selected") {
		t.Errorf("no selection delivers: %v", err)
	}

	// Selected, but this machine has neither the commit cached nor a
	// reachable Git: unavailable, nothing substituted.
	h.change(ready)
	if r := Load(dir, root, ""); r.State != Unavailable || r.Set != nil || !strings.Contains(r.Detail, selection.Commit[:12]) {
		t.Errorf("cache miss: %+v", r)
	}
}

// A hub that refuses the credential is a denial: the selection this
// machine observed earlier, and its cached set, must not stand in for it.
func TestHubRefusalIsNeverMaskedByTheLastObservedSelection(t *testing.T) {
	for _, local := range []bool{false, true} {
		h := &fakeHub{}
		h.change(ready)
		dir, root, _ := checkout(t, h, local)
		cache(t, root, selection, "# Handbook\n\nsecret policy\n")
		if r := Load(dir, root, ""); r.State != Ready {
			t.Fatalf("local=%v: first load %+v", local, r)
		}
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			h.change(func(h *fakeHub) { h.identityStatus = status })
			r := Load(dir, root, "")
			if r.State != Denied || r.Set != nil || r.Unit != "" || r.ObservedAt != "" {
				t.Errorf("local=%v status %d: %+v", local, status, r)
			}
			text, _ := Bootstrap(dir, root, "", false)
			if strings.Contains(text, "secret policy") || strings.Contains(text, "last observed") {
				t.Errorf("local=%v status %d: the hook served the cached selection: %q", local, status, text)
			}
		}
	}
}

// Only a hub that cannot be asked (a transport failure, or a gateway saying
// so) lets the last-observed selection stand in, and only with its exact
// cached commit.
func TestUnreachableHubUsesOnlyTheExactCachedCommit(t *testing.T) {
	h := &fakeHub{}
	h.change(ready)
	dir, root, ts := checkout(t, h, false)
	cache(t, root, selection, "# Handbook\n\ncached rules\n")
	if r := Load(dir, root, ""); r.State != Ready {
		t.Fatalf("first load %+v", r)
	}

	h.change(func(h *fakeHub) { h.gatewayStatus = http.StatusServiceUnavailable })
	r := Load(dir, root, "")
	if r.State != LastObserved || r.ObservedAt == "" || !strings.Contains(r.Unit, "cached rules") {
		t.Fatalf("gateway 503: %+v", r)
	}
	text, err := r.Deliver()
	if err != nil || !strings.Contains(text, "state last_observed") || !strings.Contains(text, "NOTE: the hub is unreachable") {
		t.Errorf("last-observed delivery must say so: %v\n%.400s", err, text)
	}

	h.change(func(h *fakeHub) { h.gatewayStatus = http.StatusInternalServerError })
	if r := Load(dir, root, ""); r.State != Unavailable || r.Set != nil {
		t.Errorf("a hub error is not an outage: %+v", r)
	}

	ts.Close()
	r = Load(dir, root, "")
	if r.State != LastObserved || !strings.Contains(r.Unit, "cached rules") {
		t.Fatalf("closed hub: %+v", r)
	}
	if hook, _ := Bootstrap(dir, root, "", false); !strings.HasPrefix(hook, "NOTE: the hub is unreachable") {
		t.Errorf("hook text: %q", hook)
	}
	// Without the exact cached commit there is nothing to deliver.
	if err := os.RemoveAll(process.CacheDir(root, selection)); err != nil {
		t.Fatal(err)
	}
	if r := Load(dir, root, ""); r.State != Unavailable || r.Set != nil {
		t.Errorf("no exact cache: %+v", r)
	}
}

// A required local credential that is missing is the end of the lookup:
// the hub is not asked, and the per-hub user credential is not used.
func TestRequiredLocalCredentialNeverFallsBack(t *testing.T) {
	h := &fakeHub{}
	h.change(ready)
	dir, root, _ := checkout(t, h, true)
	cache(t, root, selection, "# Handbook\n")
	files, _ := filepath.Glob(filepath.Join(root, "task-credentials", "*.json"))
	if len(files) != 1 {
		t.Fatal("credential file", files)
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	before := h.calls.Load()
	r := Load(dir, root, "")
	if r.State != Unavailable || r.Set != nil || !strings.Contains(r.Fix, "aimem task-token set") {
		t.Errorf("missing credential: %+v", r)
	}
	if h.calls.Load() != before {
		t.Error("the hub was contacted without the required credential")
	}
}

func TestTooLargeIsRefusedWholeButTemplatesStillServe(t *testing.T) {
	h := &fakeHub{}
	h.change(ready)
	dir, root, _ := checkout(t, h, true)
	cache(t, root, selection, "# Handbook\n\n"+strings.Repeat("rule line\n", 4000))
	r := Load(dir, root, "")
	if r.State != TooLarge || r.Unit != "" || r.Set == nil || !strings.Contains(r.Fix, "splits the handbook") {
		t.Fatalf("too large: %+v", r)
	}
	if _, err := r.Deliver(); err == nil || !strings.Contains(err.Error(), "state too_large") {
		t.Errorf("a too-large unit must not deliver: %v", err)
	}
	if tmpl, err := r.Template("task"); err != nil || !strings.Contains(tmpl, "state too_large") {
		t.Errorf("template of a complete set: %v %s", err, tmpl)
	}
}

// A Git server that refuses this machine is a denial, not an outage.
func TestGitRefusalIsDenied(t *testing.T) {
	git := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="process"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer git.Close()
	// No credential helper or TLS verification from this machine's
	// configuration may answer for the test.
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
	ref := process.Ref{Repo: git.URL + "/process.git", Commit: selection.Commit, Manifest: selection.Manifest}
	h := &fakeHub{}
	h.change(func(h *fakeHub) { h.tasksEnabled, h.ref = true, &ref })
	dir, root, _ := checkout(t, h, true)
	r := Load(dir, root, "")
	if r.State != Denied || r.Set != nil || !strings.Contains(r.Fix, "read access") {
		t.Fatalf("git refusal: %+v", r)
	}
}

// A malformed selection, from the hub or from a damaged last-observed
// record, is reported as unavailable; it never crashes the process that
// serves the lookup (the local MCP server, the session-start hook).
func TestMalformedSelectionIsUnavailableNotACrash(t *testing.T) {
	h := &fakeHub{}
	bad := process.Ref{Repo: selection.Repo, Commit: "abc", Manifest: selection.Manifest}
	h.change(func(h *fakeHub) { h.tasksEnabled, h.ref = true, &bad })
	dir, root, ts := checkout(t, h, false)
	if r := Load(dir, root, ""); r.State != Unavailable || r.Set != nil {
		t.Errorf("malformed hub selection: %+v", r)
	}
	last := filepath.Join(root, "process", "last-alpha.json")
	if err := os.WriteFile(last, []byte(`{"ref":{"repo":"https://127.0.0.1:1/p.git","commit":"abc","manifest":"m.json"},"observed_at":"2026-09-24T00:00:00Z"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ts.Close()
	r := Load(dir, root, "")
	if r.State != Unavailable || r.Set != nil || !strings.Contains(r.Detail, "no last-observed selection") {
		t.Errorf("damaged last-observed record: %+v", r)
	}
}

// writeLast records sel as this machine's last observed selection.
func writeLast(t *testing.T, root string, sel process.Ref, observedAt string) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"ref": sel, "observed_at": observedAt})
	if err := os.MkdirAll(filepath.Join(root, "process"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "process", "last-alpha.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A last-observed selection is served from the exact cache only: with the
// commit missing from the cache and Git reachable, Git is not contacted.
func TestLastObservedSelectionNeverFetches(t *testing.T) {
	var gitHits atomic.Int64
	git := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gitHits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer git.Close()
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	h := &fakeHub{}
	dir, root, ts := checkout(t, h, false)
	writeLast(t, root, process.Ref{Repo: git.URL + "/process.git", Commit: selection.Commit, Manifest: selection.Manifest}, "2026-09-24T00:00:00Z")
	ts.Close()
	r := Load(dir, root, "")
	if r.State != Unavailable || r.Set != nil || !strings.Contains(r.Detail, "not complete in this machine's cache") {
		t.Errorf("last observed, cache miss: %+v", r)
	}
	if n := gitHits.Load(); n != 0 {
		t.Errorf("Git was contacted %d times for a selection the hub did not confirm", n)
	}
}

// A hub that fails to answer the local credential's validation is an
// outage; only 401 and 403 are a verdict on the credential.
func TestLocalCredentialValidationOutageIsNotADenial(t *testing.T) {
	h := &fakeHub{}
	h.change(ready)
	dir, root, _ := checkout(t, h, true)
	cache(t, root, selection, "# Handbook\n")
	for _, status := range []int{http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusInternalServerError} {
		h.change(func(h *fakeHub) { h.gatewayStatus = status })
		if r := Load(dir, root, ""); r.State != Unavailable || strings.Contains(r.Fix, "reissues") {
			t.Errorf("status %d: %+v", status, r)
		}
	}
	h.change(func(h *fakeHub) { h.gatewayStatus, h.identityStatus = 0, http.StatusUnauthorized })
	if r := Load(dir, root, ""); r.State != Denied {
		t.Errorf("401: %+v", r)
	}
}

// A last-observed record that cannot say when it was observed is damaged:
// its selection is not delivered, and never as ready.
func TestLastObservedRecordWithoutTimeIsNotUsed(t *testing.T) {
	h := &fakeHub{}
	dir, root, ts := checkout(t, h, false)
	cache(t, root, selection, "# Handbook\n\nstale rules\n")
	ts.Close()
	for _, at := range []string{"", "yesterday"} {
		writeLast(t, root, selection, at)
		if r := Load(dir, root, ""); r.State != Unavailable || r.Set != nil || strings.Contains(r.Unit, "stale rules") {
			t.Errorf("observed_at %q: %+v", at, r)
		}
	}
	writeLast(t, root, selection, "2026-09-24T00:00:00Z")
	r := Load(dir, root, "")
	if r.State != LastObserved || !r.FromLastObserved {
		t.Fatalf("valid record: %+v", r)
	}
	if text, err := r.Deliver(); err != nil || !strings.Contains(text, "NOTE: the hub is unreachable") {
		t.Errorf("delivery must be marked: %v", err)
	}
}

// LoadRef runs Load's live checks: a refusal of the selection read after a
// successful identity read is a denial, and the pinned cache does not stand
// in for it.
func TestLoadRefHonorsADeniedSelectionRead(t *testing.T) {
	h := &fakeHub{}
	h.change(ready)
	dir, root, _ := checkout(t, h, true)
	cache(t, root, selection, "# Handbook\n\npinned rules\n")
	if r := LoadRef(dir, root, "", selection); r.State != Ready || !strings.Contains(r.Unit, "pinned rules") {
		t.Fatalf("pinned, ready: %+v", r)
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		h.change(func(h *fakeHub) { h.processStatus = status })
		r := LoadRef(dir, root, "", selection)
		if r.State != Denied || r.Set != nil || r.Unit != "" {
			t.Fatalf("status %d: %+v", status, r)
		}
		if _, err := r.Deliver(); err == nil {
			t.Fatalf("status %d: a denied pin delivered", status)
		}
	}
}
