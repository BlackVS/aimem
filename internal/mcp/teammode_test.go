package mcp

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/teamsession"
)

const teamToken = "aimem_user_0000000000000000000000000000000000000000000000000000000000000001"

// teamHub is a scripted TLS hub for team mode. It answers the context report
// for the sessions it knows, serves the task reads, and records every
// request's path and handle.
type teamHub struct {
	ts       *httptest.Server
	mu       sync.Mutex
	sessions map[string]string // handle -> session ID
	stale    map[string]bool   // handles answered with context_stale
	log      []recorded
	caFile   string
	// custom, when a test sets it, answers a request for a known session
	// first (returning true) before the default read answer.
	custom func(w http.ResponseWriter, r *http.Request, session string) bool
}

type recorded struct{ path, handle, auth string }

func newTeamHub(t *testing.T) *teamHub {
	t.Helper()
	h := &teamHub{sessions: map[string]string{}, stale: map[string]bool{}}
	h.ts = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handle := r.Header.Get(teamsession.Header)
		h.mu.Lock()
		h.log = append(h.log, recorded{r.URL.Path, handle, r.Header.Get("Authorization")})
		session, known := h.sessions[handle]
		stale := h.stale[handle]
		custom := h.custom
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case !known || stale:
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"code":"context_stale","message":"The team session is no longer current.","active_mode":"team","retryable":false,"next_action":"Revalidate the session through aicrew.","correlation_id":"c-1"}`))
		case custom != nil && r.URL.Path != "/v1/access/identity" && custom(w, r, session):
		case r.URL.Path == "/v1/access/identity":
			json.NewEncoder(w).Encode(map[string]any{"mode": "team", "user_id": "user-1", "token_id": "tok-1",
				"team":     map[string]string{"service_id": "aicrew-example", "team_id": "team-1", "session_id": session, "generation": "4", "role": "worker"},
				"projects": []string{"alpha"}, "knowledge": "unavailable"})
		default:
			json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Path, "session": session})
		}
	}))
	t.Cleanup(h.ts.Close)
	h.caFile = filepath.Join(t.TempDir(), "hub-ca.pem")
	os.WriteFile(h.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: h.ts.Certificate().Raw}), 0o644)
	return h
}

func (h *teamHub) requests() []recorded {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recorded(nil), h.log...)
}

func (h *teamHub) addSession(handle, session string) {
	h.mu.Lock()
	h.sessions[handle] = session
	h.mu.Unlock()
}

func teamHandle(c byte) string { return "acs1_" + strings.Repeat(string(c), 43) }

// teamRoot is a state root whose hub.json names the team hub with the
// individual credential and the hub's CA.
func teamRoot(t *testing.T, h *teamHub, edit func(*adapter.HubConfig)) string {
	t.Helper()
	root := t.TempDir()
	cfg := &adapter.HubConfig{URL: h.ts.URL, TaskToken: teamToken, CAFile: h.caFile}
	if edit != nil {
		edit(cfg)
	}
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": cfg}, "hub"); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeSession(t *testing.T, root, url, session, handle string) string {
	t.Helper()
	path := teamsession.PathFor(root, session)
	f := teamsession.File{Version: 1, Hub: "hub", URL: url, UserID: "user-1", TokenID: "tok-1", ServiceID: "aicrew-example",
		TeamID: "team-1", SessionID: session, Generation: "4", Handle: handle, HandleExpiresAt: "2026-09-27T12:00:00Z", UpdatedAt: time.Now().UTC()}
	if err := teamsession.Save(path, f); err != nil {
		t.Fatal(err)
	}
	return path
}

func teamCall(t *testing.T, s *srv, name string, args any) (string, bool) {
	t.Helper()
	raw, _ := json.Marshal(args)
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": json.RawMessage(raw)})
	out := s.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+string(params)+`}`))
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &resp); err != nil || len(resp.Result.Content) == 0 {
		t.Fatalf("%s: %s", name, out)
	}
	return resp.Result.Content[0].Text, resp.Result.IsError
}

func TestTeamModeToolListMatchesHubTeamRoutes(t *testing.T) {
	h := newTeamHub(t)
	root := teamRoot(t, h, nil)
	s := newTeamSrv(writeSession(t, root, h.ts.URL, "sess-1", teamHandle('A')), root, "alpha")
	var list struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	json.Unmarshal(s.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)), &list)
	var names []string
	for _, tl := range list.Result.Tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	// Each team tool reaches exactly one route the hub serves in team mode.
	toolRoute := map[string]string{
		"list_tasks": "GET /v1/projects/{p}/tasks", "get_task": "GET /v1/tasks/{id}", "get_task_history": "GET /v1/tasks/{id}/history",
		"list_task_comments": "GET /v1/tasks/{id}/comments", "get_task_comment": "GET /v1/tasks/{id}/comments/{c}",
		"list_epics": "GET /v1/projects/{p}/epics", "get_epic": "GET /v1/projects/{p}/epics/{e}",
		sessionContextTool: "GET /v1/access/identity",
		// The member reservation tools (C6a).
		"task_reservation_claim":    "POST /v1/projects/{p}/tasks/{task_id}/reservation/claim",
		"task_reservation_transfer": "POST /v1/projects/{p}/tasks/{task_id}/reservation/transfer",
		"task_reservation_update":   "POST /v1/projects/{p}/tasks/{task_id}/reservation/update",
		"task_reservation_release":  "POST /v1/projects/{p}/tasks/{task_id}/reservation/release",
		"task_reservation_finalize": "POST /v1/projects/{p}/tasks/{task_id}/reservation/finalize",
		"task_reservation_status":   "GET /v1/projects/{p}/tasks/{task_id}/reservation",
		"task_reservation_receipt":  "GET /v1/projects/{p}/tasks/{task_id}/reservation/receipts/{operation}/{request_key}",
		// The pilot's knowledge reads (19d8).
		"recall_memory": "GET /v1/projects/{p}/memories/recall",
		"list_docs":     "GET /v1/projects/{p}/docs",
		"read_doc":      "GET /v1/projects/{p}/docs/{name}",
	}
	// Local tools read only this binary and reach no route.
	localOnly := []string{writingTool}
	want := append([]string{}, localOnly...)
	for n := range toolRoute {
		want = append(want, n)
	}
	sort.Strings(want)
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("team tool list %v, want %v", names, want)
	}
	// The hub's team routes are the operations OpenAPI marks x-team-mode.
	raw, err := os.ReadFile(filepath.Join("..", "server", "openapi.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	var hubRoutes, toolRoutes []string
	for path, ops := range spec.Paths {
		for method, op := range ops {
			if _, ok := op["x-team-mode"]; ok {
				hubRoutes = append(hubRoutes, strings.ToUpper(method)+" "+path)
			}
		}
	}
	for _, r := range toolRoute {
		toolRoutes = append(toolRoutes, r)
	}
	sort.Strings(hubRoutes)
	sort.Strings(toolRoutes)
	if strings.Join(hubRoutes, ",") != strings.Join(toolRoutes, ",") {
		t.Fatalf("hub team routes %v, team tools reach %v", hubRoutes, toolRoutes)
	}
	if len(h.requests()) != 0 {
		t.Fatal("listing tools called the hub")
	}
	// The rule is served in a team conversation without asking any hub.
	if text, isErr := teamCall(t, s, writingTool, map[string]any{}); isErr || !strings.Contains(text, "=== end aimem-writing-rule") {
		t.Fatalf("writing_rule in team mode: %v %.200s", isErr, text)
	}
	if len(h.requests()) != 0 {
		t.Fatal("writing_rule called the hub")
	}
}

func TestTeamModeCarriesTheHandleAndVerifiesFirst(t *testing.T) {
	h := newTeamHub(t)
	root := teamRoot(t, h, nil)
	h.addSession(teamHandle('A'), "sess-1")
	path := writeSession(t, root, h.ts.URL, "sess-1", teamHandle('A'))
	s := newTeamSrv(path, root, "alpha")

	// Hidden tools are refused and never reach any hub.
	for _, name := range []string{"remember", "search_journal", "update_doc", "get_design_doc", "create_task", "update_task", "add_task_comment", "team_join", "team_context", "process_context", "team_setup"} {
		if text, isErr := teamCall(t, s, name, map[string]any{}); !isErr || !strings.Contains(text, "not available in a team conversation") {
			t.Errorf("%s in team mode: %v %s", name, isErr, text)
		}
	}
	if len(h.requests()) != 0 {
		t.Fatal("a refused tool reached the hub")
	}
	// The first read verifies the context online, then reads.
	if text, isErr := teamCall(t, s, "list_tasks", map[string]any{"project": "alpha"}); isErr || !strings.Contains(text, "sess-1") {
		t.Fatalf("list_tasks: %v %s", isErr, text)
	}
	reqs := h.requests()
	if len(reqs) != 2 || reqs[0].path != "/v1/access/identity" || reqs[1].path != "/v1/projects/alpha/tasks" {
		t.Fatalf("requests: %+v", reqs)
	}
	// A verified context stands until a refusal; every call carries the
	// handle and only the individual credential.
	teamCall(t, s, "get_task", map[string]any{"id": "t-1"})
	if text, isErr := teamCall(t, s, sessionContextTool, map[string]any{}); isErr || !strings.Contains(text, `"mode": "team"`) && !strings.Contains(text, `"mode":"team"`) {
		t.Fatalf("session_context: %v %s", isErr, text)
	}
	for _, r := range h.requests() {
		if r.handle != teamHandle('A') || r.auth != "Bearer "+teamToken {
			t.Fatalf("a request without the handle or the individual credential: %+v", r)
		}
	}
	if n := len(h.requests()); n != 4 {
		t.Fatalf("%d requests; a standing verification must not repeat", n)
	}

	// A refresh replaces the handle for the next call.
	h.addSession(teamHandle('B'), "sess-1")
	writeSession(t, root, h.ts.URL, "sess-1", teamHandle('B'))
	teamCall(t, s, "get_task", map[string]any{"id": "t-1"})
	if last := h.requests()[len(h.requests())-1]; last.handle != teamHandle('B') {
		t.Fatalf("the refreshed handle was not used: %+v", last)
	}

	// A refusal passes the envelope through and forces a new verification.
	h.mu.Lock()
	h.stale[teamHandle('B')] = true
	h.mu.Unlock()
	text, isErr := teamCall(t, s, "get_task", map[string]any{"id": "t-1"})
	if !isErr || !strings.Contains(text, "context_stale") || !strings.Contains(text, "Revalidate the session through aicrew") {
		t.Fatalf("stale context: %v %s", isErr, text)
	}
	before := len(h.requests())
	text, isErr = teamCall(t, s, "get_task", map[string]any{"id": "t-1"})
	if !isErr || !strings.Contains(text, "not verified") || len(h.requests()) != before+1 || h.requests()[before].path != "/v1/access/identity" {
		t.Fatalf("after a refusal the next call must re-verify and stop: %v %s", isErr, text)
	}
}

func TestTeamModeBlocksOnAChangedOrMissingBinding(t *testing.T) {
	h := newTeamHub(t)
	root := teamRoot(t, h, nil)
	h.addSession(teamHandle('A'), "sess-1")
	h.addSession(teamHandle('C'), "sess-2")
	path := writeSession(t, root, h.ts.URL, "sess-1", teamHandle('A'))
	s := newTeamSrv(path, root, "alpha")
	if _, isErr := teamCall(t, s, "get_task", map[string]any{"id": "t-1"}); isErr {
		t.Fatal("the first read failed")
	}
	// The file now names another session: blocked for good, even after the
	// original is restored.
	os.Remove(path)
	f := teamsession.File{Version: 1, Hub: "hub", URL: h.ts.URL, UserID: "user-1", TokenID: "tok-1", ServiceID: "aicrew-example",
		TeamID: "team-1", SessionID: "sess-2", Generation: "4", Handle: teamHandle('C'), UpdatedAt: time.Now()}
	if err := teamsession.Save(path, f); err != nil {
		t.Fatal(err)
	}
	before := len(h.requests())
	if text, isErr := teamCall(t, s, "get_task", map[string]any{"id": "t-1"}); !isErr || !strings.Contains(text, "blocked") {
		t.Fatalf("a swapped session: %v %s", isErr, text)
	}
	writeSession(t, root, h.ts.URL, "sess-1", teamHandle('A'))
	if text, isErr := teamCall(t, s, "session_context", map[string]any{}); !isErr || !strings.Contains(text, "blocked") {
		t.Fatalf("a restored file must not unblock: %v %s", isErr, text)
	}
	if len(h.requests()) != before {
		t.Fatal("a blocked process reached the hub")
	}

	// A missing file blocks at start and on the first call.
	root2 := teamRoot(t, h, nil)
	s2 := newTeamSrv(filepath.Join(teamsession.Dir(root2), "absent.json"), root2, "alpha")
	if text, isErr := teamCall(t, s2, "list_tasks", map[string]any{"project": "alpha"}); !isErr || !strings.Contains(text, "cannot be used") {
		t.Fatalf("missing file: %v %s", isErr, text)
	}
	// A file deleted mid-session blocks.
	path3 := writeSession(t, root2, h.ts.URL, "sess-1", teamHandle('A'))
	s3 := newTeamSrv(path3, root2, "alpha")
	teamCall(t, s3, "get_task", map[string]any{"id": "t-1"})
	os.Remove(path3)
	if text, isErr := teamCall(t, s3, "get_task", map[string]any{"id": "t-1"}); !isErr || !strings.Contains(text, "gone or unusable") {
		t.Fatalf("deleted file: %v %s", isErr, text)
	}
	// The hub reporting another session than the file names blocks.
	root4 := teamRoot(t, h, nil)
	h.addSession(teamHandle('D'), "sess-other")
	s4 := newTeamSrv(writeSession(t, root4, h.ts.URL, "sess-4", teamHandle('D')), root4, "alpha")
	if text, isErr := teamCall(t, s4, "get_task", map[string]any{"id": "t-1"}); !isErr || !strings.Contains(text, "different user, team or session") {
		t.Fatalf("mismatched report: %v %s", isErr, text)
	}
}

func TestTwoTeamConversationsInOneCheckoutStayApart(t *testing.T) {
	h := newTeamHub(t)
	root := teamRoot(t, h, nil)
	h.addSession(teamHandle('A'), "sess-1")
	h.addSession(teamHandle('B'), "sess-2")
	s1 := newTeamSrv(writeSession(t, root, h.ts.URL, "sess-1", teamHandle('A')), root, "alpha")
	s2 := newTeamSrv(writeSession(t, root, h.ts.URL, "sess-2", teamHandle('B')), root, "alpha")
	for i := 0; i < 3; i++ {
		t1, _ := teamCall(t, s1, "get_task", map[string]any{"id": "t-1"})
		t2, _ := teamCall(t, s2, "get_task", map[string]any{"id": "t-1"})
		if !strings.Contains(t1, "sess-1") || !strings.Contains(t2, "sess-2") {
			t.Fatalf("round %d: %s / %s", i, t1, t2)
		}
	}
	for _, r := range h.requests() {
		if r.handle != teamHandle('A') && r.handle != teamHandle('B') {
			t.Fatalf("a request carried %q", r.handle)
		}
	}
}

func TestTeamModeRefusesAnUnverifiedHub(t *testing.T) {
	h := newTeamHub(t)
	h.addSession(teamHandle('A'), "sess-1")
	for name, edit := range map[string]func(*adapter.HubConfig){
		"insecure":             func(c *adapter.HubConfig) { c.Insecure = true },
		"system roots only":    func(c *adapter.HubConfig) { c.CAFile = "" },
		"project token":        func(c *adapter.HubConfig) { c.TaskToken = "" },
		"URL changed mid-flow": func(c *adapter.HubConfig) { c.URL = strings.Replace(c.URL, "127.0.0.1", "localhost", 1) },
	} {
		root := teamRoot(t, h, edit)
		s := newTeamSrv(writeSession(t, root, h.ts.URL, "sess-1", teamHandle('A')), root, "alpha")
		before := len(h.requests())
		if text, isErr := teamCall(t, s, "get_task", map[string]any{"id": "t-1"}); !isErr {
			t.Errorf("%s: served: %s", name, text)
		}
		if len(h.requests()) != before {
			t.Errorf("%s: an unverified hub was contacted", name)
		}
	}
}

// TestTeamModeSurvivesRefreshesDuringCalls: aicrew refreshes the handle while
// the conversation keeps calling; no call is blocked by the replacement.
func TestTeamModeSurvivesRefreshesDuringCalls(t *testing.T) {
	h := newTeamHub(t)
	root := teamRoot(t, h, nil)
	handles := []string{teamHandle('A'), teamHandle('B')}
	for _, hd := range handles {
		h.addSession(hd, "sess-1")
	}
	path := writeSession(t, root, h.ts.URL, "sess-1", handles[0])
	s := newTeamSrv(path, root, "alpha")
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 40; i++ {
			writeSession(t, root, h.ts.URL, "sess-1", handles[i%2])
		}
	}()
	for i := 0; i < 40; i++ {
		if text, isErr := teamCall(t, s, "get_task", map[string]any{"id": "t-1"}); isErr {
			t.Fatalf("call %d during refreshes: %s", i, text)
		}
	}
	<-done
}

// TestTeamModeRefusedSessionContextWithdrawsVerification: a verified read,
// then a refused session_context, then a read — the last read must verify
// again, and stop while verification fails.
func TestTeamModeRefusedSessionContextWithdrawsVerification(t *testing.T) {
	h := newTeamHub(t)
	root := teamRoot(t, h, nil)
	h.addSession(teamHandle('A'), "sess-1")
	s := newTeamSrv(writeSession(t, root, h.ts.URL, "sess-1", teamHandle('A')), root, "alpha")
	if _, isErr := teamCall(t, s, "get_task", map[string]any{"id": "t-1"}); isErr {
		t.Fatal("the first read failed")
	}
	h.mu.Lock()
	h.stale[teamHandle('A')] = true
	h.mu.Unlock()
	if text, isErr := teamCall(t, s, sessionContextTool, map[string]any{}); !isErr || !strings.Contains(text, "context_stale") {
		t.Fatalf("refused session_context: %v %s", isErr, text)
	}
	before := len(h.requests())
	text, isErr := teamCall(t, s, "get_task", map[string]any{"id": "t-1"})
	reqs := h.requests()[before:]
	if !isErr || len(reqs) != 1 || reqs[0].path != "/v1/access/identity" {
		t.Fatalf("the read after a refused session_context must re-verify and stop: %v %s %+v", isErr, text, reqs)
	}
}
