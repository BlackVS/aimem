package adapter

// The hook run budget. Every aimem hook is registered with its client at a
// 10 s timeout, and a client kills a hook that overruns it: a kill before
// the local post loses the turn, a kill inside the hub post can lose the
// hub's copy. Each hook command therefore has one deadline for its whole
// run, HookBudget from process start, leaving a margin for process start
// and the client's own accounting. Reading the transcript gets the first
// share of it; whatever cannot be delivered in time stays spooled and
// goes with a later run.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"time"
)

var (
	// processStart approximates the moment the client started the hook.
	processStart = time.Now()
	// HookBudget is a hook command's whole run, measured from process
	// start: 3 s below the 10 s every installer registers.
	HookBudget = 7 * time.Second
	// TranscriptShare bounds reading the transcript or rollout and
	// waiting for its turn to be flushed, from the call that builds the
	// event; past it the event is built from the hook payload alone.
	TranscriptShare = 2500 * time.Millisecond
)

// HookDeadline is the end of this hook command's run.
func HookDeadline() time.Time { return processStart.Add(HookBudget) }

// errShare reports a transcript read cut by its time share.
var errShare = errors.New("transcript read out of time")

// maxLine bounds one transcript line; a longer one is skipped.
var maxLine = 16 << 20

// tailChunk is how much a backward scan reads at a time.
var tailChunk = 1 << 20

// scanBack reads f backwards, from its current size, and calls fn with
// each complete line (no trailing newline) and the line's byte offset, the
// last line first. It stops when fn returns true, at the start of the
// file, or at the deadline (errShare). A line longer than maxLine is
// skipped. The cost is the distance from the end to where fn stops, never
// the whole file unless fn asks for it.
func scanBack(f *os.File, deadline time.Time, fn func(line []byte, off int64) bool) error {
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	end := fi.Size()
	var carry []byte  // the start of the next-later line, not yet complete
	skipping := false // inside a line already past maxLine
	buf := make([]byte, tailChunk)
	for end > 0 {
		if time.Now().After(deadline) {
			return errShare
		}
		n := int64(tailChunk)
		if end < n {
			n = end
		}
		start := end - n
		if _, err := f.ReadAt(buf[:n], start); err != nil && err != io.EOF {
			return err
		}
		chunk := append(buf[:n:n], carry...)
		carry = nil
		// Every '\n' in chunk ends a line; what follows the last one up to
		// the end of chunk is a line whose end we already know.
		hi := len(chunk)
		for {
			i := bytes.LastIndexByte(chunk[:hi], '\n')
			if i < 0 {
				break
			}
			line := chunk[i+1 : hi]
			if skipping {
				skipping = false
			} else if len(line) > 0 && lineLen(line) <= maxLine && fn(line, start+int64(i)+1) {
				return nil
			}
			hi = i
		}
		if skipping || hi > maxLine+1 { // +1: a CR that may end it
			skipping, carry = true, nil
		} else {
			carry = append([]byte(nil), chunk[:hi]...)
		}
		end = start
	}
	if !skipping && len(carry) > 0 && lineLen(carry) <= maxLine {
		fn(carry, 0)
	}
	return nil
}

// lineLen is a line's length without the CR of a CRLF ending.
func lineLen(line []byte) int { return len(bytes.TrimSuffix(line, []byte{'\r'})) }

// forwardLines calls fn with each line of r up to its end, without the
// newline. A line longer than maxLine is skipped whole, as scanBack skips
// it, and the read goes on after it. The deadline is checked before every
// line and once more at the end, so a read that ran out of its share
// never reports success. fn must not keep the slice.
func forwardLines(r io.Reader, deadline time.Time, fn func(line []byte)) error {
	br := bufio.NewReaderSize(r, 1<<20)
	var long []byte
	over := false
	for {
		if time.Now().After(deadline) {
			return errShare
		}
		frag, err := br.ReadSlice('\n')
		partial := errors.Is(err, bufio.ErrBufferFull)
		// The line end (LF or CRLF) is not part of its length.
		n := len(long) + len(frag)
		if !partial {
			end := frag
			if bytes.HasSuffix(end, []byte{'\n'}) {
				n, end = n-1, end[:len(end)-1]
			}
			if len(end) == 0 {
				end = long // the buffer split the CRLF
			}
			if bytes.HasSuffix(end, []byte{'\r'}) {
				n--
			}
		}
		if n > maxLine+1 || (!partial && n > maxLine) {
			over, long = true, long[:0]
		}
		if partial {
			if !over {
				long = append(long, frag...)
			}
			continue
		}
		line := frag
		if len(long) > 0 {
			line = append(long, frag...)
		}
		if line = bytes.TrimSuffix(line, []byte{'\n'}); !over && len(line) > 0 {
			fn(line)
		}
		long, over = long[:0], false
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if time.Now().After(deadline) {
		return errShare
	}
	return nil
}

// reqCtx returns a context bounded by the client's run deadline, or an
// unbounded one outside a hook.
func (c *Client) reqCtx() (context.Context, context.CancelFunc) {
	if c.deadline.IsZero() {
		return context.WithCancel(context.Background())
	}
	return context.WithDeadline(context.Background(), c.deadline)
}

// left is the time left in the run; a long time outside a hook.
func (c *Client) left() time.Duration {
	if c.deadline.IsZero() {
		return time.Hour
	}
	return time.Until(c.deadline)
}

// WithDeadline bounds every request this client makes by the end of the
// hook run; what the run had to leave for a later one is noted once in
// adapter.log (noteDeferred).
func (c *Client) WithDeadline(t time.Time) *Client {
	c.deadline = t
	return c
}

// outOfTime reports an error caused by the end of the run itself, as
// opposed to a request's own timeout: a hub or service that did not answer
// within PushTimeout or localTimeout has failed, the run has not.
func (c *Client) outOfTime(err error) bool {
	return err != nil && !c.deadline.IsZero() && c.left() <= 0 && errors.Is(err, context.DeadlineExceeded)
}

// deferred records something the run left for a later run.
func (c *Client) deferred(what string) {
	for _, w := range c.deferredList {
		if w == what {
			return
		}
	}
	c.deferredList = append(c.deferredList, what)
}
