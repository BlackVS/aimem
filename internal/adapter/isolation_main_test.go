package adapter

import (
	"os"
	"testing"
	"time"

	"aimem/internal/isolation"
)

// TestMain starts every test of the package without the developer's
// installation: an inherited AIMEM_SOCKET, AIMEM_STATE_DIR or
// XDG_RUNTIME_DIR outside the temporary directory is cleared, so the local
// socket and state root resolve inside each test's own temporary
// directory. A test that still resolves one outside it panics (package
// isolation). The fake hubs and services answer on loopback, later than
// the clients' bounds on a loaded machine, which no test here is about.
func TestMain(m *testing.M) {
	isolation.ClearInherited()
	PushTimeout = time.Minute
	localTimeout = time.Minute
	os.Exit(m.Run())
}
