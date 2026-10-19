// Package linktest makes the links the containment tests need: a
// directory link (a junction on Windows, which needs no privilege, a
// symbolic link elsewhere) and a file link (a symbolic link; the test is
// skipped where creating one needs a privilege the runner lacks). Only
// tests import it.
package linktest

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// Dir makes link a directory link to the existing directory target.
func Dir(t testing.TB, link, target string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err != nil {
			t.Fatalf("mklink /J %s %s: %v: %s", link, target, err, out)
		}
		return
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// File makes link a symbolic link to target, or skips the test where the
// platform refuses it without a privilege.
func File(t testing.TB, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("file symlinks need a privilege on this Windows runner: %v", err)
		}
		t.Fatal(err)
	}
}
