package main

import (
	"slices"
	"testing"

	"aimem/internal/store"
)

// The sync partition: each hub receives its bound projects, the groups
// those projects declare, and the user DB — nothing of another hub's.
func TestHubProjectsPartition(t *testing.T) {
	reg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()

	seed := func(id, hub, groups string) {
		t.Helper()
		db, err := reg.Open(id)
		if err != nil {
			t.Fatal(err)
		}
		if hub != "-" {
			if err := db.SetMeta("hub", hub); err != nil {
				t.Fatal(err)
			}
		}
		if groups != "" {
			if err := db.SetMeta("groups", groups); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("work-a", "", `["group-ai-infra"]`) // unbound -> default hub
	seed("work-b", "-", "")                  // no meta at all -> default hub
	// A group this machine only reads: declared, with no local store. A
	// malformed entry is never listed.
	seed("work-c", "", `["group-read-only","group-../x","plain"]`)
	seed("home-a", "home", `["group-family"]`)
	// group-stray has a store that no bound project declares.
	for _, g := range []string{"group-ai-infra", "group-family", "group-stray"} {
		if _, err := reg.Open(g); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reg.Open("user"); err != nil {
		t.Fatal(err)
	}

	got, err := hubProjects(reg, "work", "work") // the default hub
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"group-ai-infra", "group-read-only", "user", "work-a", "work-b", "work-c"}
	if !slices.Equal(got, want) {
		t.Fatalf("default hub partition: got %v want %v", got, want)
	}

	got, err = hubProjects(reg, "home", "work")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"group-family", "home-a", "user"}
	if !slices.Equal(got, want) {
		t.Fatalf("home hub partition: got %v want %v", got, want)
	}
}

func TestFilterProjects(t *testing.T) {
	ids := []string{"a", "b", "c"}
	if got := filterProjects(ids, ""); !slices.Equal(got, ids) {
		t.Fatalf("no filter: %v", got)
	}
	if got := filterProjects(ids, "c, a ,nope"); !slices.Equal(got, []string{"a", "c"}) {
		t.Fatalf("filtered: %v", got)
	}
}
