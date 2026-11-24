package adapter

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// runLine returns the run's adapter.log line for session s turn t.
func runLine(t *testing.T, root, session, turn string) []byte {
	t.Helper()
	notes, _ := os.ReadFile(filepath.Join(root, "adapter.log"))
	for l := range bytes.SplitSeq(notes, []byte{'\n'}) {
		if bytes.Contains(l, []byte("session "+session+" turn "+turn+" left work for a later run")) {
			return l
		}
	}
	t.Fatalf("no run line for session %s turn %s:\n%s", session, turn, notes)
	return nil
}

// A Codex event built from the hook payload alone, because the rollout
// could not be read (not because its time share ran out), names the
// reason in the run's line with the event, the session and the turn, and
// carries no content of the turn: a thin event stays thin in the store.
func TestACodexEventFromThePayloadAloneIsNamed(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "rollout-gone.jsonl")
	p := buildCodex(t, map[string]any{
		"session_id": "s", "turn_id": "t9", "cwd": t.TempDir(),
		"transcript_path": missing,
		"hook_event_name": "Stop", "last_assistant_message": "REPLY-CONTENT-MARK",
	})
	root := t.TempDir()
	NewClient(root).SubmitLocal(p)
	line := runLine(t, root, "s", "t9")
	for _, want := range []string{"the run for the turn event of session s turn t9", "the rollout could not be read (", "rollout-gone.jsonl", "the event holds what was read"} {
		if !bytes.Contains(line, []byte(want)) {
			t.Fatalf("the run's line lacks %q: %s", want, line)
		}
	}
	if bytes.Contains(line, []byte("REPLY-CONTENT-MARK")) {
		t.Fatalf("the run's line carries the turn's content: %s", line)
	}
}

// A rollout that reads whole leaves no reason in the run's line.
func TestACodexEventFromAReadableRolloutIsNotNamed(t *testing.T) {
	p := buildCodex(t, map[string]any{
		"session_id": "sess-c", "turn_id": "turn-2", "cwd": t.TempDir(),
		"transcript_path": codexRolloutTwoTurns(t),
		"hook_event_name": "Stop",
	})
	if p.Degraded != "" {
		t.Fatalf("a readable rollout degraded the event: %q", p.Degraded)
	}
}

// The run's line fits a run that is not a hook's, such as an import
// (SubmitLocal): it names the run, never "the hook".
func TestARunsLineOutsideAHookDoesNotSayHook(t *testing.T) {
	root := t.TempDir()
	c := NewClient(root)
	p := &Payload{ProjectID: "proj-b"}
	p.Event.Client, p.Event.SessionID, p.Event.Kind = "codex", "s1", "turn"
	p.Event.TurnID, p.Event.IdempotencyKey = "t1", "codex:s1:t1"
	c.deferred("an imported event's")
	c.SubmitLocal(p)
	line := runLine(t, root, "s1", "t1")
	if bytes.Contains(line, []byte("the hook")) || !bytes.Contains(line, []byte("aimem: the run for the turn event of session s1 turn t1")) {
		t.Fatalf("the run's line outside a hook: %s", line)
	}
}
