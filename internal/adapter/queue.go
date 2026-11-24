package adapter

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"aimem/internal/filelock"
)

// A spool is a queue of records, one per line, that any number of aimem
// runs may use at once: hook runs, `aimem spool-flush`, the import. Three
// locks, each on a file of its own beside the spool, keep them apart:
//
//   - the append lock is held for one append, and by a flush for its
//     snapshot and for removing what it delivered;
//   - the flush lock is held by the one run delivering the spool, and by
//     the one run moving pending files into it;
//   - the pending lock is held shared by each run writing a pending file,
//     from before its creation to after its close, and exclusive by the
//     pending sweep while it lists the files: a listed file is whole,
//     unless its writer wrote it past the lock's wait (writePendingFile).
//
// Appenders only ever add at the end. A flush reads a snapshot, delivers
// it unlocked, then rewrites the spool without the part it walked. The
// operating system releases a lock when its holder exits, so a killed run
// leaves nothing that stops a later one.

// appendLockWait bounds how long an append waits for another run's hold
// on the append lock; past it the record is kept in a pending file.
var appendLockWait = 200 * time.Millisecond

// errQueueBusy is a lock held by another run.
var errQueueBusy = errors.New("another run holds the queue")

// errMovedTwice is an append that found the spool moved away under it on
// its retry too: the record is in no file that stays, and the caller
// keeps its own copy. It counts as a busy queue.
var errMovedTwice = fmt.Errorf("%w: an older client moved the spool away during the append, twice", errQueueBusy)

// tryLock and tryLockShared are filelock's; a test replaces them to
// refuse locks.
var (
	tryLock       = filelock.TryLock
	tryLockShared = filelock.TryLockShared
)

// noLocksLine names, once in a run's line, a filesystem that refuses the
// queue's lock.
const noLocksLine = "the queued records (this filesystem refuses the queue's lock: they are appended without it, and nothing is delivered from the queue here)"

// noteNoLocks puts noLocksLine in the run's line when err is a refused
// lock, and reports whether it was.
func (c *Client) noteNoLocks(err error) bool {
	var nl *noLocksError
	if !errors.As(err, &nl) {
		return false
	}
	// Once per client, not per event: an import submits many events on
	// one client, and each logs its own line.
	c.noLocksOnce.Do(func() { c.deferred(noLocksLine) })
	return true
}

// noLocksError is a filesystem that refuses advisory locks.
type noLocksError struct{ err error }

func (e *noLocksError) Error() string { return "the queue's lock is unavailable: " + e.err.Error() }
func (e *noLocksError) Unwrap() error { return e.err }

// spoolLockPath names a spool's lock file of a kind. A lock file is never
// removed: a run that removed one while another held it would let a third
// lock a new file of the same name, and the two would not exclude each
// other. Its name does not begin with the spool's, so neither a sweep nor
// the status reader takes it for part of the queue.
func spoolLockPath(spool, kind string) string {
	return filepath.Join(filepath.Dir(spool), "."+filepath.Base(spool)+"."+kind+"-lock")
}

type spoolLock struct{ f *os.File }

func (l *spoolLock) release() {
	if l != nil {
		filelock.Unlock(l.f)
		l.f.Close()
	}
}

// lockSpool takes a spool's lock of a kind, trying until wait has passed:
// errQueueBusy when another run holds it, a *noLocksError when the
// filesystem refuses the lock.
func lockSpool(spool, kind string, wait time.Duration) (*spoolLock, error) {
	return lockSpoolWith(tryLock, spool, kind, wait)
}

// lockSpoolShared is lockSpool for a lock held shared: any number of runs
// hold it at once, none while one holds it exclusive (lockSpool).
func lockSpoolShared(spool, kind string, wait time.Duration) (*spoolLock, error) {
	return lockSpoolWith(tryLockShared, spool, kind, wait)
}

func lockSpoolWith(try func(*os.File) (bool, error), spool, kind string, wait time.Duration) (*spoolLock, error) {
	path := spoolLockPath(spool, kind)
	f, err := openCreating(path, os.O_RDONLY|os.O_CREATE)
	if err != nil {
		return nil, err
	}
	until := time.Now().Add(wait)
	for {
		ok, err := try(f)
		switch {
		case err != nil:
			f.Close()
			return nil, &noLocksError{err}
		case ok:
			return &spoolLock{f}, nil
		case !time.Now().Before(until):
			f.Close()
			return nil, errQueueBusy
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// appendRecords appends records (separated by newlines, a final newline
// optional) to spool as whole lines, waiting at most wait for the append
// lock. On a filesystem without locks it appends unlocked, as no flush
// can run there to race it, and reports that through unlocked when it is
// not nil.
func appendRecords(spool string, recs []byte, wait time.Duration, unlocked func(error)) error {
	recs = bytes.TrimRight(recs, "\n")
	if len(bytes.TrimSpace(recs)) == 0 {
		return nil
	}
	l, err := lockSpool(spool, "append", wait)
	if nl := (*noLocksError)(nil); errors.As(err, &nl) {
		if unlocked != nil {
			unlocked(err)
		}
	} else if err != nil {
		return err
	}
	defer l.release()
	return appendHeld(spool, recs)
}

// appendHeld appends recs, which end without a newline, in one write, with
// the append lock held by the caller. A file whose last record was cut
// mid-line gets a newline first, so the torn record stays a line of its
// own and never joins this one.
func appendHeld(spool string, recs []byte) error {
	if len(bytes.TrimSpace(recs)) == 0 {
		return nil
	}
	for try := 0; ; try++ {
		data := make([]byte, 0, len(recs)+2)
		if endsCut(spool) {
			data = append(data, '\n')
		}
		data = append(append(data, recs...), '\n')
		// Write-only, as a read-write handle costs a write on Windows two
		// orders of magnitude more.
		f, err := openCreating(spool, os.O_APPEND|os.O_CREATE|os.O_WRONLY)
		if err != nil {
			return err
		}
		_, err = f.Write(data)
		if afterAppendWrite != nil {
			afterAppendWrite(spool)
		}
		moved := err == nil && renamedAway(f, spool)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil || !moved {
			return err
		}
		if try > 0 {
			return errMovedTwice
		}
		// Serves a client older than this release: its flush renames the
		// spool to a claim without the lock, and this write may have
		// landed in the claim after that run read it. Write it again into
		// the spool; at worst it is delivered twice.
	}
}

// openCreating opens path with flag, creating its directory only when it
// is missing: a directory check before every open costs an append on
// Windows a hundredfold.
func openCreating(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, flag, 0o600)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		f, err = os.OpenFile(path, flag, 0o600)
	}
	return f, err
}

// endsCut reports whether spool's last record lacks its newline: a write
// that was cut. The caller holds the append lock, so no run of this
// release is writing it.
func endsCut(spool string) bool {
	f, err := os.Open(spool)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() == 0 {
		return false
	}
	last := []byte{0}
	_, err = f.ReadAt(last, fi.Size()-1)
	return err == nil && last[0] != '\n'
}

// afterAppendWrite runs between an append's write and its check; a test
// sets it to rename the spool away in that window, as an older client's
// flush does.
var afterAppendWrite func(spool string)

// renamedAway reports whether path no longer names the open file f.
func renamedAway(f *os.File, path string) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	cur, err := os.Stat(path)
	if err != nil {
		return os.IsNotExist(err)
	}
	return !os.SameFile(fi, cur)
}

// sendFunc delivers one record of a flush: keep queues it again, and a
// stop error ends the walk at it, the record and the rest left queued.
type sendFunc func(line []byte) (keep bool, stop error)

// beforeCompact runs between a flush's walk and its compaction, and
// duringCompact between the compaction's move of the spool and its read
// of it; tests act in those windows.
var beforeCompact, duringCompact func(spool string)

// drainSpool delivers spool with send, in on-disk order, as the one run
// holding its flush lock, and then removes what it walked. noTime, asked
// after the .replay-* files are returned (claimSweep), names the run's
// lack of time itself. It returns why the walk stopped early, and an
// error that kept the flush from running or the spool from being
// rewritten: errQueueBusy when another run holds a lock, which is no
// failure.
func (c *Client) drainSpool(spool string, reserve time.Duration, noTime func() bool, send sendFunc) (stop, err error) {
	fl, err := lockSpool(spool, "flush", 0)
	if err != nil {
		return nil, err
	}
	defer fl.release()
	claimSweep(c, spool, reserve)
	if noTime() {
		return nil, nil
	}
	snap, err := snapshot(spool)
	if err != nil || len(snap) == 0 {
		return nil, err
	}
	var kept bytes.Buffer
	rest := snap
	for len(rest) > 0 {
		line, after, _ := bytes.Cut(rest, []byte{'\n'})
		if len(bytes.TrimSpace(line)) == 0 {
			rest = after
			continue
		}
		keep, s := send(line)
		if s != nil {
			stop = s
			break
		}
		rest = after
		if keep {
			kept.Write(line)
			kept.WriteByte('\n')
		}
	}
	if beforeCompact != nil {
		beforeCompact(spool)
	}
	walked := snap[:len(snap)-len(rest)]
	if len(walked) == 0 {
		return stop, nil
	}
	return stop, compactSpool(spool, walked, kept.Bytes())
}

// snapshot reads spool under its append lock, up to its last newline: an
// unterminated tail is a write that was cut, and stays where it is.
func snapshot(spool string) ([]byte, error) {
	l, err := lockSpool(spool, "append", appendLockWait)
	if err != nil {
		return nil, err
	}
	defer l.release()
	data, err := os.ReadFile(spool)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return data[:bytes.LastIndexByte(data, '\n')+1], nil
}

// compactSpool removes the walked part from the front of spool, under its
// append lock, and puts the kept records in its place, before everything
// that followed it. It moves the spool aside to a claim-named file
// (renameSpool), which no appender reaches any more, and appends what
// remains of it to the spool: no run of this release writes the spool
// meanwhile, so it starts anew. A run killed before the claim-named file
// is removed leaves it to the next flush, which returns it (claimSweep):
// its delivered records go twice, nothing is lost.
func compactSpool(spool string, walked, kept []byte) error {
	l, err := lockSpool(spool, "append", appendLockWait)
	if err != nil {
		return err
	}
	defer l.release()
	aside := fmt.Sprintf("%s.replay-%d-%d", spool, os.Getpid(), time.Now().UnixNano())
	err = renameSpool(spool, aside)
	// A reader's open of the spool, which Windows refuses the rename
	// over, is brief: try a few times.
	for try := 0; inUse(err) && try < 5; try++ {
		time.Sleep(20 * time.Millisecond)
		err = renameSpool(spool, aside)
	}
	switch {
	case inUse(err):
		// Another process keeps the spool open: it stays as it is, and the
		// walked records go again. No failure.
		return errQueueBusy
	case os.IsNotExist(err):
		// Serves a client older than this release: its flush took the
		// spool as a claim after the snapshot, and delivers the walked
		// records itself. Only the kept records are queued again.
		return appendHeld(spool, bytes.TrimRight(kept, "\n"))
	case err != nil:
		return fmt.Errorf("the queue could not be rewritten, its delivered records stay queued: %w", err)
	}
	if duringCompact != nil {
		duringCompact(spool)
	}
	cur, err := os.ReadFile(aside)
	if os.IsNotExist(err) {
		// Serves a client older than this release: its sweep took the
		// moved file as a claim and appended all of it to the spool, the
		// kept records included.
		return nil
	}
	if err != nil {
		return fmt.Errorf("the queue could not be rewritten, its delivered records stay queued in %s: %w", filepath.Base(aside), err)
	}
	rest := cur
	if bytes.HasPrefix(cur, walked) {
		rest = cur[len(walked):]
	}
	// Otherwise serves a client older than this release: its flush took
	// the spool after the snapshot and wrote another back, which goes
	// back whole.
	next := append(append(make([]byte, 0, len(kept)+len(rest)), kept...), rest...)
	// Appended, never put in place: the spool holds anything an older
	// client appended since the move, and keeps it.
	if err := appendHeld(spool, bytes.TrimRight(next, "\n")); err != nil {
		return fmt.Errorf("the queue could not be rewritten, its delivered records stay queued in %s: %w", filepath.Base(aside), err)
	}
	os.Remove(aside)
	return nil
}

// renameSpool is os.Rename; a test replaces it to fail.
var renameSpool = os.Rename

// inUse reports a rename that Windows refused because another process has
// the file open. Elsewhere a rename never meets that.
func inUse(err error) bool {
	if err == nil || runtime.GOOS != "windows" {
		return false
	}
	const errSharingViolation = syscall.Errno(32)
	return errors.Is(err, os.ErrPermission) || errors.Is(err, errSharingViolation)
}
