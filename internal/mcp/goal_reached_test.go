package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGoalReachedThroughTheTools writes a goal's check, closes the goal as
// REACHED and reads the mark and the count, through the MCP tools alone.
func TestGoalReachedThroughTheTools(t *testing.T) {
	f := newHub(t)
	call := func(tool string, args map[string]any) (string, bool) {
		t.Helper()
		args["project"] = "alpha"
		return toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": tool, "arguments": args}))
	}
	must := func(tool string, args map[string]any) map[string]any {
		t.Helper()
		text, isErr := call(tool, args)
		var out map[string]any
		if isErr || json.Unmarshal([]byte(text), &out) != nil {
			t.Fatalf("%s %v: %s", tool, args, text)
		}
		return out
	}
	const check = "a fresh session delivers one task"
	goal := must("create_epic", map[string]any{"title": "the goal", "check": check, "idempotency_key": "goal"})
	id := goal["id"].(string)
	if goal["check"] != check {
		t.Fatalf("create_epic with a check: %v", goal)
	}
	// update_epic without check and state keeps both.
	e := must("update_epic", map[string]any{"id": id, "title": "the goal, renamed", "expected_revision": 1, "idempotency_key": "rename"})
	if e["check"] != check || e["state"] != "OPEN" {
		t.Fatalf("update_epic without check or state: %v", e)
	}

	// A feature has no check of its own: the task tools refuse the argument
	// instead of dropping it.
	for _, tool := range []map[string]any{
		{"name": "create_task", "args": map[string]any{"title": "f", "epic": id, "check": "x", "idempotency_key": "chk-1",
			"feature": map[string]any{"goal": id, "position": 1, "mark": "optional"}}},
		{"name": "update_task", "args": map[string]any{"id": id, "title": "f", "check": "x", "expected_revision": 1, "idempotency_key": "chk-2"}},
		{"name": "set_feature", "args": map[string]any{"id": id, "goal": id, "position": 1, "mark": "optional", "check": "x", "expected_revision": 1, "idempotency_key": "chk-3"}},
	} {
		if text, isErr := call(tool["name"].(string), tool["args"].(map[string]any)); !isErr || !strings.Contains(text, "check belongs to an epic") {
			t.Fatalf("%s with a check must be refused: %s", tool["name"], text)
		}
	}
	if rows, _ := must("list_tasks", map[string]any{"kind": "any"})["tasks"].([]any); len(rows) != 0 {
		t.Fatalf("a refused create_task stored nothing: %v", rows)
	}

	feature := must("create_task", map[string]any{"title": "the feature", "epic": id,
		"feature": map[string]any{"goal": id, "position": 1, "mark": "optional"}, "idempotency_key": "feature"})["id"].(string)
	work := must("create_task", map[string]any{"title": "the work", "state": "DONE", "epic": id, "idempotency_key": "work"})["id"].(string)
	must("create_task", map[string]any{"title": "the goal's log", "epic": id, "idempotency_key": "log"})
	must("confirm_feature_task", map[string]any{"id": feature, "task": work, "expected_revision": 1, "idempotency_key": "confirm"})

	l := must("list_features", map[string]any{"epic": id})
	rows, _ := l["features"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["ready_to_close"] != true || l["tasks_on_no_feature"] != float64(1) {
		t.Fatalf("list_features: the mark and the count: %v", l)
	}

	if text, isErr := call("update_epic", map[string]any{"id": id, "title": "the goal", "state": "REACHED", "expected_revision": 2, "idempotency_key": "reach-1"}); !isErr || !strings.Contains(text, feature) {
		t.Fatalf("REACHED with an open feature, named: %s", text)
	}
	must("update_task", map[string]any{"id": feature, "title": "the feature", "state": "DONE", "epic": id, "dependencies": []string{work},
		"evidence_refs": []map[string]any{{"kind": "url", "ref": "https://example.com/ci/1"}}, "expected_revision": 2, "idempotency_key": "done"})
	if e := must("update_epic", map[string]any{"id": id, "title": "the goal", "state": "REACHED", "expected_revision": 2, "idempotency_key": "reach-2"}); e["state"] != "REACHED" || e["check"] != check {
		t.Fatalf("REACHED: %v", e)
	}
	if epics, _ := must("list_epics", map[string]any{})["epics"].([]any); len(epics) != 0 {
		t.Fatalf("list_epics leaves a reached goal out: %v", epics)
	}
	if epics, _ := must("list_epics", map[string]any{"include_retired": true})["epics"].([]any); len(epics) != 1 {
		t.Fatalf("list_epics with include_retired: %v", epics)
	}
	if text, isErr := call("create_task", map[string]any{"title": "late", "epic": id, "idempotency_key": "late"}); !isErr || !strings.Contains(text, "reached goal") {
		t.Fatalf("a new task in a reached goal: %s", text)
	}
	// An empty check clears it.
	if e := must("update_epic", map[string]any{"id": id, "title": "the goal", "check": "", "expected_revision": 3, "idempotency_key": "clear"}); e["check"] != "" || e["state"] != "REACHED" {
		t.Fatalf("an empty check clears it, the state stays: %v", e)
	}
}
