// Package isolation keeps test binaries away from a real aimem installation.
//
// On 2026-10-04 a throwaway service started for CLI evidence was stopped by
// process name, which also stopped the developer's installed service and
// every MCP server that used it. A test, or anything a test starts, must
// reach only what it created itself: its state root and its local socket
// in the temporary directory, and processes through the handles it holds.
// Require panics in a test binary when a resolved path lies outside the
// temporary directory; outside tests it does nothing.
package isolation

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Require panics, in a test binary only, when path is not inside the
// temporary directory. what names the path for the message ("state root",
// "local socket"), and fix says how to isolate it.
func Require(what, path, fix string) {
	if !testing.Testing() || UnderTemp(path) {
		return
	}
	panic("aimem: a test resolved the " + what + " outside the temporary directory (" + path + "); " + fix +
		" so the test cannot reach a real installation")
}

// UnderTemp reports whether path is the temporary directory or inside it.
func UnderTemp(path string) bool {
	tmp := filepath.Clean(os.TempDir())
	p := filepath.Clean(path)
	if !filepath.IsAbs(p) {
		return false
	}
	if p == tmp {
		return true
	}
	rel, err := filepath.Rel(tmp, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// inherited are the variables through which a test binary could inherit the
// developer's installation: its socket, its state root, and the runtime
// directory where a service's socket lives by default.
var inherited = []string{"AIMEM_SOCKET", "AIMEM_STATE_DIR", "XDG_RUNTIME_DIR"}

// ClearInherited unsets each of those variables that points outside the
// temporary directory. A package's TestMain calls it first, so its tests
// start with nothing of the installation; a helper process a test started
// keeps the temporary socket and root the test gave it.
func ClearInherited() {
	for _, k := range inherited {
		if v := os.Getenv(k); v != "" && !UnderTemp(v) {
			os.Unsetenv(k)
		}
	}
}
