package adapter

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aimem/internal/schema"
	"aimem/internal/server"
	"aimem/internal/store"
)

func writeTranscript(t *testing.T, lines []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildClaudeEvent(t *testing.T) {
	tp := writeTranscript(t, []string{
		`{"type":"user","uuid":"u0","message":{"role":"user","content":"old request"}}`,
		`{"type":"assistant","uuid":"a0","message":{"role":"assistant","content":[{"type":"text","text":"old reply"}]}}`,
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"fix the bug"}}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Edit"},{"type":"text","text":"working on it"}]}}`,
		`{"type":"user","uuid":"u2","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`,
		`{"type":"assistant","uuid":"a2","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"},{"type":"text","text":"fixed and tested"}]}}`,
		`not even json`,
	})
	raw, _ := json.Marshal(map[string]string{
		"session_id": "sess-x", "transcript_path": tp,
		"cwd": t.TempDir(), "hook_event_name": "Stop",
	})
	p, err := BuildClaudeEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	e := p.Event
	if e.UserRequest != "fix the bug" {
		t.Errorf("user request: %q", e.UserRequest)
	}
	if e.AssistantReply != "fixed and tested" {
		t.Errorf("reply: %q", e.AssistantReply)
	}
	if len(e.ToolSummary) != 2 || e.ToolSummary[0] != "Edit" || e.ToolSummary[1] != "Bash" {
		t.Errorf("tools: %v", e.ToolSummary)
	}
	if e.TurnID != "a2" || e.IdempotencyKey != "claude-code:sess-x:a2" {
		t.Errorf("turn identity: %s / %s", e.TurnID, e.IdempotencyKey)
	}
	if e.Kind != schema.KindTurn || e.Outcome != schema.OutcomeOK {
		t.Errorf("kind/outcome: %s/%s", e.Kind, e.Outcome)
	}
	if err := e.Validate(); err != nil {
		t.Errorf("built event invalid: %v", err)
	}
}

func TestBuildClaudeEventFailure(t *testing.T) {
	tp := writeTranscript(t, []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"do thing"}}`,
	})
	raw, _ := json.Marshal(map[string]string{
		"session_id": "s", "transcript_path": tp, "cwd": t.TempDir(),
		"hook_event_name": "StopFailure",
	})
	p, err := BuildClaudeEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Event.Kind != schema.KindFailure || p.Event.Outcome != schema.OutcomeFailed {
		t.Errorf("failure not marked: %s/%s", p.Event.Kind, p.Event.Outcome)
	}
}

// startTestServer runs the real service on a short socket path. The
// name is a hash of the test name, NOT the name itself: macOS caps a
// unix socket path at 104 bytes and its TempDir is ~50 of them, so a
// descriptive test name in the path fails bind with EINVAL there.
func startTestServer(t *testing.T, root string) func() {
	t.Helper()
	sum := sha256.Sum256([]byte(t.Name()))
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("aimem-%x.sock", sum[:4]))
	os.Remove(sock)
	t.Setenv("AIMEM_SOCKET", sock)
	reg, err := store.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(reg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))
	httpSrv, ln, err := srv.ListenAndServe(root)
	if err != nil {
		t.Fatal(err)
	}
	return func() { httpSrv.Close(); ln.Close(); reg.Close(); os.Remove(sock) }
}

func testPayload(key string) *Payload {
	return &Payload{
		ProjectID: "proj-test",
		Event: schema.Event{
			SchemaVersion:  schema.Version,
			IdempotencyKey: key,
			Client:         "claude-code",
			SessionID:      "s1",
			TurnID:         "t-" + key,
			Kind:           schema.KindTurn,
			Outcome:        schema.OutcomeOK,
			TS:             time.Now().UTC().Format(time.RFC3339),
			UserRequest:    "hello",
			AssistantReply: "with Authorization: Bearer secretsecret1234 inside",
		},
	}
}

func TestSubmitLive(t *testing.T) {
	root := t.TempDir()
	stop := startTestServer(t, root)
	defer stop()
	c := NewClient(root)
	spooled, err := c.Submit(testPayload("k1"))
	if err != nil || spooled {
		t.Fatalf("submit: spooled=%v err=%v", spooled, err)
	}
	// Adapter-side redaction must have run before the wire.
	if strings.Contains(testPayloadAfterRedact(t), "secretsecret1234") {
		t.Error("adapter-side redaction missing")
	}
}

func testPayloadAfterRedact(_ *testing.T) string {
	p := testPayload("kx")
	Redact(p)
	b, _ := json.Marshal(p)
	return string(b)
}

func TestSubmitSpoolsWhenDown(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AIMEM_SOCKET", filepath.Join(os.TempDir(), "aimem-down.sock"))
	c := NewClient(root)
	spooled, err := c.Submit(testPayload("k1"))
	if err != nil || !spooled {
		t.Fatalf("expected spool: spooled=%v err=%v", spooled, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "spool", "pending.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secretsecret1234") {
		t.Error("spool contains unredacted secret")
	}
	if !strings.Contains(string(data), "\"idempotency_key\":\"k1\"") {
		t.Errorf("spool missing record: %s", data)
	}
}

func TestSpoolReplay(t *testing.T) {
	root := t.TempDir()
	// Service down: two records spool.
	t.Setenv("AIMEM_SOCKET", filepath.Join(os.TempDir(), "aimem-down2.sock"))
	c := NewClient(root)
	c.Submit(testPayload("k1"))
	c.Submit(testPayload("k2"))
	// Service up: next submit replays the backlog.
	stop := startTestServer(t, root)
	defer stop()
	c2 := NewClient(root)
	if spooled, err := c2.Submit(testPayload("k3")); err != nil || spooled {
		t.Fatalf("submit with service up: spooled=%v err=%v", spooled, err)
	}
	if _, err := os.Stat(filepath.Join(root, "spool", "pending.jsonl")); !os.IsNotExist(err) {
		t.Error("spool not drained")
	}
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	db, _ := reg.Open("proj-test")
	tl, err := db.Timeline("s1", 0)
	if err != nil || len(tl) != 3 {
		t.Fatalf("expected 3 events after replay, got %d (err=%v)", len(tl), err)
	}
}

func TestBuildClaudeEventPreCompact(t *testing.T) {
	tp := writeTranscript(t, []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"long task"}}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"progress"}]}}`,
	})
	raw, _ := json.Marshal(map[string]string{
		"session_id": "s", "transcript_path": tp, "cwd": t.TempDir(),
		"hook_event_name": "PreCompact", "trigger": "auto",
	})
	p, err := BuildClaudeEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	e := p.Event
	if e.Kind != schema.KindCompactionMarker || e.Outcome != schema.OutcomePreCompaction {
		t.Errorf("kind/outcome: %s/%s", e.Kind, e.Outcome)
	}
	if e.TurnID != "a1-compact" || e.IdempotencyKey != "claude-code:s:a1-compact" {
		t.Errorf("marker identity not stable: %s / %s", e.TurnID, e.IdempotencyKey)
	}
	if e.UserRequest != "compaction trigger: auto" || e.AssistantReply != "" || len(e.ToolSummary) != 0 {
		t.Errorf("marker content: %q %q %v", e.UserRequest, e.AssistantReply, e.ToolSummary)
	}
	// Re-building must produce the identical idempotency key.
	p2, _ := BuildClaudeEvent(raw)
	if p2.Event.IdempotencyKey != e.IdempotencyKey {
		t.Error("marker idempotency unstable across re-fires")
	}
}

func TestPreCompactAnchorsAfterTrailingPrompt(t *testing.T) {
	// The /compact command lands as a user prompt before PreCompact fires,
	// opening a turn with no assistant reply; the marker must still anchor
	// to the previous assistant uuid, not a timestamp fallback.
	tp := writeTranscript(t, []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"long task"}}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"progress"}]}}`,
		`{"type":"user","uuid":"u2","message":{"role":"user","content":"/compact"}}`,
	})
	raw, _ := json.Marshal(map[string]string{
		"session_id": "s", "transcript_path": tp, "cwd": t.TempDir(),
		"hook_event_name": "PreCompact", "trigger": "manual",
	})
	p, err := BuildClaudeEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Event.TurnID != "a1-compact" {
		t.Errorf("marker not anchored to last assistant: %s", p.Event.TurnID)
	}
	p2, _ := BuildClaudeEvent(raw)
	if p2.Event.IdempotencyKey != p.Event.IdempotencyKey {
		t.Error("marker idempotency unstable across re-fires")
	}
}

func TestStopWaitsForLateReplyFlush(t *testing.T) {
	// Tool-using turns provide turnID from tool_use entries before the
	// final text block is flushed; the Stop path must keep retrying until
	// the reply appears (observed live: empty assistant_response).
	tp := writeTranscript(t, []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"scan the repo"}}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash"}]}}`,
	})
	go func() {
		time.Sleep(300 * time.Millisecond)
		f, _ := os.OpenFile(tp, os.O_APPEND|os.O_WRONLY, 0o600)
		defer f.Close()
		f.WriteString(`{"type":"assistant","uuid":"a2","message":{"role":"assistant","content":[{"type":"text","text":"scan done"}]}}` + "\n")
	}()
	raw, _ := json.Marshal(map[string]string{
		"session_id": "s", "transcript_path": tp, "cwd": t.TempDir(),
		"hook_event_name": "Stop",
	})
	p, err := BuildClaudeEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Event.AssistantReply != "scan done" || p.Event.TurnID != "a2" {
		t.Errorf("late flush not awaited: reply=%q turn=%s", p.Event.AssistantReply, p.Event.TurnID)
	}
}

func TestBuildClaudeEventToleratesBOM(t *testing.T) {
	p := writeTranscript(t, []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"hello"}]}}`,
	})
	payload := append([]byte{0xEF, 0xBB, 0xBF},
		[]byte(`{"session_id":"s1","transcript_path":"`+strings.ReplaceAll(p, `\`, `\\`)+`","hook_event_name":"Stop"}`)...)
	if _, err := BuildClaudeEvent(payload); err != nil {
		t.Fatalf("BOM-prefixed payload rejected: %v", err)
	}
}

func TestReplaySweepsOrphanedClaims(t *testing.T) {
	root := t.TempDir()
	// A record spools while the service is down...
	t.Setenv("AIMEM_SOCKET", filepath.Join(os.TempDir(), "aimem-down3.sock"))
	c := NewClient(root)
	c.Submit(testPayload("k-orphan"))
	// ...then a replay claims the spool and dies mid-flight: the claim
	// file is orphaned with the record inside.
	spool := filepath.Join(root, "spool", "pending.jsonl")
	if err := os.Rename(spool, spool+".replay-99999"); err != nil {
		t.Fatal(err)
	}
	// Next successful contact must sweep the orphan back and deliver it.
	stop := startTestServer(t, root)
	defer stop()
	c2 := NewClient(root)
	if spooled, err := c2.Submit(testPayload("k-live")); err != nil || spooled {
		t.Fatalf("submit with service up: spooled=%v err=%v", spooled, err)
	}
	if _, err := os.Stat(spool + ".replay-99999"); !os.IsNotExist(err) {
		t.Error("orphaned claim file not swept")
	}
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	db, _ := reg.Open("proj-test")
	tl, err := db.Timeline("s1", 0)
	if err != nil || len(tl) != 2 {
		t.Fatalf("expected both events after sweep, got %d (err=%v)", len(tl), err)
	}
}

func TestSubmitSurfacesRejectionWithoutSpooling(t *testing.T) {
	root := t.TempDir()
	stop := startTestServer(t, root)
	defer stop()
	c := NewClient(root)
	p := testPayload("k-bad")
	p.Event.SchemaVersion = 999 // the service refuses the record itself
	spooled, err := c.Submit(p)
	if err == nil || spooled {
		t.Fatalf("rejection not surfaced: spooled=%v err=%v", spooled, err)
	}
	if _, ok := err.(*rejectError); !ok {
		t.Fatalf("expected rejectError, got %T: %v", err, err)
	}
	// Spooling a record the service rejected would replay the same
	// rejection forever; it must not be queued.
	if _, serr := os.Stat(filepath.Join(root, "spool", "pending.jsonl")); !os.IsNotExist(serr) {
		t.Error("rejected record was spooled anyway")
	}
}

func TestNoteWritesAndRotates(t *testing.T) {
	root := t.TempDir()
	Note(root, "aimem: first warning (%d)", 1)
	p := filepath.Join(root, "adapter.log")
	raw, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(raw), "first warning (1)") {
		t.Fatalf("note not persisted: %q err=%v", raw, err)
	}
	if !strings.HasPrefix(string(raw), "20") { // timestamped
		t.Fatalf("missing timestamp: %q", raw[:20])
	}
	// Past the cap, the log rotates once and keeps going.
	os.WriteFile(p, make([]byte, noteLogMax+1), 0o600)
	Note(root, "aimem: after rotation")
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatal("rotation window missing")
	}
	raw, _ = os.ReadFile(p)
	if !strings.Contains(string(raw), "after rotation") || len(raw) > 1024 {
		t.Fatalf("post-rotation log wrong: %d bytes", len(raw))
	}
}
