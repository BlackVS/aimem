package adapter

import (
	"os"
	"path/filepath"
	"testing"
)

// StateRoot is the client state root, where the hub entries and their
// tokens live: AIMEM_STATE_DIR, else $XDG_STATE_HOME/aimem, else
// ~/.local/state/aimem.
//
// In a test binary it fails closed: without AIMEM_STATE_DIR it panics
// instead of falling back, so no test can resolve the developer's real hub
// entries and write to a real hub with a real token. XDG_STATE_HOME does not
// count as isolation there, since a developer's shell may set it for real.
func StateRoot() string {
	if v := os.Getenv("AIMEM_STATE_DIR"); v != "" {
		return v
	}
	if testing.Testing() {
		panic("aimem: a test resolved the client state root without AIMEM_STATE_DIR; " +
			"set it to a t.TempDir() (t.Setenv) so the test cannot reach a real hub")
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "aimem")
}
