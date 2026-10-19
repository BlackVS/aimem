package store

import (
	"slices"
	"strings"
	"testing"
)

func TestValidMemoryID(t *testing.T) {
	for _, id := range []string{"01a150e8-6560-7000-9318-25013f9bc20b", "m1", "A_b-9"} {
		if !ValidMemoryID(id) {
			t.Errorf("%q refused", id)
		}
	}
	for _, id := range []string{"", "-x", "_x", "x'y", `x"y`, "x<y", "x y", "x/y", "x\ny", strings.Repeat("a", 65)} {
		if ValidMemoryID(id) {
			t.Errorf("%q accepted", id)
		}
	}
}

func TestCheckImportedMemory(t *testing.T) {
	for _, m := range []Memory{
		{ID: `x'),alert(1)//`},
		{ID: "ok-id", SupersededBy: `y"z`},
	} {
		if CheckImportedMemory(&m) == nil {
			t.Errorf("id %q superseded_by %q accepted", m.ID, m.SupersededBy)
		}
	}
	if err := CheckImportedMemory(&Memory{ID: "01a150e8-6560-7000-9318-25013f9bc20b"}); err != nil {
		t.Errorf("UUIDv7 id refused: %v", err)
	}
}

// ImportMemory itself does not apply the import rule: a project merge replays
// rows the store already holds, and drops a row that fails to import.
func TestStoreImportKeepsAnIDOutsideTheImportRule(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	m := Memory{ID: "legacy.id:1", Text: "an old fact", CreatedAt: "2026-10-19T00:00:00Z"}
	if err := db.ImportMemory(&m); err != nil {
		t.Fatalf("store-level import refused a legacy id: %v", err)
	}
}

// Stripping applies to tags being written, not to the identity a removal
// matches: a fact holding a legacy "owner's" beside "owners" loses exactly
// the one asked for.
func TestRemoveTagMatchesALegacySpellingExactly(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	id, _, err := db.Remember("a fact", "t", RememberOpts{Tags: []string{"owners"}})
	if err != nil {
		t.Fatal(err)
	}
	// A tag stored before the write rule existed.
	if _, err := db.sql.Exec(`INSERT INTO memory_tags(memory_id, tag) VALUES(?,?)`, id, "owner's"); err != nil {
		t.Fatal(err)
	}
	if err := db.RemoveTag(id, "owner's"); err != nil {
		t.Fatalf("removing the legacy spelling: %v", err)
	}
	mems, _ := db.Memories(true)
	if len(mems) != 1 || !slices.Equal(mems[0].Tags, []string{"owners"}) {
		t.Fatalf("tags after removing owner's: %+v, want only owners", mems)
	}
}

// Tags and sources are identifiers: control and markup characters are
// dropped on every write path, so none reaches a page.
func TestTagsAndSourcesDropMarkup(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	id, _, err := db.Remember("a fact", "t", RememberOpts{
		Tags:    []string{`Topic'"<x>` + "`\\\x07"},
		Sources: []string{`project:a'b<c>`},
	})
	if err != nil {
		t.Fatal(err)
	}
	m := Memory{ID: "m2", Text: "imported", CreatedAt: "2026-10-19T00:00:00Z",
		Tags: []string{"chapter:a'b"}, Sources: []string{`ev"1`}}
	if err := db.ImportMemory(&m); err != nil {
		t.Fatal(err)
	}
	mems, _ := db.Memories(true)
	byID := map[string]Memory{}
	for _, x := range mems {
		byID[x.ID] = x
	}
	if got := byID[id]; !slices.Equal(got.Tags, []string{"topicx"}) || !slices.Equal(got.Sources, []string{"project:abc"}) {
		t.Errorf("remember kept markup: tags %q sources %q", got.Tags, got.Sources)
	}
	if got := byID["m2"]; !slices.Equal(got.Tags, []string{"chapter:ab"}) || !slices.Equal(got.Sources, []string{"ev1"}) {
		t.Errorf("import kept markup: tags %q sources %q", got.Tags, got.Sources)
	}
}
