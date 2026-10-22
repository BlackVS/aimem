package main

import (
	"io"
	"os"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/isolation"
)

// TestMain starts every test of the package without the developer's
// installation: an inherited AIMEM_SOCKET, AIMEM_STATE_DIR or
// XDG_RUNTIME_DIR outside the temporary directory is cleared, so the local
// socket and state root resolve inside each test's own temporary
// directory. A test that still resolves one outside it panics (package
// isolation). `aimem hub add` makes no identity call either, so a test that
// adds an example.com-style hub never reaches the network; the hub add
// warning tests restore it against their own httptest hubs.
func TestMain(m *testing.M) {
	isolation.ClearInherited()
	hubAddWarn = func(io.Writer, string, *adapter.HubConfig) {}
	os.Exit(m.Run())
}
