package mcp

import (
	"aimem/internal/ident"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	aliceID            string
}

func newHub(t *testing.T) *hubFixture {
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
		if err := db.SetMeta(store.TasksMetaKey, "on"); err != nil { // the admin's decision, made here
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
	return &hubFixture{h: srv.TCPHandler("env-secret", map[string]http.Handler{"/mcp": mcpHandler}), env: "env-secret", alice: aliceSecret, reader: readerSecret, aliceID: alice.ID}
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
	if isErr || !strings.Contains(text, `"next_cursor": 1`) || !strings.Contains(text, `"kind": "user"`) || !strings.Contains(text, `"user_id": "`+f.aliceID+`"`) {
		t.Fatalf("get_task_history must page and carry alice's stamp: %v %s", isErr, text)
	}
	// The list filter takes the documented kind/id string; a numeric field
	// sent as a string is a tool error, not a protocol error.
	text, isErr = toolText(f.rpc(t, f.reader, "tools/call", map[string]any{"name": "list_tasks", "arguments": map[string]any{"project": "alpha", "assignee": map[string]string{"kind": "user", "id": f.aliceID}}}))
	if isErr || !strings.Contains(text, `"tasks": []`) {
		t.Fatalf("assignee filter: %v %s", isErr, text)
	}
	text, isErr = toolText(f.rpc(t, f.reader, "tools/call", map[string]any{"name": "list_tasks", "arguments": map[string]any{"project": "alpha", "limit": "20"}}))
	if !isErr || !strings.Contains(text, "arguments") {
		t.Fatalf("bad argument type must be a tool error: %v %s", isErr, text)
	}
	// Strict arguments: a misspelled field is an error, never a silent clear.
	text, isErr = toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "update_task", "arguments": map[string]any{
		"id": task.ID, "title": "via mcp", "state": "IN_PROGRESS", "expected_revision": 2, "idempotency_key": "m7", "next_actions": "typo"}}))
	if !isErr || !strings.Contains(text, "next_actions") {
		t.Fatalf("unknown argument must be refused: %v %s", isErr, text)
	}
	// Comment paging through the tool, cursor echoed as number or string.
	text, isErr = toolText(f.rpc(t, f.reader, "tools/call", map[string]any{"name": "list_task_comments", "arguments": map[string]any{"id": task.ID, "limit": 1}}))
	if isErr || !strings.Contains(text, "looks **good**") || strings.Contains(text, "next_cursor") {
		t.Fatalf("list_task_comments: %v %s", isErr, text)
	}
	text, isErr = toolText(f.rpc(t, f.reader, "tools/call", map[string]any{"name": "get_task_history", "arguments": map[string]any{"id": task.ID, "after": "1"}}))
	if isErr || !strings.Contains(text, `"revision": 2`) || strings.Contains(text, `"revision": 1`) {
		t.Fatalf("history after cursor as string: %v %s", isErr, text)
	}
	// Legacy tools still work for the admin path... except that their local
	// client points nowhere here, which is exactly the point: task tools
	// never touched it.
	text, isErr = toolText(f.rpc(t, f.env, "tools/call", map[string]any{"name": "get_task", "arguments": map[string]any{"id": task.ID}}))
	if isErr || !strings.Contains(text, `"via mcp"`) {
		t.Fatalf("admin get_task: %v %s", isErr, text)
	}
}

// The stdio facade resolves the project's hub from hub.json and presents
// that hub's task credential — never its checkpoint token; a missing
// hub or credential is an actionable error, never a fallback.
func TestTaskCallerForUsesHubTaskToken(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tasks":[]}`))
	}))
	defer ts.Close()
	root := t.TempDir()
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{
		"home": {URL: ts.URL, Token: "checkpoint-secret", TaskToken: "aimem_user_alice"},
		"bare": {URL: ts.URL, Token: "checkpoint-secret"},
	}, "home"); err != nil {
		t.Fatal(err)
	}
	call, err := taskCallerFor(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if status, _, err := call(context.Background(), "GET", "/v1/projects/alpha/tasks", nil, nil); err != nil || status != 200 {
		t.Fatalf("call: %d %v", status, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "Bearer aimem_user_alice" {
		t.Fatalf("the hub must see the task credential, never the checkpoint token: %q", seen)
	}
	if _, err := taskCallerFor(root, "bare"); err == nil || !strings.Contains(err.Error(), "aimem hub task-token bare") {
		t.Fatalf("hub without task credential: %v", err)
	}
	if _, err := taskCallerFor(root, "work"); err == nil || !strings.Contains(err.Error(), `"work"`) {
		t.Fatalf("bound to an unconfigured hub: %v", err)
	}
	if _, err := taskCallerFor(t.TempDir(), ""); err == nil || !strings.Contains(err.Error(), "no hub configured") {
		t.Fatalf("no hub at all: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("refusals must not call the hub: %q", seen)
	}
}

// The stdio facade's tool listing follows per-project enablement as seen
// at session start: disabled hides the task tools and refuses them by
// name; enabled and unknown list them (the hub is the authority for every
// write). The probe returns unknown for anything short of a definite
// answer, and a session that started enabled gets one restart notice when
// the hub later says disabled.
func TestFacadeTaskStateListing(t *testing.T) {
	list := func(s *srv) string {
		return string(s.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)))
	}
	for _, st := range []string{taskStateEnabled, taskStateUnknown, ""} {
		if out := list(&srv{project: "alpha", taskState: st}); !strings.Contains(out, `"list_tasks"`) {
			t.Fatalf("state %q must list the task tools: %s", st, out)
		}
	}
	off := &srv{project: "alpha", taskState: taskStateDisabled}
	if out := list(off); strings.Contains(out, `"list_tasks"`) || !strings.Contains(out, `"recall_memory"`) {
		t.Fatalf("disabled must hide the task tools only: %s", out)
	}
	resp := off.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}`))
	if !strings.Contains(string(resp), "isError") || !strings.Contains(string(resp), "not enabled") {
		t.Fatalf("hidden tool called by name: %s", resp)
	}
	// The probe against a hub.
	var enabled atomic.Bool
	var say atomic.Int32 // 0: answer with tasks_enabled; 1: omit it (older hub); 2: 500
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/access/identity" || r.URL.Query().Get("project") != "alpha" {
			t.Errorf("unexpected probe: %s %s", r.Method, r.URL)
		}
		switch say.Load() {
		case 1:
			w.Write([]byte(`{"name":"x","role":"user"}`))
		case 2:
			w.WriteHeader(500)
		default:
			fmt.Fprintf(w, `{"name":"x","role":"user","tasks_enabled":%v}`, enabled.Load())
		}
	}))
	defer ts.Close()
	root := t.TempDir()
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"home": {URL: ts.URL, Token: "checkpoint", TaskToken: "aimem_user_alice"}}, "home"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if got := probeTaskState(dir, root, "alpha"); got != taskStateDisabled {
		t.Fatalf("disabled hub: %q", got)
	}
	enabled.Store(true)
	if got := probeTaskState(dir, root, "alpha"); got != taskStateEnabled {
		t.Fatalf("enabled hub: %q", got)
	}
	say.Store(1)
	if got := probeTaskState(dir, root, "alpha"); got != taskStateUnknown {
		t.Fatalf("older hub: %q", got)
	}
	say.Store(2)
	if got := probeTaskState(dir, root, "alpha"); got != taskStateUnknown {
		t.Fatalf("failing hub: %q", got)
	}
	if got := probeTaskState(dir, t.TempDir(), "alpha"); got != taskStateUnknown {
		t.Fatalf("no credential: %q", got)
	}
	if got := probeTaskState(dir, root, ""); got != taskStateUnknown {
		t.Fatalf("no project: %q", got)
	}
	// Started enabled; the hub now refuses as disabled: one notice, once.
	refusing := hubCaller(httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"error":"tasks are not enabled for this project; an admin enables them"}`))
	})).URL, "aimem_user_alice", http.DefaultClient)
	on := &srv{project: "alpha", taskState: taskStateEnabled, tasks: refusing}
	first := string(on.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}`)))
	second := string(on.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}`)))
	if !strings.Contains(first, "Restart the session") || strings.Contains(second, "Restart the session") {
		t.Fatalf("restart notice must appear exactly once:\n%s\n%s", first, second)
	}
}

// A project whose .aimem.json exists but cannot be parsed gets a refusal
// from the task tools, not a silent trip to the default hub with that
// hub's credential; the model sees why, and the fixed file works.
func TestTaskCallerRefusesUnreadableConfig(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("hub must not be called for an unreadable config: %s %s", r.Method, r.URL.Path)
	}))
	defer ts.Close()
	root := t.TempDir()
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"home": {URL: ts.URL, Token: "checkpoint", TaskToken: "aimem_user_alice"}}, "home"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, ".aimem.json")
	if err := os.WriteFile(cfg, []byte(`{"hub": "work",`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := taskCallerIn(dir, root); !errors.Is(err, ident.ErrConfigUnreadable) {
		t.Fatalf("unreadable config: err=%v; want ErrConfigUnreadable", err)
	}
	s := &srv{project: "alpha", taskSetup: func() (TaskCallFunc, error) { return taskCallerIn(dir, root) }}
	resp := s.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{}}}`))
	if !strings.Contains(string(resp), "isError") || !strings.Contains(string(resp), "cannot be parsed") {
		t.Fatalf("the model must see the refusal: %s", resp)
	}
	if err := os.WriteFile(cfg, []byte(`{"hub": "home"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := taskCallerIn(dir, root); err != nil {
		t.Fatalf("fixed config: %v", err)
	}
}

func callTool(t *testing.T, s *srv, name string, args map[string]any) (string, bool) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	var resp struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(s.handle(context.Background(), raw), &resp); err != nil {
		t.Fatal(err)
	}
	return toolText(resp.Result)
}

// A page larger than the old 1 MiB cap arrives intact; a body beyond the
// real cap is an error, never a truncated success; a malformed body is an
// error too.
func TestLocalTaskCallerLargePages(t *testing.T) {
	f := newHub(t)
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	root := t.TempDir()
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"home": {URL: ts.URL, Token: "checkpoint", TaskToken: f.alice}}, "home"); err != nil {
		t.Fatal(err)
	}
	s := &srv{project: "alpha", taskSetup: func() (TaskCallFunc, error) { return taskCallerFor(root, "") }}
	text, isErr := callTool(t, s, "create_task", map[string]any{"title": "big", "idempotency_key": "big"})
	if isErr {
		t.Fatal(text)
	}
	var task struct {
		ID string `json:"id"`
	}
	json.Unmarshal([]byte(text), &task)
	body := strings.Repeat("x", 30*1024)
	for i := range 40 {
		if text, isErr := callTool(t, s, "add_task_comment", map[string]any{"id": task.ID, "body": body, "idempotency_key": "c" + strconv.Itoa(i)}); isErr {
			t.Fatal(text)
		}
	}
	text, isErr = callTool(t, s, "list_task_comments", map[string]any{"id": task.ID, "limit": 40})
	if isErr {
		t.Fatalf("large page: %s", text)
	}
	var page struct {
		Comments []struct{ Body string } `json:"comments"`
	}
	if err := json.Unmarshal([]byte(text), &page); err != nil || len(page.Comments) != 40 || len(page.Comments[39].Body) != 30*1024 {
		t.Fatalf("large page must arrive intact: %v %d", err, len(page.Comments))
	}
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tasks":["`))
		w.Write(bytes.Repeat([]byte("y"), maxTaskResponseBytes))
		w.Write([]byte(`"]}`))
	}))
	defer huge.Close()
	s2 := &srv{project: "alpha", tasks: hubCaller(huge.URL, "aimem_user_x", huge.Client())}
	if text, isErr := callTool(t, s2, "list_tasks", nil); !isErr || !strings.Contains(text, "exceeds") {
		t.Fatalf("oversized body must be an error: %v %s", isErr, text[:min(80, len(text))])
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"tasks": [`)) }))
	defer bad.Close()
	s3 := &srv{project: "alpha", tasks: hubCaller(bad.URL, "aimem_user_x", bad.Client())}
	if text, isErr := callTool(t, s3, "list_tasks", nil); !isErr || !strings.Contains(text, "malformed") {
		t.Fatalf("malformed body must be an error: %v %s", isErr, text)
	}
}

// The stdio facade end to end against a real hub surface.
func TestLocalTaskCallerAgainstHub(t *testing.T) {
	f := newHub(t)
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	root := t.TempDir()
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"home": {URL: ts.URL, Token: "checkpoint", TaskToken: f.alice}}, "home"); err != nil {
		t.Fatal(err)
	}
	s := &srv{project: "alpha", taskSetup: func() (TaskCallFunc, error) { return taskCallerFor(root, "") }}
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
	root2 := t.TempDir()
	if err := adapter.SaveHubs(root2, map[string]*adapter.HubConfig{"home": {URL: ts.URL, Token: "checkpoint"}}, "home"); err != nil {
		t.Fatal(err)
	}
	s3 := &srv{project: "alpha", taskSetup: func() (TaskCallFunc, error) { return taskCallerFor(root2, "") }}
	resp = s3.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_task","arguments":{"id":"x"}}}`))
	if !strings.Contains(string(resp), "aimem hub task-token home") {
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
	raw, err := json.Marshal(taskToolDefs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"required":null`) {
		t.Fatal("a null required list breaks strict MCP clients")
	}
	for _, d := range taskToolDefs {
		schema, ok := d["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Fatalf("%v: inputSchema must be an object schema", d["name"])
		}
		props := schema["properties"].(map[string]any)
		if reqAny, present := schema["required"]; present {
			required, ok := reqAny.([]string)
			if !ok {
				t.Fatalf("%v: required must be []string, got %T", d["name"], reqAny)
			}
			for _, r := range required {
				if _, ok := props[r]; !ok {
					t.Fatalf("%v: required %q not in properties", d["name"], r)
				}
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
