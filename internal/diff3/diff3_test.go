package diff3

import (
	"strings"
	"testing"
)

func mergeT(t *testing.T, base, local, hub string) (string, int) {
	t.Helper()
	out, n, err := MergeText(base, local, hub, "local", "hub (rev 9)")
	if err != nil {
		t.Fatal(err)
	}
	return out, n
}

func TestCleanMergeDistinctRegions(t *testing.T) {
	base := "a\nb\nc\nd\ne\n"
	local := "a\nB-local\nc\nd\ne\n" // changed line 2
	hub := "a\nb\nc\nd\nE-hub\n"     // changed line 5
	out, n := mergeT(t, base, local, hub)
	if n != 0 || out != "a\nB-local\nc\nd\nE-hub\n" {
		t.Fatalf("clean merge failed: conflicts=%d\n%s", n, out)
	}
}

func TestConflictSameRegion(t *testing.T) {
	base := "a\nb\nc\n"
	local := "a\nB-local\nc\n"
	hub := "a\nB-hub\nc\n"
	out, n := mergeT(t, base, local, hub)
	if n != 1 {
		t.Fatalf("expected 1 conflict, got %d\n%s", n, out)
	}
	want := "a\n<<<<<<< local\nB-local\n=======\nB-hub\n>>>>>>> hub (rev 9)\nc\n"
	if out != want {
		t.Fatalf("conflict shape:\n%s", out)
	}
}

func TestIdenticalChangeIsNotAConflict(t *testing.T) {
	base := "a\nb\nc\n"
	same := "a\nB\nc\n"
	out, n := mergeT(t, base, same, same)
	if n != 0 || out != same {
		t.Fatalf("identical edits should merge silently: conflicts=%d\n%s", n, out)
	}
}

func TestInsertionsBothSides(t *testing.T) {
	base := "a\nb\n"
	local := "a\nlocal-add\nb\n" // insert after a
	hub := "a\nb\nhub-add\n"     // append at end
	out, n := mergeT(t, base, local, hub)
	if n != 0 || out != "a\nlocal-add\nb\nhub-add\n" {
		t.Fatalf("distinct insertions: conflicts=%d\n%s", n, out)
	}
	// Insertions at the SAME point are ambiguous - must conflict.
	local2 := "a\nX\nb\n"
	hub2 := "a\nY\nb\n"
	_, n = mergeT(t, base, local2, hub2)
	if n != 1 {
		t.Fatalf("same-point insertions must conflict, got %d", n)
	}
}

func TestDeletionVsEdit(t *testing.T) {
	base := "a\nb\nc\n"
	local := "a\nc\n"       // deleted b
	hub := "a\nB-hub\nc\n"  // edited b
	out, n := mergeT(t, base, local, hub)
	if n != 1 {
		t.Fatalf("delete-vs-edit must conflict, got %d\n%s", n, out)
	}
	// One-sided deletion merges cleanly.
	out, n = mergeT(t, base, local, base)
	if n != 0 || out != "a\nc\n" {
		t.Fatalf("clean deletion: conflicts=%d\n%s", n, out)
	}
}

func TestNoBaseChanges(t *testing.T) {
	base := "a\nb\n"
	if out, n := mergeT(t, base, base, base); n != 0 || out != base {
		t.Fatalf("identity merge: %d\n%s", n, out)
	}
	// Empty base (two-way degradation callers use): everything from both
	// sides lands, as one conflict when both wrote.
	_, n := mergeT(t, "", "mine\n", "theirs\n")
	if n != 1 {
		t.Fatalf("empty-base divergence should conflict, got %d", n)
	}
}

func TestTooLarge(t *testing.T) {
	big := strings.Repeat("x\n", MaxLines+1)
	if _, _, err := MergeText(big, big, big, "l", "h"); err == nil {
		t.Fatal("oversized input must refuse")
	}
}
