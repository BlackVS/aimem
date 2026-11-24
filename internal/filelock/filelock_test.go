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

// A shared lock is held by any number of handles at once, never beside an
// exclusive one (either way round), and goes when its holder closes.
func TestTryLockSharedBesideSharedNotExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".lock")
	open := func() *os.File {
		f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	a, b, x := open(), open(), open()
	if ok, err := TryLockShared(a); !ok || err != nil {
		t.Fatalf("first shared lock: %v %v", ok, err)
	}
	if ok, err := TryLockShared(b); !ok || err != nil {
		t.Fatalf("a second shared lock beside the first: %v %v", ok, err)
	}
	if ok, err := TryLock(x); ok || err != nil {
		t.Fatalf("an exclusive lock beside shared ones: %v %v", ok, err)
	}
	if err := Unlock(a); err != nil {
		t.Fatal(err)
	}
	if ok, err := TryLock(x); ok || err != nil {
		t.Fatalf("an exclusive lock beside the remaining shared one: %v %v", ok, err)
	}
	// Closing the last shared holder releases it.
	b.Close()
	if ok, err := TryLock(x); !ok || err != nil {
		t.Fatalf("exclusive lock not free after the shared holders went: %v %v", ok, err)
	}
	c := open()
	if ok, err := TryLockShared(c); ok || err != nil {
		t.Fatalf("a shared lock beside an exclusive one: %v %v", ok, err)
	}
	if err := Unlock(x); err != nil {
		t.Fatal(err)
	}
	if ok, err := TryLockShared(c); !ok || err != nil {
		t.Fatalf("shared lock not free after the exclusive unlock: %v %v", ok, err)
	}
}
