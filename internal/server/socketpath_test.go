package server

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/store"
)

// The socket follows the installation (D-STORE): AIMEM_SOCKET wins; an
// explicit state root keeps its socket inside it, ahead of XDG_RUNTIME_DIR;
// without an explicit root nothing changes.
func TestSocketPathOrder(t *testing.T) {
	root, runtime, explicit := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "sock")
	for _, c := range []struct {
		name                     string
		socket, stateDir, xdgRun string
		want                     string
	}{
		{"AIMEM_SOCKET over an explicit root and the runtime dir", explicit, root, runtime, explicit},
		{"AIMEM_SOCKET alone", explicit, "", "", explicit},
		{"an explicit root over the runtime dir", "", root, runtime, filepath.Join(root, "aimem.sock")},
		{"an explicit root alone", "", root, "", filepath.Join(root, "aimem.sock")},
		{"the runtime dir without an explicit root (unchanged)", "", "", runtime, filepath.Join(runtime, "aimem.sock")},
		{"the default root last (unchanged)", "", "", "", filepath.Join(root, "aimem.sock")},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AIMEM_SOCKET", c.socket)
			t.Setenv("AIMEM_STATE_DIR", c.stateDir)
			t.Setenv("XDG_RUNTIME_DIR", c.xdgRun)
			if got := SocketPath(root); got != c.want {
				t.Fatalf("SocketPath = %q, want %q", got, c.want)
			}
		})
	}
}

// Two installations under one OS user, each named by its own process
// environment, get two sockets even where XDG_RUNTIME_DIR is shared.
func TestTwoInstallationsGetTwoSockets(t *testing.T) {
	t.Setenv("AIMEM_SOCKET", "")
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", a)
	sa := SocketPath(a)
	t.Setenv("AIMEM_STATE_DIR", b)
	sb := SocketPath(b)
	if sa == sb || filepath.Dir(sa) != a || filepath.Dir(sb) != b {
		t.Fatalf("two installations share or misplace a socket: %q %q", sa, sb)
	}
}

// A state root too deep for a Unix socket path fails with the length and the
// remedy, not a bare bind error.
func TestTooLongSocketPathNamesTheRemedy(t *testing.T) {
	deep := t.TempDir()
	for len(filepath.Join(deep, "aimem.sock")) <= maxSocketPath+8 {
		deep = filepath.Join(deep, strings.Repeat("d", 20))
	}
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEM_SOCKET", "")
	t.Setenv("AIMEM_STATE_DIR", deep)
	reg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	s := New(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { s.Close() })
	srv, ln, err := s.ListenAndServe(deep)
	if err == nil {
		srv.Close()
		ln.Close()
		t.Skip("this platform accepted a socket path over the portable limit")
	}
	if !strings.Contains(err.Error(), "set AIMEM_SOCKET") || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("error does not name the length and the remedy: %v", err)
	}
}
