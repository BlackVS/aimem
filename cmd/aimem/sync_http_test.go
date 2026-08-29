package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
