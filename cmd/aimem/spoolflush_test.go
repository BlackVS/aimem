package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aimem/internal/adapter"
)

// spool-flush prints its counts on stdout as before, and what the run
// left for a later one on stderr; it still succeeds.
func TestSpoolFlushNamesWhatItLeftOnStderr(t *testing.T) {
	root := t.TempDir()
	spool := filepath.Join(root, "spool", "pending.jsonl")
	if err := os.MkdirAll(filepath.Dir(spool), 0o700); err != nil {
		t.Fatal(err)
	}
	rec := `{"project_id":"proj-b","event":{"client":"codex","session_id":"s1","turn_id":"t1","kind":"turn","idempotency_key":"codex:s1:t1"}}` + "\n"
	if err := os.WriteFile(spool, []byte(rec), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	c := adapter.NewClient(root).WithDeadline(time.Now().Add(5 * time.Second))
	if err := spoolFlush(c, &stdout, &stderr); err != nil {
		t.Fatalf("spool-flush with the service down: %v", err)
	}
	if got := stdout.String(); got != `{"replayed":0,"requeued":1}`+"\n" {
		t.Fatalf("stdout %q", got)
	}
	if got := stderr.String(); !strings.HasPrefix(got, "aimem: spool-flush left work for a later run: the local spool replay (") || strings.Count(got, "\n") != 1 {
		t.Fatalf("stderr %q", got)
	}

	// Nothing left: stderr stays empty.
	os.Remove(spool)
	stdout.Reset()
	stderr.Reset()
	if err := spoolFlush(adapter.NewClient(root), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != `{"replayed":0,"requeued":0}`+"\n" || stderr.Len() != 0 {
		t.Fatalf("an empty spool-flush printed %q / %q", stdout.String(), stderr.String())
	}
}
