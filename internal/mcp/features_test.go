package mcp

// The feature tools (docs/DESIGN-feature-record.md, sections 6 to 8): the
// goal's check performed by one standalone session through the MCP facade,
// with one ordinary token and no team, and what a client is told by a hub
// one release older.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestFeatureGoalCheckStandalone runs the steps of the design's section 8
// with alice's ordinary project token: no team, no aicrewd, no coordinator.
func TestFeatureGoalCheckStandalone(t *testing.T) {
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
	goal := must("create_epic", map[string]any{"title": "the goal", "idempotency_key": "goal"})["id"].(string)

	// 1. The features are set up: feature 1 mandatory on the goal's check,
	// features 2 to 4 optional, each with its alternative.
	ids := []string{}
	for i, c := range []struct {
		title string
		link  map[string]any
	}{
		{"The goal's check passes", map[string]any{"mark": "mandatory", "ground": "check", "ground_note": "it is the goal's check"}},
		{"Helpers for the mechanical steps", map[string]any{"mark": "optional", "alternative": "the steps are done by hand"}},
		{"The board shows features", map[string]any{"mark": "optional", "alternative": "list_features is read in the session"}},
		{"The process set carries the tiers", map[string]any{"mark": "optional", "alternative": "the architect applies them by hand"}},
	} {
		c.link["goal"], c.link["position"] = goal, i+1
		rec := must("create_task", map[string]any{"title": c.title, "epic": goal, "state": "READY", "acceptance_criteria": "its check passes",
			"feature": c.link, "idempotency_key": "feature-" + c.title})
		if rec["kind"] != "feature" {
			t.Fatalf("create_task with a link: %v", rec)
		}
		ids = append(ids, rec["id"].(string))
	}
	design := must("create_task", map[string]any{"title": "The design", "epic": goal, "state": "DONE", "idempotency_key": "design"})["id"].(string)

	// 2. A task is confirmed on feature 1, both named by a short prefix
	// that runs past the millisecond (records made in one millisecond
	// share their first 13 characters).
	conf := must("confirm_feature_task", map[string]any{"id": ids[0][:18], "task": design[:18], "expected_revision": 1, "idempotency_key": "confirm"})
	if deps, _ := conf["dependencies"].([]any); len(deps) != 1 || deps[0] != design {
		t.Fatalf("confirm_feature_task: %v", conf)
	}

	// 3. The goal's features read back in order.
	list := must("list_features", map[string]any{"epic": goal})
	rows, _ := list["features"].([]any)
	if len(rows) != 4 {
		t.Fatalf("list_features: %v", list)
	}
	for i, r := range rows {
		row := r.(map[string]any)
		link := row["feature"].(map[string]any)
		if row["id"] != ids[i] || link["position"] != float64(i+1) || (i == 0) != (link["mark"] == "mandatory") || (i == 0) != (row["confirmed_tasks"] == 1.0) {
			t.Fatalf("row %d: %v", i, row)
		}
	}

	must("create_task", map[string]any{"title": "Unrelated work", "idempotency_key": "unrelated"})
	// 4. The feature's tasks: list_tasks with feature returns the design.
	page := must("list_tasks", map[string]any{"feature": ids[0][:18]})
	tasks, _ := page["tasks"].([]any)
	if len(tasks) != 1 || tasks[0].(map[string]any)["id"] != design {
		t.Fatalf("list_tasks with feature: %v", page)
	}
	// A plain list is work only; a search finds features by their words.
	page = must("list_tasks", map[string]any{})
	if tasks, _ := page["tasks"].([]any); len(tasks) != 2 {
		t.Fatalf("a plain list: %v", page)
	}
	// kind=feature lists the features, with counts of features.
	page = must("list_tasks", map[string]any{"kind": "feature"})
	if tasks, _ := page["tasks"].([]any); len(tasks) != 4 || page["counts"].(map[string]any)["total"] != 4.0 {
		t.Fatalf("list_tasks kind=feature: %v", page)
	}
	page = must("search_tasks", map[string]any{"query": "board"})
	if tasks, _ := page["tasks"].([]any); len(tasks) != 1 || tasks[0].(map[string]any)["kind"] != "feature" {
		t.Fatalf("a search: %v", page)
	}

	// 5. Feature 1 moves to DONE with evidence, and is refused without it.
	done := map[string]any{"id": ids[0], "title": "The goal's check passes", "epic": goal, "state": "DONE", "acceptance_criteria": "its check passes",
		"dependencies": []string{design}, "expected_revision": 2, "idempotency_key": "done-bare"}
	if text, isErr := call("update_task", done); !isErr || !strings.Contains(text, "evidence") {
		t.Fatalf("DONE without evidence: %v %s", isErr, text)
	}
	done["evidence_refs"] = []map[string]any{{"kind": "text", "ref": "steps 1 to 4 ran through the MCP facade"}}
	done["idempotency_key"] = "done-evidence"
	rec := must("update_task", done)
	if rec["state"] != "DONE" || rec["kind"] != "feature" || rec["feature"] == nil {
		t.Fatalf("DONE with evidence: %v", rec)
	}

	// An archived feature is listed only when asked.
	done["archived"], done["expected_revision"], done["idempotency_key"] = true, 3, "archive"
	must("update_task", done)
	if rows, _ := must("list_features", map[string]any{"epic": goal})["features"].([]any); len(rows) != 3 {
		t.Fatalf("an archived feature is listed: %v", rows)
	}
	if rows, _ := must("list_features", map[string]any{"epic": goal, "include_archived": true})["features"].([]any); len(rows) != 4 {
		t.Fatalf("include_archived: %v", rows)
	}

	// set_feature: turn a task into a feature, and back.
	extra := must("create_task", map[string]any{"title": "A late feature", "epic": goal, "idempotency_key": "late"})["id"].(string)
	rec = must("set_feature", map[string]any{"id": extra, "goal": goal, "position": 5, "mark": "optional", "alternative": "none",
		"expected_revision": 1, "idempotency_key": "late-set"})
	if rec["kind"] != "feature" {
		t.Fatalf("set_feature: %v", rec)
	}
	if text, isErr := call("set_feature", map[string]any{"id": extra, "remove": true, "goal": goal, "expected_revision": 2, "idempotency_key": "late-mixed"}); !isErr || !strings.Contains(text, "remove takes no link fields") {
		t.Fatalf("remove with a link field: %s", text)
	}
	rec = must("set_feature", map[string]any{"id": extra, "remove": true, "expected_revision": 2, "idempotency_key": "late-remove"})
	if _, ok := rec["kind"]; ok {
		t.Fatalf("set_feature remove: %v", rec)
	}
	if text, isErr := call("confirm_feature_task", map[string]any{"id": ids[0], "expected_revision": 4, "idempotency_key": "no-task"}); !isErr || !strings.Contains(text, "task is required: the task to confirm on the feature") {
		t.Fatalf("confirm without a task: %s", text)
	}
	if text, isErr := call("list_features", map[string]any{}); !isErr || !strings.Contains(text, "epic is required") {
		t.Fatalf("list_features without an epic: %s", text)
	}
	if text, isErr := call("list_tasks", map[string]any{"feature": map[string]any{"goal": goal}}); !isErr || !strings.Contains(text, "feature's id") {
		t.Fatalf("a link as the list's feature: %s", text)
	}
}

// olderHub answers as a hub one release older does: it does not route the
// feature paths, and its strict decoder refuses a create with a link.
func olderHub(ordinary bool, seen *[]string) TaskCallFunc {
	return func(_ context.Context, method, path string, _ map[string]string, body []byte) (int, []byte, error) {
		*seen = append(*seen, method+" "+path)
		switch {
		case strings.Contains(path, "/feature") && ordinary:
			return http.StatusForbidden, []byte(`{"error":"ordinary token is not authorized for this endpoint"}` + "\n"), nil
		case strings.Contains(path, "/feature"):
			return http.StatusNotFound, []byte("404 page not found\n"), nil
		case method == "POST" && strings.HasSuffix(path, "/tasks") && strings.Contains(string(body), `"feature"`):
			return http.StatusBadRequest, []byte(`{"error":"invalid JSON body: json: unknown field \"feature\""}` + "\n"), nil
		}
		return http.StatusOK, []byte(`{"project":"alpha","tasks":[]}`), nil
	}
}

func TestFeatureToolsOnAnOlderHub(t *testing.T) {
	for _, ordinary := range []bool{true, false} {
		var seen []string
		s := &srv{project: "alpha", tasks: olderHub(ordinary, &seen)}
		for tool, args := range map[string]map[string]any{
			"set_feature":          {"id": "01a1e56e-4bed-7000-ab2f-47753e8623bf", "goal": "g", "position": 1, "mark": "optional", "expected_revision": 1, "idempotency_key": "k"},
			"confirm_feature_task": {"id": "01a1e56e-4bed-7000-ab2f-47753e8623bf", "task": "01a105fa-336e", "expected_revision": 1, "idempotency_key": "k"},
			"list_features":        {"epic": "01a186a3-12c3-7000-b23d-83d1cad3dad1"},
		} {
			if text, isErr := callTool(t, s, tool, args); !isErr || text != "error: "+errOlderHub {
				t.Errorf("%s on an older hub (ordinary %v): %v %q", tool, ordinary, isErr, text)
			}
		}
		text, isErr := callTool(t, s, "create_task", map[string]any{"title": "f", "idempotency_key": "k",
			"feature": map[string]any{"goal": "g", "position": 1, "mark": "optional"}})
		if !isErr || text != "error: "+errOlderHub+"; nothing was created" {
			t.Errorf("create_task with a link on an older hub: %v %q", isErr, text)
		}
		// A plain create and a list are not taken for an older hub.
		if text, isErr := callTool(t, s, "list_tasks", map[string]any{}); isErr {
			t.Errorf("list_tasks: %s", text)
		}
	}
	// Another tool's plain 404 keeps its own answer.
	s := &srv{project: "alpha", tasks: func(context.Context, string, string, map[string]string, []byte) (int, []byte, error) {
		return http.StatusNotFound, []byte("404 page not found\n"), nil
	}}
	if text, _ := callTool(t, s, "get_task", map[string]any{"id": "01a1e56e-4bed-7000-ab2f-47753e8623bf"}); text != "error: the hub does not serve task routes (upgrade the hub)" {
		t.Errorf("get_task on a hub without task routes: %q", text)
	}
}
