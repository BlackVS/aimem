package filelock

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTryLockExcludesAnotherHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	open := func() *os.File {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	a, b := open(), open()
	if ok, err := TryLock(a); !ok || err != nil {
		t.Fatalf("first lock: %v %v", ok, err)
	}
	if ok, err := TryLock(b); ok || err != nil {
		t.Fatalf("second handle took a held lock: %v %v", ok, err)
	}
	if err := Unlock(a); err != nil {
		t.Fatal(err)
	}
	if ok, err := TryLock(b); !ok || err != nil {
		t.Fatalf("lock not free after unlock: %v %v", ok, err)
	}
	// Closing the holder releases the lock, as the OS does at exit.
	b.Close()
	if ok, err := TryLock(a); !ok || err != nil {
		t.Fatalf("lock not free after the holder closed: %v %v", ok, err)
	}
}
