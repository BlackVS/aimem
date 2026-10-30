package mcp

import (
	"os"
	"testing"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/isolation"
	"aimem/internal/taskcred"
	"aimem/internal/textcheck"
)

// TestMain starts every test of the package without the developer's
// installation: an inherited AIMEM_SOCKET, AIMEM_STATE_DIR or
// XDG_RUNTIME_DIR outside the temporary directory is cleared, so the local
// socket and state root resolve inside each test's own temporary
// directory. A test that still resolves one outside it panics (package
// isolation).
func TestMain(m *testing.M) {
	isolation.ClearInherited()
	// The fake hubs answer on loopback, later than these bounds on a
	// loaded machine, which no test here is about.
	taskCheckTimeout = time.Minute
	adapter.PushTimeout = time.Minute
	taskcred.ClientTimeout = time.Minute
	os.Setenv(textcheck.ModelKey, "") // no real model behind an in-process hub
	os.Setenv(textcheck.FilterKey, "")
	os.Exit(m.Run())
}
