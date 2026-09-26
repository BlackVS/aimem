//go:build !windows

package privatefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateFileModes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "secret")
	f, err := Create(p)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("created mode %v: %v", fi.Mode().Perm(), err)
	}
	if err := Check(p); err != nil {
		t.Fatalf("private file refused: %v", err)
	}
	if _, err := Create(p); err == nil {
		t.Fatal("an existing file was reopened instead of refused")
	}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o660} {
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		if err := Check(p); err == nil {
			t.Errorf("mode %04o accepted", mode)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(p, link); err == nil {
		if err := Check(link); err == nil {
			t.Error("a symlink was accepted as the secret file")
		}
	}
}
