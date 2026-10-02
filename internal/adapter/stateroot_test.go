package adapter

import (
	"strings"
	"testing"
)

// The guard: a test that reaches the state root without an isolated
// AIMEM_STATE_DIR fails instead of resolving the developer's real hub
// entries, whatever XDG_STATE_HOME says.
func TestStateRootFailsClosedInTests(t *testing.T) {
	t.Setenv("AIMEM_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	defer func() {
		r := recover()
		msg, _ := r.(string)
		if !strings.Contains(msg, "without AIMEM_STATE_DIR") {
			t.Fatalf("StateRoot did not fail closed: recovered %v", r)
		}
	}()
	root := StateRoot()
	t.Fatalf("StateRoot returned %q in a test without AIMEM_STATE_DIR", root)
}

func TestStateRootUsesTheIsolatedDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", dir)
	if got := StateRoot(); got != dir {
		t.Fatalf("StateRoot = %q, want %q", got, dir)
	}
}
