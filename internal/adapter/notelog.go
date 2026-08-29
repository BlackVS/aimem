package adapter

// Adapter-side warnings — spooled checkpoints, orphaned hub bindings,
// shared-doc conflicts — used to exist only on the submit process's
// stderr, which OpenCode's detached spawn discards and Windows hook
// plumbing buries; the RC binding incident ran silent for four hours
// behind a green sync log. Note keeps the stderr line AND appends it,
// timestamped, to <state-root>/adapter.log, which `aimem logs` reads.
// A file rather than an API on purpose: these warnings matter most
// exactly when the local service is down.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// noteLogMax bounds adapter.log; one previous window is kept as .1.
const noteLogMax = 512 * 1024

// Note records one client-side warning under the given state root.
func Note(root, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Fprintln(os.Stderr, msg)
	p := filepath.Join(root, "adapter.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > noteLogMax {
		os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return // stderr already has it; never fail the caller over a log
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), msg)
}

func (c *Client) note(format string, a ...any) { Note(c.root, format, a...) }
