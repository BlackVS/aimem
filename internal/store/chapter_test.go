package store

import (
	"strings"
	"testing"
)

// A fact lives in exactly one knowledge-base chapter: the first filing
// wins, later chapter tags (dedup/reassert merges from cross-filed
// twins) are dropped, and untag is the correction path.
func TestSingleChapterInvariant(t *testing.T) {
	reg, _ := NewRegistry(t.TempDir())
	defer reg.Close()
	db, _ := reg.Open("p")
	id, _, err := db.Remember("fact one", "t", RememberOpts{Tags: []string{"chapter:ci", "x"}})
	if err != nil {
		t.Fatal(err)
	}
	// Reassert with a different chapter: merged tag must be dropped.
	if _, re, err := db.Remember("fact one", "t", RememberOpts{Tags: []string{"chapter:evidence", "y"}}); err != nil || !re {
		t.Fatalf("reassert: %v %v", re, err)
	}
	mems, _ := db.Memories(false)
	if len(mems) != 1 {
		t.Fatalf("memories: %d", len(mems))
	}
	var chapters, plain int
	for _, tag := range mems[0].Tags {
		if strings.HasPrefix(tag, "chapter:") {
			chapters++
		} else {
			plain++
		}
	}
	if chapters != 1 || plain != 2 {
		t.Fatalf("tags after merge: %v", mems[0].Tags)
	}
	// Correction path: untag then reassert files it elsewhere.
	if err := db.RemoveTag(id, "chapter:ci"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.Remember("fact one", "t", RememberOpts{Tags: []string{"chapter:evidence"}}); err != nil {
		t.Fatal(err)
	}
	mems, _ = db.Memories(false)
	found := false
	for _, tag := range mems[0].Tags {
		if tag == "chapter:evidence" {
			found = true
		}
		if tag == "chapter:ci" {
			t.Fatal("old chapter tag survived untag")
		}
	}
	if !found {
		t.Fatalf("re-filing failed: %v", mems[0].Tags)
	}
	if err := db.RemoveTag(id, "chapter:none"); err == nil {
		t.Fatal("removing a missing tag succeeded")
	}
}
