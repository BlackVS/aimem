package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/ident"
)

// TestDocsPullRefusesLocalChanges pins the feature's worst-possible-bug
// guard: a pull must never clobber unpublished local work without
// --force.
func TestDocsPullRefusesLocalChanges(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"name": "RUNBOOK", "body": "hub version\n", "rev": 5,
			"updated_at": "2026-08-29T00:00:00Z", "updated_by": "minis/cli",
		})
	}))
	defer hub.Close()
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{
		"home": {URL: hub.URL, Token: "t"},
	}, "home"); err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	os.WriteFile(filepath.Join(proj, ".aimem.json"),
		[]byte(`{"docs":["docs/RUNBOOK.md"]}`), 0o600)
	os.WriteFile(filepath.Join(proj, "docs", "RUNBOOK.md"),
		[]byte("local edit not on the hub\n"), 0o644)
	t.Chdir(proj)

	err := docsCmd([]string{"pull", "RUNBOOK"})
	if err == nil || !strings.Contains(err.Error(), "local changes") {
		t.Fatalf("pull clobber not refused: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(proj, "docs", "RUNBOOK.md")); string(got) != "local edit not on the hub\n" {
		t.Fatalf("local file was overwritten: %q", got)
	}

	// --force is the deliberate override: file replaced, sidecar synced.
	if err := docsCmd([]string{"pull", "RUNBOOK", "--force"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(proj, "docs", "RUNBOOK.md")); string(got) != "hub version\n" {
		t.Fatalf("forced pull did not write the hub copy: %q", got)
	}
	c := adapter.NewClient(root)
	pid, err := ident.ProjectID(proj)
	if err != nil {
		t.Fatal(err)
	}
	if rev := c.DocSyncRev(pid, "RUNBOOK"); rev != 5 {
		t.Fatalf("sidecar rev after pull: %d", rev)
	}
}

// TestDocsMergeThreeWay pins the merge flow: base from the sidecar rev,
// non-overlapping edits combine, and the sidecar lands on the hub's
// current rev so the resolved file pushes with a valid CAS base.
func TestDocsMergeThreeWay(t *testing.T) {
	const (
		baseBody = "a\nb\nc\nd\ne\n"
		hubBody  = "a\nb\nc\nd\nE-hub\n"   // hub changed the tail (rev 5)
		locBody  = "a\nB-local\nc\nd\ne\n" // local changed the head
	)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, rev := hubBody, 5
		if r.URL.Query().Get("rev") == "3" {
			body, rev = baseBody, 3
		}
		json.NewEncoder(w).Encode(map[string]any{
			"name": "RUNBOOK", "body": body, "rev": rev,
			"updated_at": "2026-08-29T00:00:00Z", "updated_by": "minis/cli",
		})
	}))
	defer hub.Close()
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{
		"home": {URL: hub.URL, Token: "t"},
	}, "home"); err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	os.WriteFile(filepath.Join(proj, ".aimem.json"),
		[]byte(`{"docs":["docs/RUNBOOK.md"]}`), 0o600)
	os.WriteFile(filepath.Join(proj, "docs", "RUNBOOK.md"), []byte(locBody), 0o644)
	t.Chdir(proj)
	c := adapter.NewClient(root)
	pid, err := ident.ProjectID(proj)
	if err != nil {
		t.Fatal(err)
	}
	// This machine last synced rev 3 - the base of the divergence.
	c.DocSyncRecord(pid, "RUNBOOK", 3, "stale")

	if err := docsCmd([]string{"merge", "RUNBOOK"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(proj, "docs", "RUNBOOK.md"))
	if string(got) != "a\nB-local\nc\nd\nE-hub\n" {
		t.Fatalf("clean merge result:\n%s", got)
	}
	// Sidecar rebased onto the hub's rev with the HUB body's hash, so
	// the merged file counts as a pending local change for push.
	if rev := c.DocSyncRev(pid, "RUNBOOK"); rev != 5 {
		t.Fatalf("sidecar rev after merge: %d", rev)
	}
	if c.DocSyncHash(pid, "RUNBOOK") != adapter.DocBodyHash([]byte(hubBody)) {
		t.Fatal("sidecar hash should be the hub body's, arming auto-publish")
	}

	// Overlapping edits: markers land in the file, and the sidecar hash
	// matches the WRITTEN file so nothing auto-publishes markers.
	os.WriteFile(filepath.Join(proj, "docs", "RUNBOOK.md"),
		[]byte("a\nb\nc\nd\nE-LOCAL\n"), 0o644)
	c.DocSyncRecord(pid, "RUNBOOK", 3, "stale")
	if err := docsCmd([]string{"merge", "RUNBOOK"}); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(proj, "docs", "RUNBOOK.md"))
	if !strings.Contains(string(got), "<<<<<<<") || !strings.Contains(string(got), "E-hub") ||
		!strings.Contains(string(got), "E-LOCAL") {
		t.Fatalf("conflict markers missing:\n%s", got)
	}
	if c.DocSyncHash(pid, "RUNBOOK") != adapter.DocBodyHash(got) {
		t.Fatal("sidecar hash must match the marker file (auto-publish stays quiet)")
	}

	// No divergence case: local based on the current rev refuses with a
	// pointer at push.
	os.WriteFile(filepath.Join(proj, "docs", "RUNBOOK.md"), []byte("edited\n"), 0o644)
	c.DocSyncRecord(pid, "RUNBOOK", 5, "whatever")
	if err := docsCmd([]string{"merge", "RUNBOOK"}); err == nil ||
		!strings.Contains(err.Error(), "just `aimem docs push") {
		t.Fatalf("no-divergence should point at push: %v", err)
	}
}
