package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func openDocDB(t *testing.T) *DB {
	t.Helper()
	r := newTestRegistry(t)
	db, err := r.Open("proj-docs")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestDocCAS(t *testing.T) {
	db := openDocDB(t)

	// Create requires base_rev 0.
	if _, err := db.PutDoc("RUNBOOK", "v1", "minis/test", 3, false); err == nil {
		t.Fatal("creation with nonzero base_rev accepted")
	}
	d1, err := db.PutDoc("RUNBOOK", "v1", "minis/test", 0, false)
	if err != nil || d1.Rev != 1 {
		t.Fatalf("create: %+v, %v", d1, err)
	}

	// Normal CAS bump.
	d2, err := db.PutDoc("RUNBOOK", "v2", "dmbunker/test", 1, false)
	if err != nil || d2.Rev != 2 {
		t.Fatalf("update: %+v, %v", d2, err)
	}

	// Identical body: idempotent success at the current rev, ANY base_rev.
	d3, err := db.PutDoc("RUNBOOK", "v2", "dmbunker/test", 99, false)
	if err != nil || d3.Rev != 2 {
		t.Fatalf("identical-body retry not idempotent: %+v, %v", d3, err)
	}

	// Stale write: refused, and the conflict carries the current doc.
	_, err = db.PutDoc("RUNBOOK", "v2-from-elsewhere", "minis/test", 1, false)
	var conflict *DocConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("stale write returned %v; want *DocConflict", err)
	}
	if conflict.Current.Rev != 2 || conflict.Current.Body != "v2" || conflict.Current.UpdatedBy != "dmbunker/test" {
		t.Fatalf("conflict does not carry the current doc: %+v", conflict.Current)
	}

	got, err := db.GetDoc("RUNBOOK", 0)
	if err != nil || got.Body != "v2" || got.Rev != 2 {
		t.Fatalf("current doc wrong after refused write: %+v, %v", got, err)
	}
	// Historical revision is retrievable.
	old, err := db.GetDoc("RUNBOOK", 1)
	if err != nil || old.Body != "v1" {
		t.Fatalf("rev 1: %+v, %v", old, err)
	}
}

func TestDocTombstone(t *testing.T) {
	db := openDocDB(t)
	db.PutDoc("NOTES", "text", "a/test", 0, false)

	// Delete is CAS like any write.
	if _, err := db.PutDoc("NOTES", "", "a/test", 7, true); err == nil {
		t.Fatal("tombstone with stale base_rev accepted")
	}
	tomb, err := db.PutDoc("NOTES", "", "a/test", 1, true)
	if err != nil || !tomb.Deleted || tomb.Rev != 2 {
		t.Fatalf("tombstone: %+v, %v", tomb, err)
	}

	// A machine that still has the file conflicts by name, and the
	// conflict names the deletion.
	_, err = db.PutDoc("NOTES", "resurrected", "b/test", 1, false)
	var conflict *DocConflict
	if !errors.As(err, &conflict) || !conflict.Current.Deleted {
		t.Fatalf("push past tombstone: %v", err)
	}
	if !strings.Contains(err.Error(), "deleted") {
		t.Errorf("conflict message does not name the deletion: %v", err)
	}

	// Deliberate republish with the tombstone's rev as base succeeds.
	back, err := db.PutDoc("NOTES", "resurrected", "b/test", 2, false)
	if err != nil || back.Rev != 3 || back.Deleted {
		t.Fatalf("deliberate republish: %+v, %v", back, err)
	}
}

func TestDocLimitsAndValidation(t *testing.T) {
	db := openDocDB(t)
	if _, err := db.PutDoc("../escape", "x", "a", 0, false); err == nil {
		t.Error("path-shaped name accepted")
	}
	if _, err := db.PutDoc("BIG", strings.Repeat("x", MaxDocBytes+1), "a", 0, false); err == nil {
		t.Error("oversize document accepted")
	}
	key := "-----BEGIN RSA " + "PRIVATE KEY-----\nx\n-----END RSA " + "PRIVATE KEY-----"
	if _, err := db.PutDoc("LEAK", "here is a key\n"+key, "a", 0, false); err == nil {
		t.Error("private key block accepted")
	}
}

func TestDocHistoryBounded(t *testing.T) {
	db := openDocDB(t)
	db.PutDoc("H", "r1", "a", 0, false)
	for i := int64(1); i < 30; i++ {
		if _, err := db.PutDoc("H", fmt.Sprintf("r%d", i+1), "a", i, false); err != nil {
			t.Fatal(err)
		}
	}
	log, err := db.DocLog("H")
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != docHistoryKeep {
		t.Errorf("history holds %d revisions; want %d", len(log), docHistoryKeep)
	}
	if log[0].Rev != 30 {
		t.Errorf("newest retained rev = %d; want 30", log[0].Rev)
	}
}

func TestDocList(t *testing.T) {
	db := openDocDB(t)
	db.PutDoc("B", "bbbb", "a", 0, false)
	db.PutDoc("A", "aa", "a", 0, false)
	docs, err := db.ListDocs()
	if err != nil || len(docs) != 2 {
		t.Fatalf("list: %+v, %v", docs, err)
	}
	if docs[0].Name != "A" || docs[0].Size != 2 || docs[0].Body != "" {
		t.Errorf("list row wrong (bodies must be omitted, sizes kept): %+v", docs[0])
	}
}

func TestPutDocRefusesSecretShapes(t *testing.T) {
	db := openDocDB(t)
	// High-confidence shapes (recognised vendor token formats) refuse —
	// a pasted secret must not fan out to every machine on the project.
	// The fixture is split so source-scanners never see a token shape.
	body := "deploy uses " + strings.Join([]string{"gh", "p_abcdefghijklmnopqrstuvwxyz123456789"}, "")
	if _, err := db.PutDoc("RUNBOOK", body, "t", 0, false); err == nil ||
		!strings.Contains(err.Error(), "secret-shaped") {
		t.Fatalf("vendor token accepted: %v", err)
	}
	// Soft shapes publish (the client warns instead): authored prose
	// legitimately talks about passwords and tokens.
	if _, err := db.PutDoc("RUNBOOK", "reset flow: password = placeholder123", "t", 0, false); err != nil {
		t.Fatalf("soft match refused: %v", err)
	}
}

func TestSearchDocs(t *testing.T) {
	db := openDocDB(t)
	if _, err := db.PutDoc("RUNBOOK", "Step one: point the LiteLLM proxy at the hub.\nStep two: restart.", "t", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutDoc("NOTES", "Nothing about proxies here.", "t", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutDoc("OLD", "litellm ancient lore", "t", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutDoc("OLD", "", "t", 1, true); err != nil { // tombstoned
		t.Fatal(err)
	}
	// Case-insensitive, multi-term AND, snippet carries the hit.
	hits, err := db.SearchDocs("LiteLLM proxy", 0)
	if err != nil || len(hits) != 1 || hits[0].Name != "RUNBOOK" {
		t.Fatalf("search: %+v err=%v", hits, err)
	}
	if !strings.Contains(hits[0].Snippet, "LiteLLM proxy") {
		t.Fatalf("snippet missing hit: %q", hits[0].Snippet)
	}
	// Tombstoned docs never match; name-only matches work.
	if hits, _ := db.SearchDocs("ancient", 0); len(hits) != 0 {
		t.Fatalf("tombstoned doc matched: %+v", hits)
	}
	if hits, _ := db.SearchDocs("notes", 0); len(hits) != 1 || hits[0].Name != "NOTES" {
		t.Fatalf("name match: %+v", hits)
	}
	// A term absent everywhere finds nothing.
	if hits, _ := db.SearchDocs("proxy zeppelin", 0); len(hits) != 0 {
		t.Fatalf("AND semantics broken: %+v", hits)
	}
}
