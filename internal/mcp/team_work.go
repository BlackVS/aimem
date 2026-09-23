package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// teamWorkTool describes one assignment, execution or management operation
// as a thin bridge to its HTTP route. The schema mirrors the HTTP body so an
// agent sees the same fields everywhere; the hub decides every authority,
// generation, ownership and revision question, so MCP and CLI refuse exactly
// as HTTP does. Admin recover and unmanage are deliberately absent: the
// checkout credential is never an operator credential.
type teamWorkTool struct {
	name     string
	method   string
	path     string // suffix under the team prefix; {attempt} and {task} come from arguments
	summary  string
	props    map[string]any
	required []string
}

func handleProps() map[string]any {
	return map[string]any{"session_id": prop("string", "your session ID"), "generation": prop("integer", "your current session generation")}
}

func withProps(base map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

var teamWorkTools = func() []teamWorkTool {
	handle := handleProps()
	reason := prop("string", "nonempty reason, at most 4096 UTF-8 bytes")
	revision := prop("integer", "task revision you last read; a conflict returns the current task")
	coordinator := prop("integer", "current coordinator generation from your join or resume")
	refs := map[string]any{"type": "array", "items": taskRefProp(), "minItems": 1}
	worker := objSchema(handleProps(), "session_id", "generation")
	tools := []teamWorkTool{
		{"team_offer", "POST", "/assignments", "Coordinator offers a READY task to an available worker session; reserves task and worker until accept, decline or withdraw.",
			withProps(handle, map[string]any{"coordinator_generation": coordinator, "task_id": prop("string", "READY task in this project"), "expected_revision": revision, "worker": worker,
				"suitability_rationale": prop("string", "complexity, required capabilities and model fit; clarify unknown fit before offering"), "cost_rationale": prop("string", "why this member is the least costly suitable one")}),
			[]string{"session_id", "generation", "coordinator_generation", "task_id", "expected_revision", "worker", "suitability_rationale", "cost_rationale"}},
		{"team_assignment", "GET", "/assignments/{attempt}", "Read one assignment as a current team member.", withProps(handle, map[string]any{"attempt": prop("string", "attempt ID")}), []string{"session_id", "generation", "attempt"}},
		{"team_reserved", "GET", "/assignments/reserved", "Read the attempt reserved for your session, if any: the first read after a resume, before reconciling local work.", handle, []string{"session_id", "generation"}},
		{"team_accept", "POST", "/assignments/{attempt}/accept", "Intended worker accepts an OFFERED attempt; task IN_PROGRESS, attempt RUNNING.", withProps(handle, map[string]any{"attempt": prop("string", "attempt ID")}), []string{"session_id", "generation", "attempt"}},
		{"team_decline", "POST", "/assignments/{attempt}/decline", "Intended worker declines an OFFERED attempt with a reason.", withProps(handle, map[string]any{"attempt": prop("string", "attempt ID"), "reason": reason}), []string{"session_id", "generation", "attempt", "reason"}},
		{"team_withdraw", "POST", "/assignments/{attempt}/withdraw", "Current coordinator withdraws an OFFERED attempt with a reason.", withProps(handle, map[string]any{"attempt": prop("string", "attempt ID"), "coordinator_generation": coordinator, "reason": reason}), []string{"session_id", "generation", "attempt", "coordinator_generation", "reason"}},
		{"team_block", "POST", "/assignments/{attempt}/block", "Assigned worker marks RUNNING work BLOCKED; the reason becomes the task blocker.", withProps(handle, map[string]any{"attempt": prop("string", "attempt ID"), "expected_revision": revision, "reason": reason}), []string{"session_id", "generation", "attempt", "expected_revision", "reason"}},
		{"team_resume_work", "POST", "/assignments/{attempt}/resume-work", "Assigned worker returns BLOCKED work to RUNNING.", withProps(handle, map[string]any{"attempt": prop("string", "attempt ID"), "expected_revision": revision, "reason": reason}), []string{"session_id", "generation", "attempt", "expected_revision", "reason"}},
		{"team_cancel", "POST", "/assignments/{attempt}/cancel", "Current coordinator requests a stop of RUNNING or BLOCKED work; a durable request, not proof a process stopped.", withProps(handle, map[string]any{"attempt": prop("string", "attempt ID"), "coordinator_generation": coordinator, "expected_revision": revision, "reason": reason}), []string{"session_id", "generation", "attempt", "coordinator_generation", "expected_revision", "reason"}},
		{"team_stopped", "POST", "/assignments/{attempt}/stopped", "Assigned worker acknowledges a stop request after reconciling local execution; the reservation stays until the coordinator closes it.", withProps(handle, map[string]any{"attempt": prop("string", "attempt ID"), "expected_revision": revision, "reason": reason}), []string{"session_id", "generation", "attempt", "expected_revision", "reason"}},
		{"team_close_stop", "POST", "/assignments/{attempt}/close-stop", "Current coordinator closes STOPPED work as CANCELLED and requeues the task READY.", withProps(handle, map[string]any{"attempt": prop("string", "attempt ID"), "coordinator_generation": coordinator, "expected_revision": revision, "reason": reason}), []string{"session_id", "generation", "attempt", "coordinator_generation", "expected_revision", "reason"}},
		{"team_submit", "POST", "/assignments/{attempt}/submit", "Assigned worker submits one immutable result for RUNNING work; task REVIEW. Corrections need a new offer.",
			withProps(handle, map[string]any{"attempt": prop("string", "attempt ID"), "expected_revision": revision, "base_commit": prop("string", "full Git object ID the change is based on"), "commit": prop("string", "full Git object ID of the candidate"),
				"summary": prop("string", "what changed"), "validation": prop("string", "what you ran and what it showed"), "evidence_refs": refs}),
			[]string{"session_id", "generation", "attempt", "expected_revision", "base_commit", "commit", "summary", "validation", "evidence_refs"}},
		{"team_review", "POST", "/assignments/{attempt}/review", "Current coordinator records accept (task stays REVIEW for delivery gates) or rework (task READY) for a SUBMITTED result. Never marks DONE.",
			withProps(handle, map[string]any{"attempt": prop("string", "attempt ID"), "coordinator_generation": coordinator, "expected_revision": revision, "result_id": prop("string", "submitted result ID"), "decision": propEnum("disposition", "accept", "rework"), "reason": reason}),
			[]string{"session_id", "generation", "attempt", "coordinator_generation", "expected_revision", "result_id", "decision", "reason"}},
		{"team_handoff", "POST", "/handoff", "Current coordinator transfers the slot to an active designated session with no reserved attempt; your handle becomes stale.",
			withProps(handle, map[string]any{"coordinator_generation": coordinator, "target": worker, "reason": reason}), []string{"session_id", "generation", "coordinator_generation", "target", "reason"}},
		{"team_edit", "POST", "/tasks/{task}/edit", "Current coordinator replaces a managed task's content between attempts; state must stay the current one.",
			withProps(handle, map[string]any{"task": prop("string", "managed task ID"), "coordinator_generation": coordinator, "expected_revision": revision, "content": map[string]any{"type": "object", "description": "full task content as returned by get_task, with the current state and archived false"}, "reason": reason}),
			[]string{"session_id", "generation", "task", "coordinator_generation", "expected_revision", "content", "reason"}},
		{"team_finalize", "POST", "/tasks/{task}/finalize", "Current coordinator records DONE for a REVIEW task whose named attempt was accepted, with merge/delivery evidence.",
			withProps(handle, map[string]any{"task": prop("string", "managed task ID"), "coordinator_generation": coordinator, "expected_revision": revision, "attempt_id": prop("string", "ACCEPTED attempt ID"), "reason": reason, "evidence": refs}),
			[]string{"session_id", "generation", "task", "coordinator_generation", "expected_revision", "attempt_id", "reason", "evidence"}},
	}
	for i := range tools {
		tools[i].props = withProps(tools[i].props, map[string]any{"project": prop("string", "project, default current checkout"), "team": prop("string", "team ID from your join")})
		tools[i].required = append([]string{"team"}, tools[i].required...)
		if tools[i].method == "POST" {
			tools[i].props["idempotency_key"] = prop("string", "unique retry key; reuse only for the identical request")
			tools[i].required = append(tools[i].required, "idempotency_key")
		}
	}
	return tools
}()

const teamWorkNote = " Uses your project task credential; the hub checks authority, generations, ownership and revision exactly as over HTTP. Each transition also delivers a lifecycle message to the counterpart's inbox; read the inbox for offers, cancellations, reviews and recoveries and ack what you have read. A retry returns its original result. Nothing here stops a local process."

var teamWorkToolDefs = func() []map[string]any {
	defs := []map[string]any{}
	for _, tool := range teamWorkTools {
		defs = append(defs, map[string]any{"name": tool.name, "description": tool.summary + teamWorkNote, "inputSchema": objSchema(tool.props, tool.required...)})
	}
	return defs
}()

func teamWorkToolByName(name string) (teamWorkTool, bool) {
	for _, tool := range teamWorkTools {
		if tool.name == name {
			return tool, true
		}
	}
	return teamWorkTool{}, false
}

// callTeamWorkTool validates the arguments against the tool schema, then
// forwards the operation fields unchanged as the HTTP body (or query for
// reads). The hub's strict decoder is the contract; nothing is reshaped.
func callTeamWorkTool(ctx context.Context, call TaskCallFunc, defaultProject string, tool teamWorkTool, raw json.RawMessage) (string, error) {
	method, path, headers, body, err := buildTeamWorkRequest(defaultProject, tool, raw)
	if err != nil {
		return "", err
	}
	return forwardTeamRequest(ctx, call, method, path, headers, body)
}

// buildTeamWorkRequest validates the arguments against the tool schema and
// renders the hub request: the operation fields go unchanged as the HTTP
// body (or query for reads).
func buildTeamWorkRequest(defaultProject string, tool teamWorkTool, raw json.RawMessage) (method, path string, headers map[string]string, body []byte, err error) {
	fail := func(e error) (string, string, map[string]string, []byte, error) { return "", "", nil, nil, e }
	var fields map[string]json.RawMessage
	if len(raw) > 64<<10 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return fail(errors.New("team arguments must be a JSON object at most 64 KiB"))
	}
	for k := range fields {
		if _, ok := tool.props[k]; !ok {
			return fail(fmt.Errorf("unknown team argument %q", k))
		}
	}
	for _, k := range tool.required {
		if len(fields[k]) == 0 || string(fields[k]) == "null" {
			return fail(fmt.Errorf("%s required", k))
		}
	}
	text := func(k string) (string, error) {
		var s string
		if len(fields[k]) == 0 {
			return "", nil
		}
		if err := json.Unmarshal(fields[k], &s); err != nil {
			return "", fmt.Errorf("%s must be a string", k)
		}
		return s, nil
	}
	project, err := text("project")
	if err != nil {
		return fail(err)
	}
	if project == "" {
		project = defaultProject
	}
	team, err := text("team")
	if err != nil {
		return fail(err)
	}
	if project == "" || team == "" {
		return fail(errors.New("project and team are required"))
	}
	path = "/v1/projects/" + url.PathEscape(project) + "/teams/" + url.PathEscape(team) + tool.path
	for _, seg := range []string{"attempt", "task"} {
		if !strings.Contains(tool.path, "{"+seg+"}") {
			continue
		}
		v, err := text(seg)
		if err != nil {
			return fail(err)
		}
		if v == "" {
			return fail(fmt.Errorf("%s required", seg))
		}
		path = strings.Replace(path, "{"+seg+"}", url.PathEscape(v), 1)
		delete(fields, seg)
	}
	key, err := text("idempotency_key")
	if err != nil {
		return fail(err)
	}
	delete(fields, "project")
	delete(fields, "team")
	delete(fields, "idempotency_key")
	headers = map[string]string{}
	if tool.method == "GET" {
		session, err := text("session_id")
		if err != nil {
			return fail(err)
		}
		var generation int64
		if err := json.Unmarshal(fields["generation"], &generation); err != nil {
			return fail(errors.New("generation must be an integer"))
		}
		path += "?" + url.Values{"session_id": {session}, "generation": {strconv.FormatInt(generation, 10)}}.Encode()
	} else {
		if key == "" {
			return fail(errors.New("idempotency_key required"))
		}
		headers["Idempotency-Key"] = key
		body, err = json.Marshal(fields)
		if err != nil {
			return fail(err)
		}
	}
	return tool.method, path, headers, body, nil
}

// forwardTeamRequest sends one team request and returns the hub's pretty
// JSON, refusing anything that is not protocol version 1.
func forwardTeamRequest(ctx context.Context, call TaskCallFunc, method, path string, headers map[string]string, body []byte) (string, error) {
	status, resp, err := call(ctx, method, path, headers, body)
	if err != nil {
		return "", err
	}
	var result struct {
		Version int    `json:"protocol_version"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(resp, &result) != nil {
		return "", errors.New("hub team protocol unavailable or malformed; upgrade hub, do not emulate with task writes")
	}
	if status/100 != 2 {
		return "", fmt.Errorf("team request HTTP %d: %s", status, result.Error)
	}
	if result.Version != 1 {
		return "", errors.New("unsupported team protocol; upgrade compatible client/hub, do not emulate with task writes")
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, resp, "", "  "); err != nil {
		return "", err
	}
	return pretty.String(), nil
}
