package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// The member reservation tools through the hub's real /mcp and routes: the
// same bodies as HTTP, the wire's outcome, and a refusal typed as the
// hub's envelope.
func TestReservationToolsThroughTheHub(t *testing.T) {
	f := newHub(t)
	call := func(name string, args map[string]any) (string, bool) {
		t.Helper()
		return toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": name, "arguments": args}))
	}
	text, isErr := call("create_task", map[string]any{"project": "alpha", "title": "reserve me", "state": "READY", "idempotency_key": "rt-create"})
	if isErr {
		t.Fatalf("create_task: %s", text)
	}
	var task struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	json.Unmarshal([]byte(text), &task)

	envelope := func(what, text string, isErr bool, code string) {
		t.Helper()
		var e struct {
			Code          string `json:"code"`
			Retryable     *bool  `json:"retryable"`
			NextAction    string `json:"next_action"`
			CorrelationID string `json:"correlation_id"`
		}
		if !isErr || json.Unmarshal([]byte(text), &e) != nil || e.Code != code || e.Retryable == nil || e.NextAction == "" || e.CorrelationID == "" {
			t.Fatalf("%s: want the %s envelope, got %v %s", what, code, isErr, text)
		}
	}
	claim := map[string]any{"version": 1, "project": "alpha", "task_id": task.ID, "request_key": "mcp-claim",
		"expected_revision": task.Revision, "holder": map[string]any{"mode": "standalone", "work_ref": "mcp-work"}}
	bad := map[string]any{}
	for k, v := range claim {
		bad[k] = v
	}
	bad["version"] = 2
	text, isErr = call("task_reservation_claim", bad)
	envelope("version 2", text, isErr, "unsupported_version")

	text, isErr = call("task_reservation_claim", claim)
	var o struct {
		Receipt struct {
			ID           string `json:"id"`
			Replayed     bool   `json:"replayed"`
			VerifiedMode string `json:"verified_mode"`
		} `json:"receipt"`
		TaskRevision int64 `json:"task_revision"`
		Reservation  struct {
			ID    string `json:"id"`
			Fence string `json:"fence"`
		} `json:"reservation"`
	}
	if isErr || json.Unmarshal([]byte(text), &o) != nil || o.Reservation.Fence != "1" || o.Receipt.VerifiedMode != "personal" {
		t.Fatalf("claim: %v %s", isErr, text)
	}
	firstID := o.Receipt.ID
	if text, isErr = call("task_reservation_claim", claim); isErr || json.Unmarshal([]byte(text), &o) != nil || !o.Receipt.Replayed || o.Receipt.ID != firstID {
		t.Fatalf("claim replay: %v %s", isErr, text)
	}
	if text, isErr = call("task_reservation_status", map[string]any{"version": 1, "project": "alpha", "task_id": task.ID}); isErr ||
		!strings.Contains(text, `"state":"held"`) {
		t.Fatalf("status: %v %s", isErr, text)
	}
	update := map[string]any{"version": 1, "project": "alpha", "task_id": task.ID, "request_key": "mcp-update",
		"expected_revision": o.TaskRevision, "reservation_id": o.Reservation.ID, "fence": "1", "intent": "submit",
		"content": map[string]any{"title": "reserve me", "state": "IN_PROGRESS"}}
	if text, isErr = call("task_reservation_update", update); isErr || !strings.Contains(text, `"fence":"2"`) {
		t.Fatalf("update: %v %s", isErr, text)
	}
	text, isErr = call("task_reservation_update", update2(update, "mcp-stale"))
	envelope("an update on the old fence", text, isErr, "stale_fence")
	if text, isErr = call("task_reservation_receipt", map[string]any{"version": 1, "project": "alpha", "task_id": task.ID,
		"operation": "update", "request_key": "mcp-update"}); isErr || !strings.Contains(text, `"state":"committed"`) {
		t.Fatalf("receipt: %v %s", isErr, text)
	}
	finalize := map[string]any{"version": 1, "project": "alpha", "task_id": task.ID, "request_key": "mcp-final",
		"expected_revision": o.TaskRevision + 1, "reservation_id": o.Reservation.ID, "fence": "2", "reason": "delivered",
		"content": map[string]any{"title": "reserve me", "state": "DONE"}}
	text, isErr = call("task_reservation_finalize", finalize)
	envelope("DONE without evidence", text, isErr, "invalid_request")
	finalize["terminal_evidence"] = []string{"review-head", "human-merge"}
	if text, isErr = call("task_reservation_finalize", finalize); isErr || !strings.Contains(text, `"active":false`) {
		t.Fatalf("finalize: %v %s", isErr, text)
	}
	// An unknown argument is refused before the hub is asked.
	if text, isErr = call("task_reservation_status", map[string]any{"version": 1, "project": "alpha", "task_id": task.ID, "actor_id": "x"}); !isErr ||
		!strings.Contains(text, "arguments") {
		t.Fatalf("an actor argument: %v %s", isErr, text)
	}
}

// update2 is update under another key at the task's new revision, but on
// the fence the update already moved past.
func update2(update map[string]any, key string) map[string]any {
	out := map[string]any{}
	for k, v := range update {
		out[k] = v
	}
	out["request_key"] = key
	out["expected_revision"] = update["expected_revision"].(int64) + 1
	return out
}
