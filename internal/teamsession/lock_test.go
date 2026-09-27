package teamsession

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestLockedExcludes: while one command holds the lock, another waits, and
// it runs as soon as the first lets go. Each takes the lock through its own
// open file, as separate processes do.
func TestLockedExcludes(t *testing.T) {
	root := t.TempDir()
	held, release, firstDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		firstDone <- Locked(root, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	var ran atomic.Bool
	secondDone := make(chan error, 1)
	go func() { secondDone <- Locked(root, func() error { ran.Store(true); return nil }) }()
	time.Sleep(300 * time.Millisecond)
	if ran.Load() {
		t.Fatal("a second command ran while the lock was held")
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil || !ran.Load() {
		t.Fatalf("the waiting command did not run once the lock was free: %v", err)
	}
	want := errors.New("from fn")
	if err := Locked(root, func() error { return want }); err != want {
		t.Fatalf("Locked returned %v, not fn's error", err)
	}
}
