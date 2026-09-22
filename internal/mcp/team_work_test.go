package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestTeamWorkWireContract(t *testing.T) {
	for _, tc := range []struct {
		name, args, method, suffix string
		body                       []string // fields that must appear in the body
	}{
		{"team_reserved", ``, "GET", "/assignments/reserved?generation=1&session_id=s", nil},
		{"team_assignment", `"attempt":"a1"`, "GET", "/assignments/a1?generation=1&session_id=s", nil},
		{"team_offer", `"coordinator_generation":1,"task_id":"t1","expected_revision":1,"worker":{"session_id":"w","generation":1},"suitability_rationale":"S","cost_rationale":"C","idempotency_key":"k"`, "POST", "/assignments", []string{`"task_id":"t1"`, `"worker":{"session_id":"w","generation":1}`}},
		{"team_accept", `"attempt":"a1","idempotency_key":"k"`, "POST", "/assignments/a1/accept", []string{`"session_id":"s"`}},
		{"team_decline", `"attempt":"a1","reason":"r","idempotency_key":"k"`, "POST", "/assignments/a1/decline", []string{`"reason":"r"`}},
		{"team_block", `"attempt":"a1","expected_revision":2,"reason":"r","idempotency_key":"k"`, "POST", "/assignments/a1/block", []string{`"expected_revision":2`, `"reason":"r"`}},
		{"team_cancel", `"attempt":"a1","coordinator_generation":1,"expected_revision":2,"reason":"r","idempotency_key":"k"`, "POST", "/assignments/a1/cancel", []string{`"coordinator_generation":1`, `"reason":"r"`}},
		{"team_stopped", `"attempt":"a1","expected_revision":3,"reason":"r","idempotency_key":"k"`, "POST", "/assignments/a1/stopped", []string{`"expected_revision":3`}},
		{"team_withdraw", `"attempt":"a1","coordinator_generation":1,"reason":"r","idempotency_key":"k"`, "POST", "/assignments/a1/withdraw", []string{`"reason":"r"`}},
		{"team_resume_work", `"attempt":"a1","expected_revision":2,"reason":"r","idempotency_key":"k"`, "POST", "/assignments/a1/resume-work", []string{`"expected_revision":2`}},
		{"team_close_stop", `"attempt":"a1","coordinator_generation":1,"expected_revision":4,"reason":"r","idempotency_key":"k"`, "POST", "/assignments/a1/close-stop", []string{`"coordinator_generation":1`}},
		{"team_submit", `"attempt":"a1","expected_revision":2,"base_commit":"b","commit":"c","summary":"s","validation":"v","evidence_refs":[{"kind":"text","ref":"e"}],"idempotency_key":"k"`, "POST", "/assignments/a1/submit", []string{`"evidence_refs":[{"kind":"text","ref":"e"}]`}},
		{"team_review", `"attempt":"a1","coordinator_generation":1,"expected_revision":3,"result_id":"r1","decision":"accept","reason":"r","idempotency_key":"k"`, "POST", "/assignments/a1/review", []string{`"decision":"accept"`}},
		{"team_handoff", `"coordinator_generation":1,"target":{"session_id":"n","generation":1},"reason":"r","idempotency_key":"k"`, "POST", "/handoff", []string{`"target":{"session_id":"n","generation":1}`}},
		{"team_edit", `"task":"t1","coordinator_generation":1,"expected_revision":1,"content":{"title":"x","state":"READY"},"reason":"r","idempotency_key":"k"`, "POST", "/tasks/t1/edit", []string{`"content":{"title":"x","state":"READY"}`}},
		{"team_finalize", `"task":"t1","coordinator_generation":1,"expected_revision":4,"attempt_id":"a1","reason":"r","evidence":[{"kind":"text","ref":"e"}],"idempotency_key":"k"`, "POST", "/tasks/t1/finalize", []string{`"attempt_id":"a1"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			call := func(_ context.Context, method, path string, headers map[string]string, body []byte) (int, []byte, error) {
				called = true
				if method != tc.method || path != "/v1/projects/alpha/teams/t"+tc.suffix {
					t.Fatal(method, path)
				}
				if method == "POST" {
					if headers["Idempotency-Key"] != "k" {
						t.Fatal(headers)
					}
					for _, forbidden := range []string{`"idempotency_key"`, `"project"`, `"team"`, `"attempt"`, `"task"`} {
						if strings.Contains(string(body), forbidden) {
							t.Fatal("routing field forwarded in body", string(body))
						}
					}
					for _, want := range tc.body {
						if !strings.Contains(string(body), want) {
							t.Fatal(want, string(body))
						}
					}
				} else if body != nil {
					t.Fatal("read with body")
				}
				return 200, []byte(`{"protocol_version":1}`), nil
			}
			args := `"session_id":"s","generation":1`
			if tc.args != "" {
				args += "," + tc.args
			}
			raw := `{"project":"alpha","team":"t",` + args + `}`
			if _, err := callTeamTool(context.Background(), call, "", tc.name, json.RawMessage(raw)); err != nil || !called {
				t.Fatal(err)
			}
		})
	}
	// Unknown arguments, missing required fields and missing routing segments are refused before any request.
	sent := 0
	call := func(context.Context, string, string, map[string]string, []byte) (int, []byte, error) {
		sent++
		return 200, []byte(`{"protocol_version":1}`), nil
	}
	for _, raw := range []string{
		`{"project":"alpha","team":"t","session_id":"s","generation":1,"attempt":"a1","idempotency_key":"k","actor":"forged"}`,
		`{"project":"alpha","team":"t","session_id":"s","generation":1,"idempotency_key":"k"}`,
		`{"project":"alpha","team":"t","session_id":"s","generation":1,"attempt":"a1"}`,
		`[]`,
	} {
		if _, err := callTeamTool(context.Background(), call, "", "team_accept", json.RawMessage(raw)); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	if sent != 0 {
		t.Fatal("invalid request sent")
	}
	if len(teamWorkToolDefs) != 16 {
		t.Fatal("tool table changed", len(teamWorkToolDefs))
	}
}

// Every bridged tool has a wire-contract case above.
func TestTeamWorkWireMatrixCoversEveryTool(t *testing.T) {
	src, err := os.ReadFile("team_work_test.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range teamWorkTools {
		if !strings.Contains(string(src), `{"`+tool.name+`", `) {
			t.Fatal("no wire-contract case for", tool.name)
		}
	}
}

func TestTeamWorkThroughRealHub(t *testing.T) {
	f := newHub(t)
	setup := fmt.Sprintf(`{"name":"Builders","enrollment":[{"user_id":%q,"coordinator":true}]}`, f.aliceID)
	req := httptest.NewRequest("POST", "/v1/projects/alpha/teams", strings.NewReader(setup))
	req.Header.Set("Authorization", "Bearer "+f.env)
	req.Header.Set("Idempotency-Key", "setup")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	call := func(token, name string, a map[string]any) map[string]any {
		t.Helper()
		raw, bad := toolText(f.rpc(t, token, "tools/call", map[string]any{"name": name, "arguments": a}))
		if bad {
			t.Fatalf("%s: %s", name, raw)
		}
		if strings.Contains(raw, `"token_id"`) || strings.Contains(raw, `"user_id"`) {
			t.Fatalf("%s leaked bindings: %s", name, raw)
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	refuse := func(token, name string, a map[string]any) {
		t.Helper()
		if raw, bad := toolText(f.rpc(t, token, "tools/call", map[string]any{"name": name, "arguments": a})); !bad {
			t.Fatalf("%s accepted for %q: %s", name, token, raw)
		}
	}
	createTask := func(key string) (string, int64) {
		t.Helper()
		body := `{"title":"bounded change","state":"READY","objective":"prove the bridge","acceptance_criteria":"runtime proof"}`
		r := httptest.NewRequest("POST", "/v1/projects/alpha/tasks", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f.alice)
		r.Header.Set("Idempotency-Key", key)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		if w.Code != 201 {
			t.Fatal(w.Body.String())
		}
		var task struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
			t.Fatal(err)
		}
		return task.ID, task.Revision
	}
	readTask := func(id string) map[string]any {
		t.Helper()
		r := httptest.NewRequest("GET", "/v1/tasks/"+id, nil)
		r.Header.Set("Authorization", "Bearer "+f.alice)
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		var task map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
			t.Fatal(err)
		}
		return task
	}
	join := func(role, key string) (string, string) {
		out := call(f.alice, "team_join", map[string]any{"project": "alpha", "team": "Builders", "role": role, "profile": map[string]any{"label": key, "platform": "fixture", "platform_version": "1"}, "idempotency_key": key})
		s := out["session"].(map[string]any)
		return s["team_id"].(string), s["id"].(string)
	}
	team, co := join("coordinator", "co")
	_, worker := join("worker", "worker")
	coHandle := map[string]any{"project": "alpha", "team": team, "session_id": co, "generation": 1}
	workerHandle := map[string]any{"project": "alpha", "team": team, "session_id": worker, "generation": 1}
	with := func(base map[string]any, extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	taskID, rev := createTask("task-1")
	offerArgs := with(coHandle, map[string]any{"coordinator_generation": 1, "task_id": taskID, "expected_revision": rev, "worker": map[string]any{"session_id": worker, "generation": 1}, "suitability_rationale": "S task; fit confirmed", "cost_rationale": "least costly suitable member", "idempotency_key": "offer"})
	// Admin and read-only credentials are refused by the hub, not emulated.
	refuse(f.env, "team_offer", offerArgs)
	refuse(f.reader, "team_offer", offerArgs)
	offer := call(f.alice, "team_offer", offerArgs)["assignment"].(map[string]any)
	attempt := offer["id"].(string)
	if offer["state"] != "OFFERED" {
		t.Fatal(offer)
	}
	// The offer reached the worker's inbox as a lifecycle message before acceptance.
	inbox := call(f.alice, "team_inbox", workerHandle)["messages"].([]any)
	if len(inbox) != 1 || inbox[0].(map[string]any)["kind"] != "lifecycle" || inbox[0].(map[string]any)["lifecycle"].(map[string]any)["attempt_id"] != attempt {
		t.Fatalf("worker inbox: %v", inbox)
	}
	if got := call(f.alice, "team_accept", with(workerHandle, map[string]any{"attempt": attempt, "idempotency_key": "accept"}))["assignment"].(map[string]any); got["state"] != "RUNNING" {
		t.Fatal(got)
	}
	if got := call(f.alice, "team_reserved", workerHandle)["assignment"].(map[string]any); got["id"] != attempt {
		t.Fatal(got)
	}
	// The coordinator holds no reservation: the hub answers 404 and the tool reports it.
	refuse(f.alice, "team_reserved", coHandle)
	if got := call(f.alice, "team_block", with(workerHandle, map[string]any{"attempt": attempt, "expected_revision": 2, "reason": "waiting", "idempotency_key": "block"}))["assignment"].(map[string]any); got["state"] != "BLOCKED" {
		t.Fatal(got)
	}
	if got := call(f.alice, "team_resume_work", with(workerHandle, map[string]any{"attempt": attempt, "expected_revision": 3, "reason": "answered", "idempotency_key": "resume-work"}))["assignment"].(map[string]any); got["state"] != "RUNNING" {
		t.Fatal(got)
	}
	submitted := call(f.alice, "team_submit", with(workerHandle, map[string]any{"attempt": attempt, "expected_revision": 4, "base_commit": strings.Repeat("a", 40), "commit": strings.Repeat("b", 40), "summary": "done", "validation": "go test ./... passed", "evidence_refs": []map[string]any{{"kind": "text", "ref": "disposable run"}}, "idempotency_key": "submit"}))["assignment"].(map[string]any)
	if submitted["state"] != "SUBMITTED" || readTask(taskID)["state"] != "REVIEW" {
		t.Fatal(submitted)
	}
	resultID := submitted["result"].(map[string]any)["id"].(string)
	// A worker handle cannot review; the coordinator accepts.
	refuse(f.alice, "team_review", with(workerHandle, map[string]any{"attempt": attempt, "coordinator_generation": 1, "expected_revision": 5, "result_id": resultID, "decision": "accept", "reason": "assessed", "idempotency_key": "w-review"}))
	if got := call(f.alice, "team_review", with(coHandle, map[string]any{"attempt": attempt, "coordinator_generation": 1, "expected_revision": 5, "result_id": resultID, "decision": "accept", "reason": "assessed", "idempotency_key": "review"}))["assignment"].(map[string]any); got["state"] != "ACCEPTED" {
		t.Fatal(got)
	}
	// Handoff to an idle designated session; the old handle is stale, the successor finalizes.
	_, successor := join("worker", "successor")
	handoff := call(f.alice, "team_handoff", with(coHandle, map[string]any{"coordinator_generation": 1, "target": map[string]any{"session_id": successor, "generation": 1}, "reason": "shift ends", "idempotency_key": "handoff"}))["session"].(map[string]any)
	if handoff["id"] != successor || handoff["role"] != "coordinator" || handoff["coordinator_generation"].(float64) != 2 {
		t.Fatal(handoff)
	}
	newCo := map[string]any{"project": "alpha", "team": team, "session_id": successor, "generation": 1}
	finalize := map[string]any{"task": taskID, "coordinator_generation": 2, "expected_revision": 6, "attempt_id": attempt, "reason": "merged by the owner", "evidence": []map[string]any{{"kind": "text", "ref": "merge evidence"}}, "idempotency_key": "finalize"}
	refuse(f.alice, "team_finalize", with(coHandle, finalize))
	if got := call(f.alice, "team_finalize", with(newCo, finalize))["task"].(map[string]any); got["state"] != "DONE" {
		t.Fatal(got)
	}
	// Edit between attempts on a second task: offer, withdraw, then edit.
	task2, rev2 := createTask("task-2")
	offer2 := call(f.alice, "team_offer", with(newCo, map[string]any{"coordinator_generation": 2, "task_id": task2, "expected_revision": rev2, "worker": map[string]any{"session_id": worker, "generation": 1}, "suitability_rationale": "S task; fit confirmed", "cost_rationale": "least costly suitable member", "idempotency_key": "offer-2"}))["assignment"].(map[string]any)
	call(f.alice, "team_withdraw", with(newCo, map[string]any{"attempt": offer2["id"], "coordinator_generation": 2, "reason": "reassess", "idempotency_key": "withdraw-2"}))
	content := readTask(task2)
	for _, k := range []string{"id", "revision", "coordination", "created_at", "updated_at", "project", "links"} {
		delete(content, k)
	}
	content["title"] = "re-scoped change"
	edited := call(f.alice, "team_edit", with(newCo, map[string]any{"task": task2, "coordinator_generation": 2, "expected_revision": rev2, "content": content, "reason": "scope refined", "idempotency_key": "edit"}))["task"].(map[string]any)
	if edited["title"] != "re-scoped change" || edited["state"] != "READY" || edited["coordination"] == nil {
		t.Fatal(edited)
	}
	// The tool list exposes exactly the bridged operations to the ordinary token.
	names := toolNames(f.rpc(t, f.alice, "tools/list", nil))
	for _, tool := range teamWorkTools {
		found := false
		for _, n := range names {
			if n == tool.name {
				found = true
			}
		}
		if !found {
			t.Fatal("missing tool", tool.name)
		}
	}
	for _, n := range names {
		if n == "team_recover" || n == "team_unmanage" {
			t.Fatal("admin operation exposed over MCP")
		}
	}
}
