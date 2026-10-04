package isolation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnderTemp(t *testing.T) {
	tmp := os.TempDir()
	for _, p := range []string{tmp, filepath.Join(tmp, "a"), filepath.Join(tmp, "a", "b", "aimem.sock"), t.TempDir()} {
		if !UnderTemp(p) {
			t.Errorf("%s: not under %s", p, tmp)
		}
	}
	outside := filepath.Join(filepath.Dir(filepath.Clean(tmp)), "elsewhere")
	for _, p := range []string{"", "relative/aimem.sock", outside, filepath.Join(tmp, "..", "x"), tmp + "-sibling"} {
		if UnderTemp(p) {
			t.Errorf("%q counted as under %s", p, tmp)
		}
	}
}

func TestRequirePanicsOutsideTemp(t *testing.T) {
	Require("local socket", filepath.Join(t.TempDir(), "aimem.sock"), "fix") // no panic
	outside := filepath.Join(filepath.Dir(filepath.Clean(os.TempDir())), "real", "aimem.sock")
	defer func() {
		r := recover()
		if msg, _ := r.(string); !strings.Contains(msg, "local socket outside the temporary directory") || !strings.Contains(msg, "set X") {
			t.Fatalf("panic: %v", r)
		}
	}()
	Require("local socket", outside, "set X")
	t.Fatal("no panic for a path outside the temporary directory")
}

// ClearInherited drops what points at an installation and keeps what a
// test set inside the temporary directory.
func TestClearInherited(t *testing.T) {
	outside := filepath.Join(filepath.Dir(filepath.Clean(os.TempDir())), "real")
	inside := t.TempDir()
	t.Setenv("AIMEM_SOCKET", filepath.Join(outside, "aimem.sock"))
	t.Setenv("XDG_RUNTIME_DIR", outside)
	t.Setenv("AIMEM_STATE_DIR", inside)
	ClearInherited()
	for _, k := range []string{"AIMEM_SOCKET", "XDG_RUNTIME_DIR"} {
		if _, set := os.LookupEnv(k); set {
			t.Errorf("%s still set", k)
		}
	}
	if os.Getenv("AIMEM_STATE_DIR") != inside {
		t.Errorf("a temporary state root was cleared")
	}
}
