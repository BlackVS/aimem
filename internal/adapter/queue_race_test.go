package adapter

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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

// A sweep never takes a pending file its writer has not finished: the
// writer holds the pending lock shared from before the file's creation to
// after its close, and the sweep lists the files holding it exclusive. A
// sweep in either window (the file empty, then written but open) leaves
// the file, however old it looks, and a later sweep moves it in once.
func TestASweepNeverTakesAPendingFileItsWriterHolds(t *testing.T) {
	defer func(d time.Duration) { pendingStale = d }(pendingStale)
	defer func() { duringPendingWrite = nil }()
	pendingStale = 0 // every file looks stale: only the lock keeps the sweep off
	root := t.TempDir()
	spool := filepath.Join(root, "spool", "hub.jsonl")
	sweep := func() { NewClient(root).WithDeadline(time.Now().Add(5 * time.Second)).sweepPending(spool) }
	var stages []bool
	duringPendingWrite = func(path string, written bool) {
		stages = append(stages, written)
		sweep()
		if got := queued(t, spool); len(got) != 0 {
			t.Errorf("a sweep took a file its writer holds (written %v): queued %q", written, got)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("a sweep removed a file its writer holds (written %v): %v", written, err)
		}
	}
	name, err := writePending(spool, rec("mine"))
	duringPendingWrite = nil
	if err != nil || !slices.Equal(stages, []bool{false, true}) {
		t.Fatalf("writePending: %v (hook stages %v)", err, stages)
	}
	if b, _ := os.ReadFile(name); !bytes.Contains(b, rec("mine")) {
		t.Fatalf("the pending file holds %q", b)
	}
	sweep()
	if got := queued(t, spool); !slices.Equal(got, []string{"mine"}) {
		t.Fatalf("queued %q after the writer finished, want the record once", got)
	}
	if left := spoolSiblings(spool, ".pending-"); len(left) != 0 {
		t.Fatalf("pending files left %v", left)
	}
}

// A writer hung while holding the pending lock holds off pending sweeps
// only: they leave every pending file, quietly and at once, while appends
// and the flush go on.
func TestAHungPendingWriterHoldsOffOnlyTheSweep(t *testing.T) {
	defer func(d time.Duration) { pendingStale = d }(pendingStale)
	pendingStale = 0
	var got []string
	var mu sync.Mutex
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(b))
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer hub.Close()
	root, _ := hubRoot(t, hub.URL)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	stale, err := writePending(spool, rec("stale"))
	if err != nil {
		t.Fatal(err)
	}
	hung, err := lockSpoolShared(spool, "pending", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer hung.release()

	c := NewClient(root).WithDeadline(time.Now().Add(5 * time.Second))
	start := time.Now()
	c.sweepPending(spool)
	if took := time.Since(start); took > appendLockWait {
		t.Errorf("the sweep waited %v for a hung writer", took)
	}
	if _, err := os.Stat(stale); err != nil || len(queued(t, spool)) != 0 {
		t.Fatalf("a sweep moved a pending file beside a writer's lock: %v, queued %q", err, queued(t, spool))
	}
	if len(c.deferredList) != 0 {
		t.Errorf("a sweep held off by a writer named it: %q", c.deferredList)
	}
	beside, err := writePending(spool, rec("beside"))
	if err != nil {
		t.Fatalf("a second writer beside a hung one: %v", err)
	}
	if err := appendLines(spool, rec("appended")); err != nil {
		t.Fatalf("an append beside a hung pending writer: %v", err)
	}
	hubCfg := &HubConfig{URL: hub.URL, Token: "t"}
	c.flushHubSpool(hubCfg, spool)
	if q := queued(t, spool); len(q) != 0 {
		t.Fatalf("the flush beside a hung pending writer left %q", q)
	}
	mu.Lock()
	delivered := strings.Join(got, "")
	mu.Unlock()
	if !strings.Contains(delivered, string(rec("appended"))) {
		t.Fatalf("the flush did not deliver the appended record: %q", delivered)
	}

	hung.release()
	left := spoolSiblings(spool, ".pending-")
	want := []string{stale, beside}
	slices.Sort(left)
	slices.Sort(want)
	if !slices.Equal(left, want) {
		t.Fatalf("pending files %v, want %v", left, want)
	}
}

// An older client's sweep takes no lock and may remove a pending file of
// this release while it is still empty. The writer finds its name gone
// after the close and writes the record once more, under a fresh name; a
// second removal is an error, so the caller keeps the record by its other
// means. Windows refuses the removal of a file still open, so there the
// first file is kept.
func TestAPendingFileAnOlderSweepRemovedIsWrittenAgain(t *testing.T) {
	defer func() { duringPendingWrite = nil }()
	for _, removals := range []int{1, 2} {
		t.Run(fmt.Sprint("removals=", removals), func(t *testing.T) {
			spool := filepath.Join(t.TempDir(), "spool", "hub.jsonl")
			removed := 0
			duringPendingWrite = func(path string, written bool) {
				if !written && removed < removals && os.Remove(path) == nil {
					removed++
				}
			}
			name, err := writePending(spool, rec("mine"))
			duringPendingWrite = nil
			if removed == 2 {
				if !errors.Is(err, errPendingTaken) {
					t.Fatalf("writePending after two removals: %q, %v", name, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(name); !bytes.Equal(bytes.TrimSpace(b), rec("mine")) {
				t.Fatalf("the pending file holds %q (removed %d)", b, removed)
			}
			if left := spoolSiblings(spool, ".pending-"); !slices.Equal(left, []string{name}) {
				t.Fatalf("pending files %v, want %s", left, name)
			}
		})
	}
}

// Pending files are created exclusively under names of their own, never
// renamed: many writers at once each keep their own record whole.
func TestPendingFilesNeverReplaceOneAnother(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "spool", "hub.jsonl")
	const n = 64
	names := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name, err := writePending(spool, rec(fmt.Sprint("r", i)))
			if err != nil {
				t.Error(err)
			}
			names[i] = name
		}()
	}
	wg.Wait()
	for i, name := range names {
		b, err := os.ReadFile(name)
		if err != nil || !bytes.Equal(bytes.TrimSpace(b), rec(fmt.Sprint("r", i))) {
			t.Fatalf("pending file %d (%s) holds %q (%v)", i, name, b, err)
		}
	}
	if left := spoolSiblings(spool, ".pending-"); len(left) != n {
		t.Fatalf("%d pending files, want %d", len(left), n)
	}
}

// On a filesystem that refuses locks the pending file is written without
// the lock, as no sweep runs there.
func TestAPendingFileOnALocklessFilesystemIsWritten(t *testing.T) {
	defer func(f func(*os.File) (bool, error)) { tryLockShared = f }(tryLockShared)
	tryLockShared = func(*os.File) (bool, error) { return false, errors.New("locks refused here") }
	spool := filepath.Join(t.TempDir(), "spool", "hub.jsonl")
	name, err := writePending(spool, rec("mine"))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(name); !bytes.Contains(b, rec("mine")) {
		t.Fatalf("the pending file holds %q", b)
	}
}

// A standalone replay (aimem spool-flush) on a lockless filesystem keeps
// the line for its caller, which writes no event line, to print.
func TestAReplayOnALocklessFilesystemLeavesTheLineForItsCaller(t *testing.T) {
	defer func(f func(*os.File) (bool, error)) { tryLock = f }(tryLock)
	tryLock = func(*os.File) (bool, error) { return false, errors.New("locks refused here") }
	c := NewClient(t.TempDir()).WithDeadline(time.Now().Add(5 * time.Second))
	if err := os.MkdirAll(filepath.Dir(c.spoolPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(c.spoolPath(), append(rec("kept"), '\n'), 0o600)
	if _, _, err := c.ReplaySpool(); err != nil {
		t.Fatalf("ReplaySpool: %v", err)
	}
	if left := c.LeftForLater(); !slices.Contains(left, noLocksLine) {
		t.Fatalf("LeftForLater %q lacks the refused lock", left)
	}
}

// One client importing many records on a lockless filesystem names the
// refused lock once, in the first record's line (an import submits each
// record on one client, and each writes its own line).
func TestAnImportOnALocklessFilesystemSaysSoOnce(t *testing.T) {
	defer func(f func(*os.File) (bool, error)) { tryLock = f }(tryLock)
	tryLock = func(*os.File) (bool, error) { return false, errors.New("locks refused here") }
	root := t.TempDir()
	c := NewClient(root).WithDeadline(time.Now().Add(5 * time.Second))
	p := &Payload{ProjectID: "proj-b"}
	p.Event.Client, p.Event.SessionID, p.Event.Kind = "codex", "s1", "turn"
	for _, turn := range []string{"t1", "t2", "t3"} {
		p.Event.TurnID, p.Event.IdempotencyKey = turn, "codex:s1:"+turn
		if _, err := c.SubmitLocal(p); err != nil {
			t.Fatal(err)
		}
	}
	notes, _ := os.ReadFile(filepath.Join(root, "adapter.log"))
	if n := bytes.Count(notes, []byte(noLocksLine)); n != 1 {
		t.Fatalf("adapter.log names the refused lock %d time(s), want 1:\n%s", n, notes)
	}
	if b, _ := os.ReadFile(c.spoolPath()); bytes.Count(b, []byte{'\n'}) != 3 {
		t.Fatalf("the local spool holds %q, want three records", b)
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
	notes, _ := os.ReadFile(filepath.Join(root, "adapter.log"))
	if n := bytes.Count(notes, []byte(noLocksLine)); n != 1 {
		t.Fatalf("adapter.log names the refused lock %d time(s), want 1:\n%s", n, notes)
	}
	if bytes.Count(notes, []byte("lock")) != bytes.Count(notes, []byte(noLocksLine)) {
		t.Errorf("another line names the lock:\n%s", notes)
	}
	if slices.Contains(c.deferredList, noLocksLine) {
		t.Errorf("the client names the refused lock again after its line: %q", c.deferredList)
	}
	if b, _ := os.ReadFile(c.spoolPath()); bytes.Count(b, []byte{'\n'}) != 2 || !bytes.Contains(b, rec("second")) {
		t.Fatalf("the local spool holds %q, want the submit's record and the second", b)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "spool", "hub.jsonl")); bytes.Count(b, []byte{'\n'}) != 1 {
		t.Fatalf("the hub spool holds %q, want the submit's record", b)
	}
}

// writePending is writePendingFile for tests that need only the path.
func writePending(spool string, body []byte) (string, error) {
	p, err := writePendingFile(spool, body)
	if p == nil {
		return "", err
	}
	return p.path, err
}

// A local submit whose spool and pending lock are both held past their
// waits (two runs stopped while holding them) still keeps its record, in
// a pending file written without the lock, which the sweep's age rule
// keeps off while fresh and moves in once stale.
func TestASubmitBesideTwoHeldLocksKeepsItsRecord(t *testing.T) {
	defer func(d time.Duration) { pendingStale = d }(pendingStale)
	root := t.TempDir()
	c := NewClient(root).WithDeadline(time.Now().Add(5 * time.Second))
	spool := c.spoolPath()
	appender, err := lockSpool(spool, "append", 0)
	if err != nil {
		t.Fatal(err)
	}
	sweeper, err := lockSpool(spool, "pending", 0)
	if err != nil {
		t.Fatal(err)
	}
	p := &Payload{ProjectID: "proj-b"}
	p.Event.Client, p.Event.SessionID, p.Event.TurnID, p.Event.Kind = "codex", "s1", "t1", "turn"
	p.Event.IdempotencyKey = "codex:s1:t1"
	if _, err := c.SubmitLocal(p); err != nil {
		t.Fatalf("SubmitLocal beside two held locks: %v", err)
	}
	for _, w := range c.deferredList {
		if strings.Contains(w, "LOST") {
			t.Fatalf("the run names a loss: %q", c.deferredList)
		}
	}
	pend := spoolSiblings(spool, ".pending-")
	if len(pend) != 1 {
		t.Fatalf("pending files %v, want one", pend)
	}
	if b, _ := os.ReadFile(pend[0]); !bytes.Contains(b, []byte(`"codex:s1:t1"`)) {
		t.Fatalf("the pending file holds %q", b)
	}
	sweeper.release()
	appender.release()
	pendingStale = time.Hour
	NewClient(root).WithDeadline(time.Now().Add(5 * time.Second)).sweepPending(spool)
	if q := queued(t, spool); len(q) != 0 {
		t.Fatalf("a fresh pending file was swept: %q", q)
	}
	pendingStale = 0
	NewClient(root).WithDeadline(time.Now().Add(5 * time.Second)).sweepPending(spool)
	if b, _ := os.ReadFile(spool); bytes.Count(b, []byte(`"codex:s1:t1"`)) != 1 {
		t.Fatalf("the spool holds %q, want the record once", b)
	}
}

// A run removes only the pending file it wrote: after a sweep moved its
// record in, another run's file at the same name stays. Names carry the
// writer's pid and a nanosecond clock, so two writes never share one.
func TestARunRemovesOnlyItsOwnPendingFile(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "spool", "hub.jsonl")
	a, err := writePendingFile(spool, rec("a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := writePendingFile(spool, rec("b"))
	if err != nil {
		t.Fatal(err)
	}
	if a.path == b.path || !strings.Contains(filepath.Base(a.path), fmt.Sprintf(".pending-%d-", os.Getpid())) {
		t.Fatalf("pending names %s and %s", a.path, b.path)
	}
	// A sweep moved A in and removed it; B's run got A's former name.
	if err := os.Remove(a.path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.path, append(rec("other"), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	removePending(a)
	if got, err := os.ReadFile(a.path); err != nil || !bytes.Contains(got, rec("other")) {
		t.Fatalf("A's run removed the file at its former name: %q, %v", got, err)
	}
	removePending(b)
	if _, err := os.Stat(b.path); !os.IsNotExist(err) {
		t.Fatalf("a run's own pending file is still there: %v", err)
	}
	removePending(nil)
}

// A writer takes the pending lock beside another writer holding it: it
// holds it shared, not past the wait without it.
func TestAPendingWriterHoldsTheLockBesideAnother(t *testing.T) {
	defer func() { duringPendingWrite = nil }()
	spool := filepath.Join(t.TempDir(), "spool", "hub.jsonl")
	other, err := lockSpoolShared(spool, "pending", 0)
	if err != nil {
		t.Fatal(err)
	}
	held := false
	duringPendingWrite = func(_ string, written bool) {
		if !written {
			return
		}
		other.release()
		x, err := lockSpool(spool, "pending", 0)
		held = errors.Is(err, errQueueBusy)
		x.release()
	}
	if _, err := writePending(spool, rec("mine")); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("the writer did not hold the pending lock beside another writer")
	}
}
