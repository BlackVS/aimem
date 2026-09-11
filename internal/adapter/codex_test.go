package adapter

// Rollout fixture lines below reproduce the REAL shapes captured live
// from codex-cli 0.153 on 2026-09-11 (a probe hook dumped the Stop and
// SessionStart stdin payloads; the rollout lines come from
// ~/.codex/sessions). Notable quirks the shapes pin: user item content
// uses "text" but agent item content uses "Text"; response_item lines
// carry no turn id, only event_msg lines do.

import (
	"encoding/json"
	"testing"

	"aimem/internal/schema"
)

func codexRolloutTwoTurns(t *testing.T) string {
	return writeTranscript(t, []string{
		`{"timestamp":"2026-09-11T07:56:24.000Z","ordinal":0,"type":"session_meta","payload":{"session_id":"sess-c","cwd":"C:\\w","originator":"codex_exec","cli_version":"0.153.4"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-1"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-1","item":{"type":"UserMessage","id":"i1","content":[{"type":"text","text":"old request"}]}}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","status":"completed","name":"exec","input":"dir"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-1","item":{"type":"AgentMessage","id":"m1","content":[{"type":"Text","text":"old reply"}]}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-1","last_agent_message":"old reply"}}`,
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-2"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-2","item":{"type":"UserMessage","id":"i2","content":[{"type":"text","text":"fix the bug"}]}}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"commentary"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"read_file","arguments":"{}"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"go test"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-2","item":{"type":"Reasoning","id":"r1","summary_text":[]}}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-2","item":{"type":"AgentMessage","id":"m2","content":[{"type":"Text","text":"fixed and tested"}]}}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-2","last_agent_message":"fixed and tested"}}`,
		`not even json`,
	})
}

func TestBuildCodexEvent(t *testing.T) {
	tp := codexRolloutTwoTurns(t)
	raw, _ := json.Marshal(map[string]any{
		"session_id": "sess-c", "turn_id": "turn-2", "transcript_path": tp,
		"cwd": t.TempDir(), "hook_event_name": "Stop",
		"stop_hook_active": false, "last_assistant_message": "fixed and tested",
	})
	p, err := BuildCodexEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	e := p.Event
	if e.Client != "codex" {
		t.Errorf("client: %q", e.Client)
	}
	if e.UserRequest != "fix the bug" {
		t.Errorf("user request: %q (must be turn-2's, not turn-1's)", e.UserRequest)
	}
	if e.AssistantReply != "fixed and tested" {
		t.Errorf("reply: %q", e.AssistantReply)
	}
	// turn-1's exec call must not bleed into turn-2's tool list.
	if len(e.ToolSummary) != 2 || e.ToolSummary[0] != "read_file" || e.ToolSummary[1] != "exec" {
		t.Errorf("tools: %v", e.ToolSummary)
	}
	if e.TurnID != "turn-2" || e.IdempotencyKey != "codex:sess-c:turn-2" {
		t.Errorf("turn identity: %s / %s", e.TurnID, e.IdempotencyKey)
	}
	if e.Kind != schema.KindTurn || e.Outcome != schema.OutcomeOK {
		t.Errorf("kind/outcome: %s/%s", e.Kind, e.Outcome)
	}
	if err := e.Validate(); err != nil {
		t.Errorf("built event invalid: %v", err)
	}
}

func TestBuildCodexEventPayloadReplyWins(t *testing.T) {
	// The rollout may not have flushed the final AgentMessage when Stop
	// fires; the payload's last_assistant_message is authoritative.
	tp := writeTranscript(t, []string{
		`{"type":"event_msg","payload":{"type":"task_started","turn_id":"t1"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"t1","item":{"type":"UserMessage","id":"i1","content":[{"type":"text","text":"ask"}]}}}`,
	})
	raw, _ := json.Marshal(map[string]any{
		"session_id": "s", "turn_id": "t1", "transcript_path": tp,
		"cwd": t.TempDir(), "hook_event_name": "Stop",
		"last_assistant_message": "late reply",
	})
	p, err := BuildCodexEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Event.AssistantReply != "late reply" {
		t.Errorf("reply: %q", p.Event.AssistantReply)
	}
	if p.Event.UserRequest != "ask" {
		t.Errorf("user request: %q", p.Event.UserRequest)
	}
}

func TestBuildCodexEventPreCompact(t *testing.T) {
	tp := codexRolloutTwoTurns(t)
	raw, _ := json.Marshal(map[string]any{
		"session_id": "sess-c", "transcript_path": tp, // no turn_id
		"cwd": t.TempDir(), "hook_event_name": "PreCompact", "trigger": "auto",
	})
	p, err := BuildCodexEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	e := p.Event
	if e.Kind != schema.KindCompactionMarker || e.Outcome != schema.OutcomePreCompaction {
		t.Errorf("kind/outcome: %s/%s", e.Kind, e.Outcome)
	}
	// Anchored to the rollout's last turn so a re-fired hook dedupes.
	if e.TurnID != "turn-2-compact" {
		t.Errorf("turn id: %q", e.TurnID)
	}
	if e.UserRequest != "compaction trigger: auto" || e.AssistantReply != "" || e.ToolSummary != nil {
		t.Errorf("marker content leaked: %q / %q / %v", e.UserRequest, e.AssistantReply, e.ToolSummary)
	}
	p2, _ := BuildCodexEvent(raw)
	if p2.Event.IdempotencyKey != e.IdempotencyKey {
		t.Errorf("re-fired PreCompact changed key: %s vs %s", p2.Event.IdempotencyKey, e.IdempotencyKey)
	}
}

func TestBuildCodexEventRejectsIncomplete(t *testing.T) {
	for _, raw := range []string{
		`{"turn_id":"t","transcript_path":"x"}`,
		`{"session_id":"s","turn_id":"t"}`,
		`no json`,
	} {
		if _, err := BuildCodexEvent([]byte(raw)); err == nil {
			t.Errorf("payload %q accepted", raw)
		}
	}
}

func TestBuildCodexEventBOMTolerant(t *testing.T) {
	tp := codexRolloutTwoTurns(t)
	raw, _ := json.Marshal(map[string]any{
		"session_id": "s", "turn_id": "turn-2", "transcript_path": tp,
		"cwd": t.TempDir(), "hook_event_name": "Stop",
	})
	withBOM := append([]byte{0xEF, 0xBB, 0xBF}, raw...)
	if _, err := BuildCodexEvent(withBOM); err != nil {
		t.Fatalf("BOM payload rejected: %v", err)
	}
}
