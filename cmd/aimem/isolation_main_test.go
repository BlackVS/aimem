package main

import (
	"io"
	"os"
	"testing"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/isolation"
	"aimem/internal/provider"
	"aimem/internal/taskcred"
	"aimem/internal/textcheck"
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
	os.Setenv(textcheck.ModelKey, "") // no real model behind an in-process hub
	os.Setenv(textcheck.FilterKey, "")
	os.Setenv(provider.AllowPrivateEnv, "1") // the fake model servers listen on loopback
	hubAddWarn = func(io.Writer, string, *adapter.HubConfig) {}
	// The fake hubs answer on loopback, later than these bounds on a
	// loaded machine, which no test here is about.
	adapter.PushTimeout = time.Minute
	sourceTimeout = time.Minute
	taskcred.ClientTimeout = time.Minute
	os.Exit(m.Run())
}
