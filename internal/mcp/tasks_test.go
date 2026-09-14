package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/adapter"
	"aimem/internal/server"
	"aimem/internal/store"
)

// hub is a real hub-mode surface (route table + /mcp behind the bearer
// gate) with an ordinary user token on project alpha, a read-only
// ordinary token, and the env admin token.
type hubFixture struct {
	h                  http.Handler
	env, alice, reader string
}

func newHub(t *testing.T) *hubFixture {
	t.Helper()
	reg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	for _, p := range []string{"alpha", "beta"} {
		if _, err := reg.Open(p); err != nil {
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
	alice, err := acc.CreateUser("admin", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := reg.ProjectAccessID("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := acc.SetGrant("admin", instance, "user", alice.ID, true); err != nil {
		t.Fatal(err)
	}
	_, aliceSecret, err := acc.Issue("admin", alice.ID, "agent", instance, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, readerSecret, err := acc.Issue("admin", alice.ID, "reader", "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// The legacy tools' local client points nowhere: nothing in these
	// tests may reach it.
	dead := &http.Client{Transport: http.NewFileTransport(http.Dir(t.TempDir()))}
	mcpHandler := NewHTTPHandler(dead, func(r *http.Request) (TaskCallFunc, bool) {
		call, only := srv.MCPPrincipal(r)
		if call == nil {
			return nil, only
		}
		return call, only
	})
	return &hubFixture{h: srv.TCPHandler("env-secret", map[string]http.Handler{"/mcp": mcpHandler}), env: "env-secret", alice: aliceSecret, reader: readerSecret}
}

func (f *hubFixture) rpc(t *testing.T, token, method string, params any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(raw)))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("%s: HTTP %d %s", method, w.Code, w.Body)
	}
	var resp struct {
		Result map[string]any `json:"result"`
		Error  *rpcError      `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v %s", err, w.Body)
	}
	if resp.Error != nil {
		t.Fatalf("%s: rpc error %+v", method, resp.Error)
	}
	return resp.Result
}

// toolText returns the tool's text content and whether it was an error.
func toolText(res map[string]any) (string, bool) {
	content, _ := res["content"].([]any)
	if len(content) == 0 {
		return "", false
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	isErr, _ := res["isError"].(bool)
	return text, isErr
}

func toolNames(res map[string]any) []string {
	var names []string
	for _, tl := range res["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	return names
}

func TestRemoteMCPTaskToolsUseTheCallersAuthority(t *testing.T) {
	f := newHub(t)
	// An ordinary token sees task tools only, and hidden tools stay hidden
	// when called by name.
	names := toolNames(f.rpc(t, f.alice, "tools/list", nil))
	if len(names) != len(taskToolDefs) {
		t.Fatalf("ordinary token tool list: %v", names)
	}
	for _, n := range names {
		if !isTaskTool(n) {
			t.Fatalf("legacy tool %q exposed to an ordinary token", n)
		}
	}
	text, isErr := toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "recall_memory", "arguments": map[string]any{"query": "x", "project": "alpha"}}))
	if !isErr || !strings.Contains(text, "task tools only") {
		t.Fatalf("hidden tool by name: %q %v", text, isErr)
	}
	// The admin sees everything.
	if n := len(toolNames(f.rpc(t, f.env, "tools/list", nil))); n != len(toolDefs)+len(taskToolDefs) {
		t.Fatalf("admin tool list: %d", n)
	}

	// Lifecycle through MCP: create, read, update with CAS, comment, list.
	text, isErr = toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "create_task", "arguments": map[string]any{
		"project": "alpha", "title": "via mcp", "objective": "prove the boundary", "idempotency_key": "m1"}}))
	if isErr {
		t.Fatalf("create_task: %s", text)
	}
	var task struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
		Project  string `json:"project"`
		State    string `json:"state"`
	}
	if err := json.Unmarshal([]byte(text), &task); err != nil || task.Project != "alpha" || task.State != "BACKLOG" {
		t.Fatalf("create_task result: %v %s", err, text)
	}
	text, isErr = toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "create_task", "arguments": map[string]any{
		"project": "alpha", "title": "via mcp", "objective": "prove the boundary", "idempotency_key": "m1"}}))
	if isErr || !strings.Contains(text, task.ID) {
		t.Fatalf("create_task replay: %v %s", isErr, text)
	}
	text, isErr = toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "create_task", "arguments": map[string]any{
		"project": "beta", "title": "foreign", "idempotency_key": "m2"}}))
	if !isErr || !strings.Contains(text, "not issued for this project") {
		t.Fatalf("alice on beta through MCP: %v %s", isErr, text)
	}
	text, isErr = toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "create_task", "arguments": map[string]any{
		"project": "alpha", "title": "no key"}}))
	if !isErr || !strings.Contains(text, "idempotency_key") {
		t.Fatalf("missing key: %v %s", isErr, text)
	}
	text, isErr = toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "update_task", "arguments": map[string]any{
		"id": task.ID, "title": "via mcp", "state": "IN_PROGRESS", "expected_revision": 1, "idempotency_key": "m3"}}))
	if isErr || !strings.Contains(text, `"revision": 2`) {
		t.Fatalf("update_task: %v %s", isErr, text)
	}
	text, isErr = toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "update_task", "arguments": map[string]any{
		"id": task.ID, "title": "stale", "state": "IN_PROGRESS", "expected_revision": 1, "idempotency_key": "m4"}}))
	if !isErr || !strings.Contains(text, "current:") || !strings.Contains(text, `"revision":2`) {
		t.Fatalf("stale update must hand back the current task: %v %s", isErr, text)
	}
	text, isErr = toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "add_task_comment", "arguments": map[string]any{
		"id": task.ID, "body": "looks **good**", "idempotency_key": "m5"}}))
	if isErr {
		t.Fatalf("add_task_comment: %s", text)
	}
	var comment struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(text), &comment)
	text, isErr = toolText(f.rpc(t, f.reader, "tools/call", map[string]any{"name": "get_task_comment", "arguments": map[string]any{
		"task_id": task.ID, "comment_id": comment.ID}}))
	if isErr || !strings.Contains(text, "looks **good**") {
		t.Fatalf("get_task_comment as reader: %v %s", isErr, text)
	}
	text, isErr = toolText(f.rpc(t, f.reader, "tools/call", map[string]any{"name": "add_task_comment", "arguments": map[string]any{
		"id": task.ID, "body": "nope", "idempotency_key": "m6"}}))
	if !isErr {
		t.Fatalf("read-only token commented: %s", text)
	}
	text, isErr = toolText(f.rpc(t, f.reader, "tools/call", map[string]any{"name": "list_tasks", "arguments": map[string]any{"project": "alpha", "state": "IN_PROGRESS"}}))
	if isErr || !strings.Contains(text, task.ID) {
		t.Fatalf("list_tasks: %v %s", isErr, text)
	}
	text, isErr = toolText(f.rpc(t, f.reader, "tools/call", map[string]any{"name": "get_task_history", "arguments": map[string]any{"id": task.ID, "limit": 1}}))
	if isErr || !strings.Contains(text, `"next_cursor": 1`) {
		t.Fatalf("get_task_history paging: %v %s", isErr, text)
	}
	// Legacy tools still work for the admin path... except that their local
	// client points nowhere here, which is exactly the point: task tools
	// never touched it.
	text, isErr = toolText(f.rpc(t, f.env, "tools/call", map[string]any{"name": "get_task", "arguments": map[string]any{"id": task.ID}}))
	if isErr || !strings.Contains(text, `"via mcp"`) {
		t.Fatalf("admin get_task: %v %s", isErr, text)
	}
}

// The stdio facade reaches the hub over HTTP with the hub's task
// credential; a missing credential is an actionable error, never a
// fallback.
func TestLocalTaskCallerUsesHubTaskToken(t *testing.T) {
	f := newHub(t)
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	s := &srv{project: "alpha", taskSetup: func() (TaskCallFunc, error) {
		return hubCaller(ts.URL, f.alice, ts.Client()), nil
	}}
	resp := s.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_task","arguments":{"title":"from stdio","idempotency_key":"s1"}}}`))
	if !strings.Contains(string(resp), `\"project\": \"alpha\"`) || strings.Contains(string(resp), "isError") {
		t.Fatalf("stdio create: %s", resp)
	}
	// Wrong credential: the hub refuses, the model sees why.
	s2 := &srv{project: "alpha", taskSetup: func() (TaskCallFunc, error) {
		return hubCaller(ts.URL, "aimem_user_wrong", ts.Client()), nil
	}}
	resp = s2.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}`))
	if !strings.Contains(string(resp), "isError") || !strings.Contains(string(resp), "HTTP 401") {
		t.Fatalf("wrong credential: %s", resp)
	}
	// No credential configured: actionable, and the setup error is not cached as success.
	s3 := &srv{project: "alpha", taskSetup: func() (TaskCallFunc, error) {
		return nil, fmt.Errorf("hub %q has no task credential: run `aimem hub task-token %s <token>`", "home", "home")
	}}
	resp = s3.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_task","arguments":{"id":"x"}}}`))
	if !strings.Contains(string(resp), "aimem hub task-token") {
		t.Fatalf("missing credential: %s", resp)
	}
	// A facade with no task route at all (unit default) says so.
	s4 := &srv{project: "alpha"}
	resp = s4.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_task","arguments":{"id":"x"}}}`))
	if !strings.Contains(string(resp), "not available") {
		t.Fatalf("no caller: %s", resp)
	}
}

func TestTaskToolDefsAreValidSchema(t *testing.T) {
	for _, d := range taskToolDefs {
		schema, ok := d["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Fatalf("%v: inputSchema must be an object schema", d["name"])
		}
		props := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]string)
		for _, r := range required {
			if _, ok := props[r]; !ok {
				t.Fatalf("%v: required %q not in properties", d["name"], r)
			}
		}
	}
}

func TestHubTaskTokenRoundTrips(t *testing.T) {
	root := t.TempDir()
	hubs := map[string]*adapter.HubConfig{"home": {URL: "https://hub.example", Token: "checkpoint", TaskToken: "aimem_user_abc"}}
	if err := adapter.SaveHubs(root, hubs, "home"); err != nil {
		t.Fatal(err)
	}
	got, def := adapter.LoadHubs(root)
	if def != "home" || got["home"].TaskToken != "aimem_user_abc" || got["home"].Token != "checkpoint" {
		t.Fatalf("task token lost: %+v", got["home"])
	}
}
