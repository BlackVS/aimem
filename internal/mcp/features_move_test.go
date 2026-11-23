package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestFeatureMoveThroughTheTool moves a feature to the next goal with
// set_feature, as the architect's session does through the MCP tools alone,
// and reads the move back from list_features and get_task_history.
func TestFeatureMoveThroughTheTool(t *testing.T) {
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
	reached := must("create_epic", map[string]any{"title": "the reached goal", "idempotency_key": "reached"})["id"].(string)
	next := must("create_epic", map[string]any{"title": "the next goal", "idempotency_key": "next"})["id"].(string)
	feature := must("create_task", map[string]any{"title": "Computed views", "epic": reached,
		"feature": map[string]any{"goal": reached, "position": 2, "mark": "optional", "alternative": "counted by hand"}, "idempotency_key": "feature"})["id"].(string)
	work := must("create_task", map[string]any{"title": "The tier as a value", "epic": reached, "idempotency_key": "work"})["id"].(string)
	must("confirm_feature_task", map[string]any{"id": feature, "task": work, "expected_revision": 1, "idempotency_key": "confirm"})

	// A move states its mark: none is taken from the link the feature had.
	if text, isErr := call("set_feature", map[string]any{"id": feature, "goal": next, "position": 2, "expected_revision": 2, "idempotency_key": "no-mark"}); !isErr || !strings.Contains(text, "mark must be mandatory or optional") {
		t.Fatalf("a move without a mark: %s", text)
	}

	rec := must("set_feature", map[string]any{"id": feature, "goal": next, "position": 2, "mark": "mandatory", "ground": "check",
		"ground_note": "the second part of the goal's check", "expected_revision": 2, "idempotency_key": "move"})
	link, _ := rec["feature"].(map[string]any)
	if rec["id"] != feature || rec["epic"] != next || link["goal"] != next || link["mark"] != "mandatory" || link["ground"] != "check" || link["alternative"] != nil {
		t.Fatalf("set_feature with the next goal: %v", rec)
	}
	if deps, _ := rec["dependencies"].([]any); len(deps) != 1 || deps[0] != work {
		t.Fatalf("the confirmed tasks after the move: %v", rec)
	}
	if rows, _ := must("list_features", map[string]any{"epic": next})["features"].([]any); len(rows) != 1 || rows[0].(map[string]any)["id"] != feature {
		t.Fatalf("list_features on the next goal: %v", rows)
	}
	if rows, _ := must("list_features", map[string]any{"epic": reached})["features"].([]any); len(rows) != 0 {
		t.Fatalf("list_features on the reached goal: %v", rows)
	}
	changes, _ := must("get_task_history", map[string]any{"id": feature})["changes"].([]any)
	if len(changes) != 3 || changes[2].(map[string]any)["moved_from"] != reached {
		t.Fatalf("the move's history entry: %v", changes)
	}
}
