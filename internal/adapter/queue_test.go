package adapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// queued lists the keys of the records in a spool, in order.
func queued(t *testing.T, spool string) []string {
	t.Helper()
	b, err := os.ReadFile(spool)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for line := range bytes.SplitSeq(b, []byte{'\n'}) {
		var r struct {
			K string `json:"k"`
		}
		if json.Unmarshal(line, &r) == nil {
			out = append(out, r.K)
		}
	}
	return out
}

// Criterion 1: a record appended between the flush's read of the backlog
// and its removal of what it delivered is kept: still queued, after the
// records the flush did not reach.
func TestAnAppendBetweenTheFlushReadAndItsRemovalIsKept(t *testing.T) {
	defer func() { beforeCompact = nil }()
	hub, keys := keysHub(t, nil)
	root, _ := hubRoot(t, hub.URL)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	for _, k := range []string{"a", "b", "c"} {
		appendLines(spool, rec(k))
	}
	beforeCompact = func(string) {
		if err := appendLines(spool, rec("late")); err != nil {
			t.Errorf("the append during the flush: %v", err)
		}
	}
	c := NewClient(root)
	c.flushHubSpool(resolved(t, root), spool)
	if got := strings.Join(keys(), ","); got != "a,b,c" {
		t.Fatalf("the hub got %q, want the backlog it read", got)
	}
	if got := queued(t, spool); !slices.Equal(got, []string{"late"}) {
		t.Fatalf("queued after the flush: %q, want the late record", got)
	}

	// The same window in the local replay, with a record the service
	// keeps failing: kept in front of the late one.
	beforeCompact = func(string) { appendLines(NewClient(root).spoolPath(), rec("late")) }
	local := NewClient(root) // no service here: every post fails
	for _, k := range []string{"x", "y"} {
		appendLines(local.spoolPath(), rec(k))
	}
	if _, kept, err := local.ReplaySpool(); err != nil || kept != 2 {
		t.Fatalf("replay: %d kept, %v", kept, err)
	}
	if got := queued(t, local.spoolPath()); !slices.Equal(got, []string{"x", "y", "late"}) {
		t.Fatalf("the local spool after the replay: %q", got)
	}
}

// Criterion 2 against a hub that answers: two runs at once, ten times
// over, deliver every record of the backlog and their own; each record is
// delivered or still queued, once.
func TestConcurrentRunsAgainstAnAnsweringHubDeliverEveryRecordOnce(t *testing.T) {
	hub, keys := keysHub(t, nil)
	root, _ := hubRoot(t, hub.URL)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	want := map[string]bool{}
	var backlog bytes.Buffer
	for i := range 300 {
		backlog.Write(rec(fmt.Sprint("b", i)))
		backlog.WriteByte('\n')
		want[fmt.Sprint("b", i)] = true
	}
	appendLines(spool, backlog.Bytes())
	for round := range 10 {
		var wg sync.WaitGroup
		for j := range 2 {
			key := fmt.Sprintf("r%d-%d", round, j)
			want[key] = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				cmd := exec.Command(os.Args[0], "-test.run=^TestHubPushHelperProcess$")
				cmd.Env = append(os.Environ(), "AIMEM_TEST_PUSH_ROOT="+root, "AIMEM_TEST_PUSH_KEY="+key)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("run %s: %v\n%s", key, err, out)
				}
			}()
		}
		wg.Wait()
	}
	seen := map[string]int{}
	for _, k := range keys() {
		seen[k]++
	}
	for _, k := range queued(t, spool) {
		seen[k]++
	}
	for k := range want {
		if seen[k] != 1 {
			t.Errorf("record %s was delivered or queued %d time(s), want 1", k, seen[k])
		}
	}
	if len(seen) != len(want) {
		t.Errorf("%d distinct records, want %d", len(seen), len(want))
	}
	if left, _ := filepath.Glob(spool + ".*"); len(left) != 0 {
		t.Errorf("left behind %v", left)
	}
}

// Criterion 3: a flush that finds another run delivering the backlog
// leaves it alone and says nothing: the hub is not asked, the spool is the
// same file, and neither the run's line nor adapter.log names it.
func TestAFlushThatFindsAnotherRunningLeavesTheBacklogQuietly(t *testing.T) {
	hub, keys := keysHub(t, nil)
	root, _ := hubRoot(t, hub.URL)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	appendLines(spool, rec("queued"))
	same := spoolLink(t, spool)
	other, err := lockSpool(spool, "flush", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other.release()
	c := NewClient(root).WithDeadline(time.Now().Add(5 * time.Second))
	c.flushHubSpool(resolved(t, root), spool)
	if len(keys()) != 0 || !same() || len(c.deferredList) != 0 {
		t.Fatalf("hub asked %v, same file %v, deferred %q", keys(), same(), c.deferredList)
	}
	local := NewClient(root)
	other2, err := lockSpool(local.spoolPath(), "flush", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer other2.release()
	appendLines(local.spoolPath(), rec("local"))
	if n, kept, err := local.ReplaySpool(); n != 0 || kept != 0 || err != nil || len(local.deferredList) != 0 {
		t.Fatalf("replay: %d, %d, %v, deferred %q", n, kept, err, local.deferredList)
	}
	if notes, _ := os.ReadFile(filepath.Join(root, "adapter.log")); len(notes) != 0 {
		t.Fatalf("adapter.log:\n%s", notes)
	}
}

// TestSpoolLockHelperProcess holds a spool's locks until it is killed:
// the run of TestALockOfAKilledRunStopsNothing.
func TestSpoolLockHelperProcess(t *testing.T) {
	spool := os.Getenv("AIMEM_TEST_LOCK_SPOOL")
	if spool == "" {
		t.Skip("helper process only")
	}
	for _, kind := range []string{"flush", "append"} {
		if _, err := lockSpool(spool, kind, 0); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("held")
	time.Sleep(time.Minute)
}

// killedLockBound is the stated bound for a killed run's locks to stop
// delivery: the OS releases them at its exit.
const killedLockBound = 5 * time.Second

// Criterion 4: the locks of a run that is killed stop nothing once it is
// gone. While it lives, a flush leaves the backlog to it; right after the
// kill the next flush delivers it, and an append no longer waits. The
// temporary file of a compaction a killed run left is removed.
func TestALockOfAKilledRunStopsNothing(t *testing.T) {
	hub, keys := keysHub(t, nil)
	root, _ := hubRoot(t, hub.URL)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	appendLines(spool, rec("queued"))
	leftover := spool + ".replay-123-456" // a compaction's move of the spool, its run killed
	os.WriteFile(leftover, append(rec("moved"), '\n'), 0o600)

	cmd := exec.Command(os.Args[0], "-test.run=^TestSpoolLockHelperProcess$")
	cmd.Env = append(os.Environ(), "AIMEM_TEST_LOCK_SPOOL="+spool)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	if line, _ := bufio.NewReader(out).ReadString('\n'); !strings.Contains(line, "held") {
		t.Fatalf("the helper said %q", line)
	}
	NewClient(root).flushHubSpool(resolved(t, root), spool)
	if len(keys()) != 0 {
		t.Fatalf("a flush ran beside a live holder: %v", keys())
	}
	cmd.Process.Kill()
	cmd.Wait()
	killed := time.Now()

	// The OS releases the dead run's locks at its exit; Windows documents
	// that this may take a moment, so later flushes are tried for up to
	// killedLockBound.
	for len(keys()) == 0 && time.Since(killed) < killedLockBound {
		NewClient(root).flushHubSpool(resolved(t, root), spool)
		if len(keys()) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	freed := time.Since(killed)
	if got := strings.Join(keys(), ","); got != "queued,moved" {
		t.Fatalf("%v after the kill the hub got %q", freed, got)
	}
	start := time.Now()
	if err := appendLines(spool, rec("after")); err != nil {
		t.Fatal(err)
	}
	t.Logf("delivered %v after the kill; an append took %v", freed, time.Since(start))
	if time.Since(start) >= appendLockWait {
		t.Fatalf("an append waited %v on a dead run's lock", time.Since(start))
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Fatalf("the killed compaction's file is still there: %v", err)
	}
	if got := queued(t, spool); !slices.Equal(got, []string{"after"}) {
		t.Fatalf("queued %q", got)
	}
}

// Criterion 6: a hook run that meets the queue held by another run ends
// inside its budget: it waits for the append lock at most appendLockWait,
// and its record waits in its own pending file.
func TestAHookRunEndsInItsBudgetWithTheQueueHeld(t *testing.T) {
	down := httptest.NewServer(http.NotFoundHandler())
	url := down.URL
	down.Close()
	root, _ := hubRoot(t, url)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	appendLines(spool, rec("queued"))
	// Interrupted runs' pending files, which the run would move in: with
	// the queue held, it moves none and does not wait for each.
	defer func(d time.Duration) { pendingStale = d }(pendingStale)
	pendingStale = 0
	for i := range 10 {
		if _, err := pendingWrite(spool, rec(fmt.Sprint("p", i))); err != nil {
			t.Fatal(err)
		}
	}
	for _, kind := range []string{"flush", "append"} {
		other, err := lockSpool(spool, kind, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer other.release()
	}
	budget := 5 * time.Second
	start := time.Now()
	NewClient(root).WithDeadline(start.Add(budget)).pushHub("", rec("mine"))
	took := time.Since(start)
	t.Logf("the run took %v with the queue held (append wait %v, budget %v)", took, appendLockWait, budget)
	// Two waits of appendLockWait at most: its own append, and the first
	// pending file's. One wait per pending file would be 2.2 s.
	if took > 1200*time.Millisecond {
		t.Fatalf("the run took %v", took)
	}
	if got := queued(t, spool); !slices.Equal(got, []string{"queued"}) {
		t.Fatalf("the spool changed under another run's lock: %q", got)
	}
	pend := spoolSiblings(spool, ".pending-")
	if len(pend) != 11 {
		t.Fatalf("%d pending files, want the ten and the run's own", len(pend))
	}
	var mine int
	for _, p := range pend {
		if b, _ := os.ReadFile(p); bytes.Contains(b, rec("mine")) {
			mine++
		}
	}
	if mine != 1 {
		t.Fatalf("the run's record is in %d pending files", mine)
	}
}

// The local spool keeps a record in a pending file too when another run
// holds it, and its replay moves the file in once it is stale.
func TestTheLocalSpoolFallsBackToAPendingFile(t *testing.T) {
	defer func(d time.Duration) { pendingStale = d }(pendingStale)
	root := t.TempDir()
	c := NewClient(root)
	other, err := lockSpool(c.spoolPath(), "append", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.spool(rec("mine")); err != nil {
		t.Fatalf("spool with the queue held: %v", err)
	}
	other.release()
	if pend := spoolSiblings(c.spoolPath(), ".pending-"); len(pend) != 1 {
		t.Fatalf("pending files %v", pend)
	}
	pendingStale = 0
	c.ReplaySpool() // no service: the record is moved in and kept
	if got := queued(t, c.spoolPath()); !slices.Equal(got, []string{"mine"}) {
		t.Fatalf("queued %q", got)
	}
	if pend := spoolSiblings(c.spoolPath(), ".pending-"); len(pend) != 0 {
		t.Fatalf("pending files left %v", pend)
	}
}

// A write that was cut mid-line stays one line of its own: the next append
// starts on a new line, and a flush neither delivers the torn record with
// the next one nor removes it.
func TestATornLastLineNeverJoinsTheNextRecord(t *testing.T) {
	hub, keys := keysHub(t, nil)
	root, _ := hubRoot(t, hub.URL)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	os.MkdirAll(filepath.Dir(spool), 0o700)
	os.WriteFile(spool, append(append(rec("whole"), '\n'), `{"k":"to`...), 0o600)
	snap, err := snapshot(spool)
	if err != nil || string(snap) != string(rec("whole"))+"\n" {
		t.Fatalf("snapshot %q, %v", snap, err)
	}
	appendLines(spool, rec("next"))
	if b, _ := os.ReadFile(spool); !bytes.HasSuffix(b, []byte(`{"k":"to`+"\n"+string(rec("next"))+"\n")) {
		t.Fatalf("the spool after an append to a torn line: %q", b)
	}
	NewClient(root).flushHubSpool(resolved(t, root), spool)
	if got := strings.Join(keys(), ","); got != "whole,,next" { // the torn line reaches the hub as the invalid record it is
		t.Fatalf("the hub got %q", got)
	}
}

// For a client older than this release: an append whose spool an older
// flush renamed to its claim between the write and the check is written
// again into the spool. The record is then in the claim and in the spool:
// delivered twice at worst, never lost.
func TestAnAppendIntoAnOlderClientsClaimIsWrittenAgain(t *testing.T) {
	defer func() { afterAppendWrite = nil }()
	spool := filepath.Join(t.TempDir(), "spool", "hub.jsonl")
	appendLines(spool, rec("before"))
	claim := spool + ".replay-4242"
	var renameErr error
	afterAppendWrite = func(sp string) {
		afterAppendWrite = nil
		renameErr = os.Rename(sp, claim)
	}
	if err := appendLines(spool, rec("mine")); err != nil {
		t.Fatal(err)
	}
	if renameErr != nil {
		// Windows refuses the older client's rename while the append holds
		// the file: the record is in the spool, once.
		if got := queued(t, spool); !slices.Equal(got, []string{"before", "mine"}) {
			t.Fatalf("rename refused (%v); the spool holds %q", renameErr, got)
		}
		return
	}
	if got := queued(t, claim); !slices.Equal(got, []string{"before", "mine"}) {
		t.Fatalf("the claim holds %q", got)
	}
	if got := queued(t, spool); !slices.Equal(got, []string{"mine"}) {
		t.Fatalf("the spool holds %q, want the record written again", got)
	}
}

// For a client older than this release: an unlocked append that lands
// after a compaction moved the spool aside creates the spool anew, and the
// compaction appends the rest to it rather than replacing it: kept, on
// every platform.
func TestAnOlderClientsAppendDuringACompactionIsKept(t *testing.T) {
	defer func() { duringCompact = nil }()
	hub, keys := keysHub(t, nil)
	root, _ := hubRoot(t, hub.URL)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	appendLines(spool, rec("a"))
	duringCompact = func(sp string) {
		f, err := os.OpenFile(sp, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) // no lock, as the older client
		if err != nil {
			t.Fatal(err)
		}
		f.Write(append(rec("old"), '\n'))
		f.Close()
	}
	NewClient(root).flushHubSpool(resolved(t, root), spool)
	if got := strings.Join(keys(), ","); got != "a" {
		t.Fatalf("the hub got %q", got)
	}
	if got := queued(t, spool); !slices.Equal(got, []string{"old"}) {
		t.Fatalf("queued %q, want the older client's record", got)
	}
	if left, _ := filepath.Glob(spool + ".*"); len(left) != 0 {
		t.Fatalf("left behind %v", left)
	}
}

// For a client older than this release: a flush that finds the spool taken
// by an older client's claim after its snapshot removes nothing, and the
// claim the older client leaves is returned to the spool by the next
// flush, under its flush lock.
func TestAnOlderClientsClaimIsReturnedAndNeverCut(t *testing.T) {
	defer func() { beforeCompact = nil }()
	// With a run appending to a new spool after the older flush took the
	// old one, the compaction finds a spool that does not start with what
	// it walked; without, it finds none.
	for _, newAppend := range []bool{true, false} {
		t.Run(fmt.Sprint("newAppend=", newAppend), func(t *testing.T) {
			hub, keys := keysHub(t, nil)
			root, _ := hubRoot(t, hub.URL)
			spool := filepath.Join(root, "spool", "hub.jsonl")
			appendLines(spool, rec("a"))
			appendLines(spool, rec("b"))
			claim := spool + ".replay-4242"
			beforeCompact = func(sp string) {
				beforeCompact = nil
				if err := os.Rename(sp, claim); err != nil { // the older flush takes the spool
					t.Fatal(err)
				}
				if newAppend {
					appendLines(sp, rec("c"))
				}
			}
			c := NewClient(root).WithDeadline(time.Now().Add(5 * time.Second))
			c.flushHubSpool(resolved(t, root), spool)
			if len(c.deferredList) != 0 {
				t.Fatalf("the run's line names %q", c.deferredList)
			}
			want, wantKeys := []string(nil), "a,b,a,b"
			if newAppend {
				want, wantKeys = []string{"c"}, "a,b,c,a,b"
			}
			if got := queued(t, spool); !slices.Equal(got, want) {
				t.Fatalf("queued %q, want %q: the new spool untouched", got, want)
			}
			// The older run died with its claim: the next flush returns it.
			NewClient(root).flushHubSpool(resolved(t, root), spool)
			if got := strings.Join(keys(), ","); got != wantKeys {
				t.Fatalf("the hub got %q, want %q", got, wantKeys)
			}
			if left, _ := filepath.Glob(spool + "*"); len(left) != 0 {
				t.Fatalf("left behind %v", left)
			}
		})
	}
}

// A flush whose compaction meets the spool open in another process (the
// status reader) says nothing, whether all was delivered or a record was
// appended meanwhile. On Windows, which refuses to move an open file, the
// spool stays as it is and its delivered records go again; elsewhere the
// move succeeds under the reader.
func TestARewriteThatMeetsAnOpenSpoolIsQuiet(t *testing.T) {
	defer func() { beforeCompact = nil }()
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprint("late=", late), func(t *testing.T) {
			hub, keys := keysHub(t, nil)
			root, _ := hubRoot(t, hub.URL)
			spool := filepath.Join(root, "spool", "hub.jsonl")
			appendLines(spool, rec("a"))
			appendLines(spool, rec("b"))
			var reader *os.File
			beforeCompact = func(sp string) {
				if late {
					appendLines(sp, rec("late"))
				}
				var err error
				if reader, err = os.Open(sp); err != nil {
					t.Fatal(err)
				}
			}
			c := NewClient(root).WithDeadline(time.Now().Add(5 * time.Second))
			c.flushHubSpool(resolved(t, root), spool)
			reader.Close()
			if got := strings.Join(keys(), ","); got != "a,b" {
				t.Fatalf("the hub got %q", got)
			}
			if len(c.deferredList) != 0 {
				t.Fatalf("the run's line names %q", c.deferredList)
			}
			var want []string
			if runtime.GOOS == "windows" {
				want = []string{"a", "b"}
			}
			if late {
				want = append(want, "late")
			}
			if got := queued(t, spool); !slices.Equal(got, want) {
				t.Fatalf("queued %q, want %q", got, want)
			}
			if left, _ := filepath.Glob(spool + ".*"); len(left) != 0 {
				t.Fatalf("left behind %v", left)
			}
		})
	}
}

// For a client older than this release: its sweep, which takes every
// .replay-* file without a lock, may take a compaction's moved file before
// the compaction reads it. It appends the whole file to the spool, so the
// delivered records go again and nothing is lost, and the run says
// nothing.
func TestAnOlderClientsSweepOfAMovedFileIsQuiet(t *testing.T) {
	defer func() { duringCompact = nil }()
	hub, keys := keysHub(t, nil)
	root, _ := hubRoot(t, hub.URL)
	spool := filepath.Join(root, "spool", "hub.jsonl")
	appendLines(spool, rec("a"))
	duringCompact = func(sp string) {
		moved, _ := filepath.Glob(sp + ".replay-*")
		for _, m := range moved { // as the older sweep: unlocked append, then remove
			data, _ := os.ReadFile(m)
			f, err := os.OpenFile(sp, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			f.Write(data)
			f.Close()
			os.Remove(m)
		}
	}
	c := NewClient(root).WithDeadline(time.Now().Add(5 * time.Second))
	c.flushHubSpool(resolved(t, root), spool)
	if len(c.deferredList) != 0 {
		t.Fatalf("the run's line names %q", c.deferredList)
	}
	if got := strings.Join(keys(), ","); got != "a" {
		t.Fatalf("the hub got %q", got)
	}
	if got := queued(t, spool); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("queued %q, want the swept record back", got)
	}
}
