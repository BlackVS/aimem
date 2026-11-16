package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/schema"
	"aimem/internal/server"
	"aimem/internal/store"
)

func seedEvent(t *testing.T, db *store.DB, key string) {
	t.Helper()
	_, _, err := db.Append(&schema.Event{
		SchemaVersion: schema.Version, IdempotencyKey: key,
		Client: "claude-code", SessionID: "s1", TurnID: "t-" + key,
		Kind: schema.KindTurn, Outcome: schema.OutcomeOK,
		TS: time.Now().UTC().Format(time.RFC3339), UserRequest: "req " + key,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSyncOneHTTP exercises the full API transport: events both ways,
// memories both ways, cursors written — against a REAL hub server.
func TestSyncOneHTTP(t *testing.T) {
	// The hub: its own registry behind the real handler (auth is the TCP
	// wrapper's job; the transport under test sends the bearer anyway).
	hubReg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer hubReg.Close()
	hubSrv := server.New(hubReg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	ts := httptest.NewServer(hubSrv.Handler())
	defer ts.Close()
	hubDB, _ := hubReg.Open("proj-sync")
	seedEvent(t, hubDB, "hub-e1")
	if err := hubDB.ImportMemory(&store.Memory{
		ID: "hub-m1", Text: "fact from the hub", Kind: "fact", Confidence: 0.7,
		CreatedAt: "2026-08-29T00:00:00Z", Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}

	// The workstation: state root + running local service (the pull leg
	// lands events through it, like the ssh legs always did).
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	t.Setenv("AIMEM_EMBED_MODEL", "") // keep the post-sync backfill inert
	sock := filepath.Join(os.TempDir(), "aimem-synchttp.sock")
	os.Remove(sock)
	t.Setenv("AIMEM_SOCKET", sock)
	localReg, err := store.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	localSrv := server.New(localReg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	httpSrv, ln, err := localSrv.ListenAndServe(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { httpSrv.Close(); ln.Close(); localReg.Close(); os.Remove(sock) }()
	localDB, _ := localReg.Open("proj-sync")
	seedEvent(t, localDB, "local-e1")
	if err := localDB.ImportMemory(&store.Memory{
		ID: "local-m1", Text: "fact from the workstation", Kind: "fact", Confidence: 0.7,
		CreatedAt: "2026-08-29T00:00:00Z", Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}

	hub := &adapter.HubConfig{URL: ts.URL, Token: "t"}
	if err := syncOneHTTP("home", hub, "home"); err != nil {
		t.Fatal(err)
	}

	// Both sides converged.
	if tl, _ := hubDB.Timeline("s1", 0); len(tl) != 2 {
		t.Fatalf("hub events after sync: %d, want 2", len(tl))
	}
	if tl, _ := localDB.Timeline("s1", 0); len(tl) != 2 {
		t.Fatalf("local events after sync: %d, want 2", len(tl))
	}
	if mems, _ := hubDB.Memories(false); len(mems) != 2 {
		t.Fatalf("hub memories after sync: %d, want 2", len(mems))
	}
	if mems, _ := localDB.Memories(false); len(mems) != 2 {
		t.Fatalf("local memories after sync: %d, want 2", len(mems))
	}
	// Cursors landed under the hub-keyed name.
	if readCursor("hub:home", "push") == "" || readCursor("hub:home", "pull") == "" {
		t.Fatal("hub-keyed cursors not written")
	}

	// Idempotent re-run: nothing duplicates.
	if err := syncOneHTTP("home", hub, "home"); err != nil {
		t.Fatal(err)
	}
	if tl, _ := hubDB.Timeline("s1", 0); len(tl) != 2 {
		t.Fatalf("re-sync duplicated events: %d", len(tl))
	}
}

// TestSyncHubFallback pins the transport-selection contract: a hub
// without the sync routes falls back to ssh when one is configured, and
// says to upgrade when not.
func TestSyncHubFallback(t *testing.T) {
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer old.Close()
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	h := &adapter.HubConfig{URL: old.URL, Token: "t"} // no ssh destination
	err := syncHub("old", h, "old", "")
	if err == nil || !strings.Contains(err.Error(), "upgrade the hub") {
		t.Fatalf("missing-routes error: %v", err)
	}
	// No transport at all is its own, skippable error.
	if err := syncHub("bare", &adapter.HubConfig{}, "bare", ""); err != errNoSyncTransport {
		t.Fatalf("bare hub: %v", err)
	}
}

// TestOrphanBindings pins the loud-surface guarantee born from a binding
// incident: a project bound to an unconfigured hub NAME must be named
// by sync, because since the no-fallback guard it syncs nowhere.
func TestOrphanBindings(t *testing.T) {
	reg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	seed := func(id, hub string) {
		db, _ := reg.Open(id)
		if hub != "" {
			db.SetMeta("hub", hub)
		}
	}
	seed("good", "home")
	seed("unbound", "")
	seed("lost", "work") // the incident's case: name matches no configured hub
	reg.Open("group-g")  // groups are exempt
	hubs := map[string]*adapter.HubConfig{"home": {URL: "x", Token: "t"}}
	warns := orphanBindings(reg, hubs)
	if len(warns) != 1 || !strings.Contains(warns[0], `"work"`) || !strings.Contains(warns[0], "lost") {
		t.Fatalf("orphan detection: %v", warns)
	}
}

func TestVersionAtLeast(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		{"v0.3.24", true}, {"0.3.24", true}, {"v0.3.25", true}, {"v0.4.0", true},
		{"v1.0.0", true}, {"v0.3.24-3-gabc123", true}, {"v0.3.23", false},
		{"v0.3.23-9-gdef456", false}, {"v0.2.99", false}, {"dev", false},
		{"", false}, {"garbage", false},
	}
	for _, c := range cases {
		if got := adapter.VersionAtLeast(c.v, 0, 3, 24); got != c.want {
			t.Errorf("adapter.VersionAtLeast(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestVerifyEventStream(t *testing.T) {
	n := func(i int) *int { return &i }
	// Terminator present: count must match exactly, whatever the hub version.
	if err := verifyEventStream(n(3), 3, ""); err != nil {
		t.Errorf("matching terminator must verify: %v", err)
	}
	if err := verifyEventStream(n(5), 3, ""); err == nil {
		t.Error("count mismatch must refuse the stream")
	}
	// Terminator absent: fatal only when the hub is known to send one.
	if err := verifyEventStream(nil, 3, "v0.3.24"); err == nil {
		t.Error("missing terminator from a terminator-capable hub must refuse")
	}
	if err := verifyEventStream(nil, 3, "v0.3.23"); err != nil {
		t.Errorf("legacy hub without terminator must pass: %v", err)
	}
	if err := verifyEventStream(nil, 3, "dev"); err != nil {
		t.Errorf("undatable hub version must not require the terminator: %v", err)
	}
}

// syncMachine is one workstation: a state root and its running local
// service, the shape syncOneHTTP's pull legs land through.
func syncMachine(t *testing.T, name string) (*store.Registry, func()) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	sock := filepath.Join(os.TempDir(), "aimem-grp-"+name+".sock")
	os.Remove(sock)
	t.Setenv("AIMEM_SOCKET", sock)
	reg, err := store.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(reg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	httpSrv, ln, err := srv.ListenAndServe(root)
	if err != nil {
		t.Fatal(err)
	}
	return reg, func() { httpSrv.Close(); ln.Close(); reg.Close(); os.Remove(sock) }
}

// declare binds a project on the machine and declares the shared group,
// as the event pushes of a session in that project record it.
func declare(t *testing.T, reg *store.Registry, project string) {
	t.Helper()
	db, err := reg.Open(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta("groups", `["group-shared"]`); err != nil {
		t.Fatal(err)
	}
}

// A group this machine has never written is still pulled: machine B
// declares the group A wrote, has no store of it, and after one sync
// holds and recalls A's fact.
func TestSyncPullsDeclaredGroupWithoutLocalStore(t *testing.T) {
	t.Setenv("AIMEM_EMBED_MODEL", "")
	hubReg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer hubReg.Close()
	ts := httptest.NewServer(server.New(hubReg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))).Handler())
	defer ts.Close()
	hub := &adapter.HubConfig{URL: ts.URL, Token: "t"}

	// Machine A writes a group fact and syncs it to the hub.
	regA, stopA := syncMachine(t, "a")
	declare(t, regA, "proj-a")
	gA, err := regA.Open("group-shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := gA.ImportMemory(&store.Memory{
		ID: "grp-m1", Text: "the shared widget runs on port 9", Kind: "fact", Confidence: 0.7,
		CreatedAt: "2026-11-16T00:00:00Z", Actor: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := syncOneHTTP("primary", hub, "primary"); err != nil {
		t.Fatal(err)
	}
	stopA()

	// Machine B declares the group and has never written to it.
	regB, stopB := syncMachine(t, "b")
	defer stopB()
	declare(t, regB, "proj-b")
	if ids, _ := regB.Projects(); slices.Contains(ids, "group-shared") {
		t.Fatal("machine B has a group store before its first sync")
	}
	if err := syncOneHTTP("primary", hub, "primary"); err != nil {
		t.Fatal(err)
	}
	var res struct {
		Memories []store.Memory `json:"memories"`
	}
	if err := localGetJSON("/v1/projects/group-shared/memories/recall?q=widget&budget=1000", &res); err != nil {
		t.Fatalf("recall of the group on machine B after its sync: %v", err)
	}
	if len(res.Memories) != 1 || res.Memories[0].ID != "grp-m1" {
		t.Fatalf("machine B recalled %+v; want machine A's grp-m1", res.Memories)
	}
}
