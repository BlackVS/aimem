package uuidv7

import (
	"regexp"
	"testing"
	"time"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewShapeAndMonotonicity(t *testing.T) {
	// The store relies on lexicographic order matching creation order
	// (ordering, latest-checkpoint queries, sync cursors) - so strict
	// monotonicity within a process is a contract, not a nicety.
	const n = 10000
	prev := ""
	seen := make(map[string]bool, n)
	for range n {
		id := New()
		if !uuidRe.MatchString(id) {
			t.Fatalf("not a v7 uuid: %q", id)
		}
		if id <= prev {
			t.Fatalf("not strictly increasing: %q after %q", id, prev)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
		prev = id
	}
}

func TestShiftBack(t *testing.T) {
	id := New()
	back := ShiftBack(id, time.Hour)
	if back == "" || !uuidRe.MatchString(back) {
		t.Fatalf("shifted id malformed: %q", back)
	}
	if back >= id {
		t.Fatalf("shift back did not lower the bound: %q >= %q", back, id)
	}
	// A shift past the epoch clamps to zero rather than wrapping.
	if z := ShiftBack(id, 100*365*24*time.Hour); !uuidRe.MatchString(z) ||
		z[:13] != "00000000-0000" {
		t.Fatalf("epoch clamp: %q", z)
	}
	// Garbage in, empty out - a cursor must never be fabricated.
	if ShiftBack("short", time.Hour) != "" || ShiftBack("zzzzzzzz-zzzz-7000", time.Hour) != "" {
		t.Fatal("malformed id produced a cursor")
	}
}
