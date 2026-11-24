package adapter

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// downURL is the address of a hub that refuses every connection.
func downURL(t *testing.T) string {
	t.Helper()
	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close()
	return url
}

// Runs that sweep one stale pending file at once queue its record once,
// on the hub path and the local path: the sweep runs only as the one run
// holding the flush lock, and a run that finds it held leaves the file
// alone.
func TestConcurrentSweepsQueueAPendingRecordOnce(t *testing.T) {
	defer func(d time.Duration) { pendingStale = d }(pendingStale)
	pendingStale = 0
	url := downURL(t)
	for _, local := range []bool{false, true} {
		t.Run(fmt.Sprint("local=", local), func(t *testing.T) {
			root, _ := hubRoot(t, url)
			spool := filepath.Join(root, "spool", "hub.jsonl")
			if local {
				spool = NewClient(root).spoolPath()
			}
			sweep := func() {
				c := NewClient(root).WithDeadline(time.Now().Add(5 * time.Second))
				if local {
					c.ReplaySpool() // no service here: the record is moved in and kept
				} else {
					c.sweepPending(spool)
				}
			}

			// A run holding the flush lock: the sweep leaves the file.
			if _, err := writePending(spool, rec("held")); err != nil {
				t.Fatal(err)
			}
			other, err := lockSpool(spool, "flush", 0)
			if err != nil {
				t.Fatal(err)
			}
			sweep()
			other.release()
			if got := queued(t, spool); len(got) != 0 {
				t.Fatalf("swept beside the lock's holder: %q", got)
			}
			sweep()

			const rounds = 20
			want := []string{"held"}
			for round := range rounds {
				key := fmt.Sprint("stale", round)
				want = append(want, key)
				if _, err := writePending(spool, rec(key)); err != nil {
					t.Fatal(err)
				}
				var wg sync.WaitGroup
				for range 8 {
					wg.Add(1)
					go func() { defer wg.Done(); sweep() }()
				}
				wg.Wait()
			}
			n := map[string]int{}
			for _, k := range queued(t, spool) {
				n[k]++
			}
			for _, k := range want {
				if n[k] != 1 {
					t.Errorf("record %s is queued %d time(s), want 1", k, n[k])
				}
			}
			if left := spoolSiblings(spool, ".pending-"); len(left) != 0 {
				t.Errorf("pending files left %v", left)
			}
		})
	}
}

// An append whose spool an older client's flush moves away on its retry
// too is an error, and the caller keeps the record: queue() in a pending
// file, the hub path in its own pending file.
func TestAnAppendMovedAwayTwiceKeepsTheRecord(t *testing.T) {
	defer func() { afterAppendWrite = nil }()
	url := downURL(t)
	for _, path := range []string{"queue", "pushHub"} {
		t.Run(path, func(t *testing.T) {
			root, _ := hubRoot(t, url)
			spool := filepath.Join(root, "spool", "hub.jsonl")
			appendLines(spool, rec("before"))
			moves := 0
			var renameErr error
			afterAppendWrite = func(sp string) {
				moves++
				if err := os.Rename(sp, fmt.Sprintf("%s.replay-%d", sp, 4240+moves)); err != nil && renameErr == nil {
					renameErr = err
				}
			}
			c := NewClient(root).WithDeadline(time.Now().Add(5 * time.Second))
			var err error
			if path == "queue" {
				err = c.queue(spool, rec("mine"))
			} else {
				c.pushHub("", rec("mine"))
			}
			afterAppendWrite = nil
			if err != nil {
				t.Fatalf("queue: %v", err)
			}
			if renameErr != nil {
				// Windows refuses the older client's rename while the
				// append holds the file: the record is in the spool, once.
				if got := queued(t, spool); !slices.Equal(got, []string{"before", "mine"}) {
					t.Fatalf("rename refused (%v); the spool holds %q", renameErr, got)
				}
				return
			}
			if moves != 2 {
				t.Fatalf("%d moves, want the append and its retry moved", moves)
			}
			pend := spoolSiblings(spool, ".pending-")
			if len(pend) != 1 {
				t.Fatalf("pending files %v, want the record kept in one", pend)
			}
			if b, _ := os.ReadFile(pend[0]); !bytes.Contains(b, rec("mine")) {
				t.Fatalf("the pending file holds %q", b)
			}
		})
	}
	// The error itself counts as busy, which every caller keeps its copy on.
	spool := filepath.Join(t.TempDir(), "spool", "hub.jsonl")
	appendLines(spool, rec("before"))
	moves := 0
	afterAppendWrite = func(sp string) {
		moves++
		os.Rename(sp, fmt.Sprintf("%s.replay-%d", sp, 4250+moves))
	}
	err := appendLines(spool, rec("mine"))
	afterAppendWrite = nil
	if moves == 2 && (!errors.Is(err, errMovedTwice) || !errors.Is(err, errQueueBusy)) {
		t.Fatalf("appendLines after two moves: %v", err)
	}
}

// A pending file is visible only with its record: it is written under a
// name no sweep matches and renamed. A temporary name a dead run left is
// moved into the spool by the pending sweep once older than pendingStale,
// and not before.
func TestAPendingFileIsVisibleOnlyWithItsRecord(t *testing.T) {
	defer func(d time.Duration) { pendingStale = d }(pendingStale)
	defer func() { beforePendingPublish = nil }()
	root := t.TempDir()
	spool := filepath.Join(root, "spool", "hub.jsonl")
	seen := false
	beforePendingPublish = func(tmp string) {
		seen = true
		if got := spoolSiblings(spool, ".pending-"); len(got) != 0 {
			t.Errorf("a pending file is visible before its rename: %v", got)
		}
		if b, _ := os.ReadFile(tmp); !bytes.Contains(b, rec("mine")) {
			t.Errorf("the temporary file holds %q", b)
		}
	}
	name, err := writePending(spool, rec("mine"))
	beforePendingPublish = nil
	if err != nil || !seen {
		t.Fatalf("writePending: %v (hook ran: %v)", err, seen)
	}
	if pend := spoolSiblings(spool, ".pending-"); !slices.Equal(pend, []string{name}) {
		t.Fatalf("pending files %v, want %s", pend, name)
	}
	if b, _ := os.ReadFile(name); !bytes.Contains(b, rec("mine")) {
		t.Fatalf("the pending file holds %q", b)
	}

	dead := filepath.Join(filepath.Dir(spool), pendingTempPrefix(spool)+"dead")
	os.WriteFile(dead, append(rec("dead"), '\n'), 0o600)
	c := NewClient(root).WithDeadline(time.Now().Add(5 * time.Second))
	pendingStale = time.Hour
	c.sweepPending(spool)
	if _, err := os.Stat(dead); err != nil || len(queued(t, spool)) != 0 {
		t.Fatalf("a fresh temporary name was swept: %v, queued %q", err, queued(t, spool))
	}
	pendingStale = 0
	c.sweepPending(spool)
	got := queued(t, spool)
	slices.Sort(got)
	if !slices.Equal(got, []string{"dead", "mine"}) {
		t.Fatalf("queued %q, want the dead run's record and the pending one", got)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatalf("the dead run's temporary name is still there: %v", err)
	}
}

// On a filesystem that refuses the queue's lock, a run that only appends
// (the local service down) says so in its line, once however many records
// it queues, and its records are queued.
func TestARunOnALocklessFilesystemSaysSoOnce(t *testing.T) {
	defer func(f func(*os.File) (bool, error)) { tryLock = f }(tryLock)
	tryLock = func(*os.File) (bool, error) { return false, errors.New("locks refused here") }
	root, p := hubRoot(t, downURL(t))
	c := NewClient(root).WithDeadline(time.Now().Add(5 * time.Second))
	c.Submit(p) // the local post fails: the local spool, then the hub's
	c.spool(rec("second"))
	n := 0
	for _, w := range c.deferredList {
		if w == noLocksLine {
			n++
		} else if strings.Contains(w, "lock") {
			t.Errorf("another line names the lock: %q", w)
		}
	}
	if n != 1 {
		t.Fatalf("the run's line names the refused lock %d time(s): %q", n, c.deferredList)
	}
	if b, _ := os.ReadFile(c.spoolPath()); bytes.Count(b, []byte{'\n'}) != 2 || !bytes.Contains(b, rec("second")) {
		t.Fatalf("the local spool holds %q, want the submit's record and the second", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "spool", "hub.jsonl")); bytes.Count(b, []byte{'\n'}) != 1 {
		t.Fatalf("the hub spool holds %q, want the submit's record", b)
	}
}
