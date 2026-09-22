package mcp

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/server"
	"aimem/internal/store"
)

// The rehearsal drives one coordinator and two workers, as three distinct
// principals, through the pilot scenarios of DESIGN-agent-teams.md
// ("Verification before enabling a pilot") on an isolated hub: agent
// operations go over the same MCP wire real clients use, operator actions
// over the admin HTTP routes, and the audit export is the evidence. No model,
// no client process: docs/TEAM-REHEARSAL.md says what this does and does not
// prove.

type rehearsalHub struct {
	t        *testing.T
	h        http.Handler
	admin    string
	reg      *store.Registry
	acc      *access.Store
	instance string
	users    map[string]string // name -> user id
	tokens   map[string]string // name -> secret
	tokenIDs map[string]string // name -> token id
	evidence []string
}

func newRehearsalHub(t *testing.T) *rehearsalHub {
	t.Helper()
	reg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	for _, p := range []string{"alpha", "beta"} {
		db, err := reg.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetMeta(store.TasksMetaKey, "on"); err != nil {
			t.Fatal(err)
		}
	}
	srv := server.New(reg, slog.New(slog.NewTextHandler(new(strings.Builder), nil)))
	t.Cleanup(func() { srv.Close() })
	acc, err := access.Open(reg.Root())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { acc.Close() })
	instance, err := reg.ProjectAccessID("alpha")
	if err != nil {
		t.Fatal(err)
	}
	r := &rehearsalHub{t: t, admin: "env-secret", reg: reg, acc: acc, instance: instance, users: map[string]string{}, tokens: map[string]string{}, tokenIDs: map[string]string{}}
	for _, name := range []string{"alice", "bob", "carol"} {
		u, err := acc.CreateUser("admin", name)
		if err != nil {
			t.Fatal(err)
		}
		if err := acc.SetGrant("admin", instance, "user", u.ID, true); err != nil {
			t.Fatal(err)
		}
		r.users[name] = u.ID
		r.issue(name, name+"-1")
	}
	dead := &http.Client{Transport: http.NewFileTransport(http.Dir(t.TempDir()))}
	mcpHandler := NewHTTPHandler(dead, func(req *http.Request) (TaskCallFunc, bool) {
		return srv.MCPPrincipal(req)
	})
	r.h = srv.TCPHandler(r.admin, map[string]http.Handler{"/mcp": mcpHandler})
	return r
}

// issue creates a project-scoped ordinary token for a user under a label.
func (r *rehearsalHub) issue(user, label string) string {
	r.t.Helper()
	tok, secret, err := r.acc.Issue("admin", r.users[user], label, r.instance, time.Now().Add(time.Hour))
	if err != nil {
		r.t.Fatal(err)
	}
	r.tokens[label] = secret
	r.tokenIDs[label] = tok.ID
	return secret
}

func (r *rehearsalHub) note(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	r.evidence = append(r.evidence, line)
	r.t.Log(line)
}

// http performs an admin or ordinary HTTP request against the hub.
func (r *rehearsalHub) http(method, path, token, key string, body any) (int, []byte) {
	r.t.Helper()
	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else if s, ok := body.(string); ok {
		reader = strings.NewReader(s)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			r.t.Fatal(err)
		}
		reader = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

func (r *rehearsalHub) mustHTTP(method, path, token, key string, body any, want int) map[string]any {
	r.t.Helper()
	code, raw := r.http(method, path, token, key, body)
	if code != want {
		r.t.Fatalf("%s %s: %d %s", method, path, code, raw)
	}
	out := map[string]any{}
	if len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, &out); err != nil {
			r.t.Fatal(err)
		}
	}
	return out
}

// tool calls an MCP tool with a bearer token, exactly as a client does, and
// fails the test when the hub refuses; refuse expects the refusal.
func (r *rehearsalHub) rpc(token, method string, params any) map[string]any {
	r.t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	if w.Code != 200 {
		return map[string]any{"isError": true, "content": []any{map[string]any{"text": fmt.Sprintf("HTTP %d: %s", w.Code, w.Body.String())}}}
	}
	var res struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		r.t.Fatal(err, w.Body.String())
	}
	return res.Result
}

func (r *rehearsalHub) tool(token, name string, args map[string]any) map[string]any {
	r.t.Helper()
	raw, bad := toolText(r.rpc(token, "tools/call", map[string]any{"name": name, "arguments": args}))
	if bad {
		r.t.Fatalf("%s: %s", name, raw)
	}
	if strings.Contains(raw, `"token_id"`) || strings.Contains(raw, `"user_id"`) {
		r.t.Fatalf("%s leaked bindings: %s", name, raw)
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		r.t.Fatal(name, err, raw)
	}
	return out
}

// refuse expects the hub to refuse the call for the named reason: an
// unrelated error is not a passed negative scenario, and a refusal may not
// carry a binding any more than a success may.
func (r *rehearsalHub) refuse(token, name string, args map[string]any, want string) {
	r.t.Helper()
	raw, bad := toolText(r.rpc(token, "tools/call", map[string]any{"name": name, "arguments": args}))
	if err := checkRefusal(raw, bad, want); err != nil {
		r.t.Fatalf("%s: %v", name, err)
	}
}

// checkRefusal is the rule behind refuse, kept pure so the rule itself is
// tested: refused, for the expected reason, without a user or token id.
func checkRefusal(raw string, bad bool, want string) error {
	switch {
	case !bad:
		return fmt.Errorf("accepted: %s", raw)
	case !strings.Contains(raw, want):
		return fmt.Errorf("refused for another reason than %q: %s", want, raw)
	case strings.Contains(raw, "token_id") || strings.Contains(raw, "user_id"):
		return fmt.Errorf("refusal leaked bindings: %s", raw)
	}
	return nil
}

const (
	refusedConflict = "HTTP 409: assignment state, task readiness or worker capacity conflict"
	refusedStale    = "HTTP 409: team session is closed or generation is stale"
	refusedScope    = "HTTP 403: token scope or current grant does not permit writes to this project"
)

func (r *rehearsalHub) createTask(key string) (string, int64) {
	r.t.Helper()
	out := r.mustHTTP("POST", "/v1/projects/alpha/tasks", r.tokens["alice-1"], key, `{"title":"bounded change `+key+`","state":"READY","objective":"prove the loop","acceptance_criteria":"runtime proof"}`, 201)
	return out["id"].(string), int64(out["revision"].(float64))
}

func (r *rehearsalHub) task(id string) map[string]any {
	r.t.Helper()
	return r.mustHTTP("GET", "/v1/tasks/"+id, r.tokens["alice-1"], "", nil, 200)
}

func (r *rehearsalHub) revision(id string) int64 {
	r.t.Helper()
	return int64(r.task(id)["revision"].(float64))
}

func assignmentOf(out map[string]any) map[string]any {
	a, _ := out["assignment"].(map[string]any)
	return a
}

func merge(base map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func TestTeamRehearsal(t *testing.T) {
	r := newRehearsalHub(t)
	alice, bob, carol := r.tokens["alice-1"], r.tokens["bob-1"], r.tokens["carol-1"]
	setup := fmt.Sprintf(`{"name":"Builders","enrollment":[{"user_id":%q,"coordinator":true},{"user_id":%q},{"user_id":%q,"coordinator":true}]}`, r.users["alice"], r.users["bob"], r.users["carol"])
	team := r.mustHTTP("POST", "/v1/projects/alpha/teams", r.admin, "setup", setup, 201)["id"].(string)
	base := "/v1/projects/alpha/teams/" + team

	// 1. Three principals join; the roster shows all three and no bindings.
	join := func(token, role, key string) map[string]any {
		out := r.tool(token, "team_join", map[string]any{"project": "alpha", "team": "Builders", "role": role, "profile": map[string]any{"label": key, "platform": "fixture", "platform_version": "1"}, "idempotency_key": key})
		s := out["session"].(map[string]any)
		return map[string]any{"project": "alpha", "team": team, "session_id": s["id"], "generation": 1}
	}
	co := join(alice, "coordinator", "join-alice")
	wb := join(bob, "worker", "join-bob")
	wc := join(carol, "worker", "join-carol")
	members := r.tool(alice, "team_members", co)["members"].([]any)
	if len(members) != 3 {
		t.Fatal("roster", members)
	}
	r.note("1 joined: coordinator and two workers as three principals; roster lists 3")

	// 2. Direct worker question and answer, acknowledged by both sides.
	asked := r.tool(bob, "team_send", merge(wb, map[string]any{"recipient": map[string]any{"kind": "member", "id": wc["session_id"]}, "kind": "question", "payload": map[string]any{"text": "Which test pins the allowlist?"}, "idempotency_key": "q1"}))["message"].(map[string]any)
	inbox := r.tool(carol, "team_inbox", wc)["messages"].([]any)
	if len(inbox) != 1 || inbox[0].(map[string]any)["id"] != asked["id"] {
		t.Fatal("carol inbox", inbox)
	}
	r.tool(carol, "team_ack", merge(wc, map[string]any{"message_ids": []string{asked["id"].(string)}, "idempotency_key": "ack-q1"}))
	answered := r.tool(carol, "team_send", merge(wc, map[string]any{"recipient": map[string]any{"kind": "member", "id": wb["session_id"]}, "kind": "answer", "reply_to": asked["id"], "payload": map[string]any{"text": "TestOrdinaryTokenGateMatrix, observed at the base commit", "refs": []map[string]any{{"kind": "text", "ref": "internal/server/tasks_test.go at the base commit"}}}, "idempotency_key": "a1"}))["message"].(map[string]any)
	if got := r.tool(bob, "team_inbox", wb)["messages"].([]any); len(got) != 1 || got[0].(map[string]any)["id"] != answered["id"] {
		t.Fatal("bob inbox", got)
	}
	r.tool(bob, "team_ack", merge(wb, map[string]any{"message_ids": []string{answered["id"].(string)}, "idempotency_key": "ack-a1"}))
	r.note("2 question/answer: bob asked carol, carol answered with a source, both acknowledged")

	// 3. Assignment collision: one reserved attempt per task and per worker.
	taskA, revA := r.createTask("task-a")
	offer := func(token string, handle map[string]any, gen int64, task string, rev int64, worker map[string]any, key string) map[string]any {
		return merge(handle, map[string]any{"coordinator_generation": gen, "task_id": task, "expected_revision": rev, "worker": map[string]any{"session_id": worker["session_id"], "generation": worker["generation"]}, "suitability_rationale": "S task; declared capability fits", "cost_rationale": "least costly suitable available member", "idempotency_key": key})
	}
	attemptA := assignmentOf(r.tool(alice, "team_offer", offer(alice, co, 1, taskA, revA, wb, "offer-a")))["id"].(string)
	r.refuse(alice, "team_offer", offer(alice, co, 1, taskA, r.revision(taskA), wc, "offer-a-collision"), refusedConflict)
	taskB, revB := r.createTask("task-b")
	r.refuse(alice, "team_offer", offer(alice, co, 1, taskB, revB, wb, "offer-b-busy"), refusedConflict)
	r.note("3 collision: second offer for a reserved task refused; offer to a worker holding an attempt refused")

	// 4. The complete loop on task A.
	if got := assignmentOf(r.tool(bob, "team_accept", merge(wb, map[string]any{"attempt": attemptA, "idempotency_key": "accept-a"}))); got["state"] != "RUNNING" {
		t.Fatal(got)
	}
	r.tool(bob, "team_send", merge(wb, map[string]any{"recipient": map[string]any{"kind": "team"}, "kind": "progress", "task_id": taskA, "payload": map[string]any{"text": "storage done, tests next"}, "idempotency_key": "progress-a"}))
	submitA := merge(wb, map[string]any{"attempt": attemptA, "expected_revision": r.revision(taskA), "base_commit": strings.Repeat("a", 40), "commit": strings.Repeat("b", 40), "summary": "task A done", "validation": "go test ./... passed", "evidence_refs": []map[string]any{{"kind": "text", "ref": "disposable run"}}, "idempotency_key": "submit-a"})
	submittedA := assignmentOf(r.tool(bob, "team_submit", submitA))
	resultA := submittedA["result"].(map[string]any)["id"].(string)
	if got := assignmentOf(r.tool(alice, "team_review", merge(co, map[string]any{"attempt": attemptA, "coordinator_generation": 1, "expected_revision": r.revision(taskA), "result_id": resultA, "decision": "accept", "reason": "evidence checked at the candidate head", "idempotency_key": "review-a"}))); got["state"] != "ACCEPTED" {
		t.Fatal(got)
	}
	if got := r.tool(alice, "team_finalize", merge(co, map[string]any{"task": taskA, "coordinator_generation": 1, "expected_revision": r.revision(taskA), "attempt_id": attemptA, "reason": "merged by the owner", "evidence": []map[string]any{{"kind": "text", "ref": "merge evidence"}}, "idempotency_key": "finalize-a"}))["task"].(map[string]any); got["state"] != "DONE" {
		t.Fatal(got)
	}
	r.note("4 loop: offer, accept, progress, submit, review accept, finalize DONE on task A")

	// 5. Worker restart during execution: resume advances the generation, the
	// reserved attempt follows, and the old handle is refused.
	attemptB := assignmentOf(r.tool(alice, "team_offer", offer(alice, co, 1, taskB, r.revision(taskB), wb, "offer-b")))["id"].(string)
	r.tool(bob, "team_accept", merge(wb, map[string]any{"attempt": attemptB, "idempotency_key": "accept-b"}))
	resumed := r.tool(bob, "team_resume", merge(wb, map[string]any{"idempotency_key": "resume-bob"}))["session"].(map[string]any)
	if resumed["generation"].(float64) != 2 {
		t.Fatal(resumed)
	}
	wbOld := wb
	wb = merge(wb, map[string]any{"generation": 2})
	if got := assignmentOf(r.tool(bob, "team_reserved", wb)); got["id"] != attemptB || got["worker"].(map[string]any)["generation"].(float64) != 2 {
		t.Fatal(got)
	}
	r.refuse(bob, "team_block", merge(wbOld, map[string]any{"attempt": attemptB, "expected_revision": r.revision(taskB), "reason": "stale", "idempotency_key": "stale-block"}), refusedStale)
	r.note("5 restart: resume moved bob to generation 2, the reserved attempt followed, the old handle was refused")

	// 6. Duplicate commands and results: a replayed key returns the original
	// receipt; a re-issued submit under a new key is refused.
	submitB := merge(wb, map[string]any{"attempt": attemptB, "expected_revision": r.revision(taskB), "base_commit": strings.Repeat("a", 40), "commit": strings.Repeat("c", 40), "summary": "task B done", "validation": "go test ./... passed", "evidence_refs": []map[string]any{{"kind": "text", "ref": "disposable run"}}, "idempotency_key": "submit-b"})
	first := assignmentOf(r.tool(bob, "team_submit", submitB))
	replay := assignmentOf(r.tool(bob, "team_submit", submitB))
	if first["result"].(map[string]any)["id"] != replay["result"].(map[string]any)["id"] {
		t.Fatal("replay produced a second result")
	}
	r.refuse(bob, "team_submit", merge(submitB, map[string]any{"idempotency_key": "submit-b-again"}), refusedConflict)
	resultB := first["result"].(map[string]any)["id"].(string)
	r.note("6 duplicates: replayed submit returned the same result; a re-issued submit was refused")

	// 7. Coordinator loss: an admin hands the slot to carol with reconciliation;
	// alice's old handle is refused afterwards; carol reviews task B.
	handoff := map[string]any{"session_id": co["session_id"], "generation": 1, "coordinator_generation": 1, "target": map[string]any{"session_id": wc["session_id"], "generation": 1}, "reason": "coordinator host lost", "reconciliation": map[string]any{"coordinator_stopped": true, "liveness_check": "no heartbeat for two intervals; host unreachable", "evidence_refs": []map[string]any{{"kind": "text", "ref": "operator checked the host"}}}}
	if got := r.mustHTTP("POST", base+"/handoff", r.admin, "admin-handoff", handoff, 200)["session"].(map[string]any); got["role"] != "coordinator" || got["coordinator_generation"].(float64) != 2 {
		t.Fatal(got)
	}
	r.refuse(alice, "team_review", merge(co, map[string]any{"attempt": attemptB, "coordinator_generation": 1, "expected_revision": r.revision(taskB), "result_id": resultB, "decision": "accept", "reason": "stale coordinator", "idempotency_key": "stale-review"}), refusedStale)
	newCo := wc
	if got := assignmentOf(r.tool(carol, "team_review", merge(newCo, map[string]any{"attempt": attemptB, "coordinator_generation": 2, "expected_revision": r.revision(taskB), "result_id": resultB, "decision": "accept", "reason": "evidence checked by the successor", "idempotency_key": "review-b"}))); got["state"] != "ACCEPTED" {
		t.Fatal(got)
	}
	r.note("7 handoff: admin transferred the slot to carol with reconciliation; alice's old handle refused; carol reviewed task B")

	// 8. Token revocation mid-attempt, then operator token replacement.
	taskC, revC := r.createTask("task-c")
	attemptC := assignmentOf(r.tool(carol, "team_offer", offer(carol, newCo, 2, taskC, revC, wb, "offer-c")))["id"].(string)
	r.tool(bob, "team_accept", merge(wb, map[string]any{"attempt": attemptC, "idempotency_key": "accept-c"}))
	r.mustHTTP("DELETE", "/v1/access/tokens/"+r.tokenIDs["bob-1"], r.admin, "", nil, 200)
	r.refuse(bob, "team_reserved", wb, "HTTP 401")
	bob2 := r.issue("bob", "bob-2")
	rebind := map[string]any{"session_id": wb["session_id"], "expected_generation": 2, "token_id": r.tokenIDs["bob-2"], "reconciliation": map[string]any{"old_credential_stopped": true, "reason": "credential revoked after a leak report", "runtime_check": "old client stopped; no command in flight", "evidence_refs": []map[string]any{{"kind": "text", "ref": "operator checked the host"}}}}
	if got := r.mustHTTP("POST", base+"/sessions/"+wb["session_id"].(string)+"/rebind-token", r.admin, "rebind-bob", rebind, 200)["session"].(map[string]any); got["generation"].(float64) != 3 {
		t.Fatal(got)
	}
	wb = merge(wb, map[string]any{"generation": 3})
	if got := assignmentOf(r.tool(bob2, "team_reserved", wb)); got["id"] != attemptC {
		t.Fatal(got)
	}
	bob = bob2
	r.note("8 revocation: revoked token refused; operator rebound bob's session to a replacement token; work continued at generation 3")

	// 9. Cross-project denial: a token scoped to another project cannot join.
	beta, err := r.reg.ProjectAccessID("beta")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.acc.SetGrant("admin", beta, "user", r.users["carol"], true); err != nil {
		t.Fatal(err)
	}
	_, betaSecret, err := r.acc.Issue("admin", r.users["carol"], "carol-beta", beta, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	r.refuse(betaSecret, "team_join", map[string]any{"project": "alpha", "team": "Builders", "role": "worker", "profile": map[string]any{"label": "intruder", "platform": "fixture", "platform_version": "1"}, "idempotency_key": "join-beta"}, refusedScope)
	r.refuse(betaSecret, "team_members", newCo, refusedScope)
	r.note("9 cross-project: a token scoped to project beta cannot join or read the alpha team")

	// 10. Legacy mutation on a managed task is refused for every credential.
	for _, token := range []string{alice, r.admin} {
		if code, raw := r.http("PUT", "/v1/tasks/"+taskC, token, "legacy-"+token[:4], fmt.Sprintf(`{"title":"bypass","state":"DONE","archived":true,"expected_revision":%d}`, r.revision(taskC))); code != 409 || !strings.Contains(string(raw), "managed_task") {
			t.Fatal(code, string(raw))
		}
	}
	r.note("10 legacy mutation: generic update/archive of a managed task refused with managed_task for ordinary and admin credentials")

	// 11. Integration base changed after the worker finished: rework, new offer, resubmission.
	submitC := func(commitByte byte, key string) map[string]any {
		return assignmentOf(r.tool(bob, "team_submit", merge(wb, map[string]any{"attempt": attemptC, "expected_revision": r.revision(taskC), "base_commit": strings.Repeat("a", 40), "commit": strings.Repeat(string(commitByte), 40), "summary": "task C done", "validation": "go test ./... passed", "evidence_refs": []map[string]any{{"kind": "text", "ref": "disposable run"}}, "idempotency_key": key})))
	}
	firstC := submitC('d', "submit-c")
	if got := assignmentOf(r.tool(carol, "team_review", merge(newCo, map[string]any{"attempt": attemptC, "coordinator_generation": 2, "expected_revision": r.revision(taskC), "result_id": firstC["result"].(map[string]any)["id"], "decision": "rework", "reason": "integration base moved to a new master; rebase and resubmit", "idempotency_key": "rework-c"}))); got["state"] != "RETURNED" {
		t.Fatal(got)
	}
	attemptC2 := assignmentOf(r.tool(carol, "team_offer", offer(carol, newCo, 2, taskC, r.revision(taskC), wb, "offer-c2")))["id"].(string)
	r.tool(bob, "team_accept", merge(wb, map[string]any{"attempt": attemptC2, "idempotency_key": "accept-c2"}))
	attemptC = attemptC2
	secondC := assignmentOf(r.tool(bob, "team_submit", merge(wb, map[string]any{"attempt": attemptC2, "expected_revision": r.revision(taskC), "base_commit": strings.Repeat("e", 40), "commit": strings.Repeat("f", 40), "summary": "task C rebased on the new base", "validation": "go test ./... passed", "evidence_refs": []map[string]any{{"kind": "text", "ref": "disposable run"}}, "idempotency_key": "submit-c2"})))
	if got := assignmentOf(r.tool(carol, "team_review", merge(newCo, map[string]any{"attempt": attemptC2, "coordinator_generation": 2, "expected_revision": r.revision(taskC), "result_id": secondC["result"].(map[string]any)["id"], "decision": "accept", "reason": "rebased candidate checked", "idempotency_key": "review-c2"}))); got["state"] != "ACCEPTED" {
		t.Fatal(got)
	}
	r.note("11 base change: rework returned the first result; a new offer, resubmission on the new base and acceptance followed")

	// 12. Operator recovery of an abandoned running attempt, then unmanage.
	taskD, revD := r.createTask("task-d")
	attemptD := assignmentOf(r.tool(carol, "team_offer", offer(carol, newCo, 2, taskD, revD, wb, "offer-d")))["id"].(string)
	r.tool(bob, "team_accept", merge(wb, map[string]any{"attempt": attemptD, "idempotency_key": "accept-d"}))
	recovery := map[string]any{"expected_revision": r.revision(taskD), "expected_worker": map[string]any{"session_id": wb["session_id"], "generation": 3}, "expected_session_generation": 3, "expected_coordinator_generation": 2, "reconciliation": map[string]any{"execution_stopped": true, "reason": "worker host lost", "runtime_check": "process gone; no command in flight", "worktree_check": "worktree clean at the base commit", "evidence_refs": []map[string]any{{"kind": "text", "ref": "operator checked the host"}}}}
	if got := assignmentOf(r.mustHTTP("POST", base+"/assignments/"+attemptD+"/recover", r.admin, "recover-d", recovery, 200)); got["state"] != "RECOVERED" {
		t.Fatal(got)
	}
	if got := r.task(taskD); got["state"] != "READY" {
		t.Fatal(got)
	}
	r.mustHTTP("POST", base+"/tasks/"+taskD+"/unmanage", r.admin, "unmanage-d", map[string]any{"expected_revision": r.revision(taskD), "reason": "back to the ordinary backlog"}, 200)
	r.note("12 recovery: admin recovered the abandoned attempt with reconciliation (task READY) and released the task from management")

	// 13. Audit export at one snapshot: complete, and it names what happened.
	events, messages, ops, deliveries := r.export(base, t.TempDir())
	for _, op := range []string{"team.assignment.offer", "team.assignment.accept", "team.assignment.submit", "team.assignment.review", "team.coordinator.handoff", "team.session.rebind_token", "team.assignment.recover", "team.message.send", "team.message.ack"} {
		if !ops[op] {
			t.Fatalf("export lacks %s (have %v)", op, ops)
		}
	}
	if !deliveries["delivered"] || !deliveries["acknowledged"] {
		t.Fatal("export lacks delivery states", deliveries)
	}
	r.note("13 export: %d events and %d messages at one snapshot, complete; delivered and acknowledged records present", events, messages)
	if len(r.evidence) != 13 {
		t.Fatal("scenario count", len(r.evidence))
	}
}

// export follows the admin JSONL export to completion, writes it to dir and
// returns counts, the operations seen and the delivery states seen.
func (r *rehearsalHub) export(base, dir string) (int, int, map[string]bool, map[string]bool) {
	r.t.Helper()
	path := base + "/export?limit=20"
	ops, deliveries := map[string]bool{}, map[string]bool{}
	events, messages := 0, 0
	out, err := os.Create(filepath.Join(dir, "team-export.jsonl"))
	if err != nil {
		r.t.Fatal(err)
	}
	defer out.Close()
	for page := 0; ; page++ {
		if page > 100 {
			r.t.Fatal("export never completes")
		}
		code, raw := r.http("GET", path, r.admin, "", nil)
		if code != 200 {
			r.t.Fatal(code, string(raw))
		}
		out.Write(raw)
		var end map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				r.t.Fatal(err)
			}
			switch rec["record"] {
			case "event":
				events++
				ops[rec["operation"].(string)] = true
			case "message":
				messages++
				for _, d := range rec["deliveries"].([]any) {
					dl := d.(map[string]any)
					if dl["delivered_at"] != nil {
						deliveries["delivered"] = true
					}
					if dl["acked_at"] != nil {
						deliveries["acknowledged"] = true
					}
				}
			case "end":
				end = rec
			}
		}
		if end["complete"] == true {
			return events, messages, ops, deliveries
		}
		next := end["next"].(map[string]any)
		path = fmt.Sprintf("%s/export?limit=20&after_events=%d&after_messages=%d&snapshot_events=%d&snapshot_messages=%d", base, int64(next["after_events"].(float64)), int64(next["after_messages"].(float64)), int64(next["snapshot_events"].(float64)), int64(next["snapshot_messages"].(float64)))
	}
}

// The refusal rule itself: an unrelated error, an accepted call or a refusal
// that carries a binding must not count as a passed negative scenario.
func TestRehearsalRefusalRule(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		bad  bool
		want string
		ok   bool
	}{
		{"error: team request " + refusedConflict, true, refusedConflict, true},
		{"HTTP 401: ", true, "HTTP 401", true},
		{"HTTP 500: storage failure", true, refusedConflict, false},
		{"error: invalid arguments: attempt required", true, refusedConflict, false},
		{`{"protocol_version": 1, "assignment": {}}`, false, refusedConflict, false},
		{"error: team request " + refusedConflict + ` for user_id 0123`, true, refusedConflict, false},
	} {
		if err := checkRefusal(tc.raw, tc.bad, tc.want); (err == nil) != tc.ok {
			t.Fatalf("%q bad=%v want=%q: %v", tc.raw, tc.bad, tc.want, err)
		}
	}
}
