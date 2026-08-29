package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/ident"
	"aimem/internal/server"
	"aimem/internal/store"
)

// TestSessionFactsNotice pins the opt-in session-start injection: off
// without config, and with it a budgeted slice of facts recalled
// against the previous session's requests.
func TestSessionFactsNotice(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	sock := filepath.Join(os.TempDir(), "aimem-sfacts.sock")
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
	defer func() { httpSrv.Close(); ln.Close(); reg.Close(); os.Remove(sock) }()

	proj := t.TempDir()
	os.WriteFile(filepath.Join(proj, ".aimem.json"),
		[]byte(`{"project":"proj-sfacts","session_facts":300}`), 0o600)
	t.Chdir(proj)

	// No journal yet: nothing to build a query from, silent.
	if n := sessionFactsNotice(); n != "" {
		t.Fatalf("empty journal should stay silent: %q", n)
	}

	db, _ := reg.Open("proj-sfacts")
	seedEvent(t, db, "sf-1") // user_request "req sf-1"
	if _, _, err := db.Remember("requests like sf-1 are handled by the frobnicator", "test",
		store.RememberOpts{Kind: "convention"}); err != nil {
		t.Fatal(err)
	}
	udb, _ := reg.Open("user")
	if _, _, err := udb.Remember("the user prefers sf-1 style summaries", "test",
		store.RememberOpts{Kind: "preference"}); err != nil {
		t.Fatal(err)
	}

	n := sessionFactsNotice()
	if !strings.Contains(n, "frobnicator") {
		t.Fatalf("project fact not injected:\n%s", n)
	}
	if !strings.Contains(n, "preference, user") {
		t.Fatalf("user-scope fact not injected with its label:\n%s", n)
	}
	if !strings.Contains(n, "verify before relying") {
		t.Fatalf("missing the verification framing:\n%s", n)
	}

	// A tiny budget trims rather than overflows.
	os.WriteFile(filepath.Join(proj, ".aimem.json"),
		[]byte(`{"project":"proj-sfacts","session_facts":5}`), 0o600)
	if small := sessionFactsNotice(); len(small) > 400 {
		t.Fatalf("budget not respected: %d bytes", len(small))
	}

	// Absent config = off, regardless of journal content.
	os.WriteFile(filepath.Join(proj, ".aimem.json"),
		[]byte(`{"project":"proj-sfacts"}`), 0o600)
	if n := sessionFactsNotice(); n != "" {
		t.Fatalf("opt-in not honored: %q", n)
	}
	if b := ident.SessionFactsBudget(proj); b != 0 {
		t.Fatalf("budget accessor: %d", b)
	}
}
