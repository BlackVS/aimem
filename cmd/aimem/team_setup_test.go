package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/taskcred"
)

// fakeTeamHub is the smallest hub the setup command can talk to: identity,
// status, process selection, join, roster, heartbeat, resume, reserved
// attempt and inbox, with switches for every refusal the command maps.
type fakeTeamHub struct {
	mu       sync.Mutex
	secret   string
	identity map[string]any
	joinCode int    // 0 means 201
	joinMsg  string // error text for a refusal
	joinKeys []string
	garbage  int // number of join replies to send as unreadable 201s
	sessions map[string]*fakeSession
	nextID   int
	roster   int // 0 means 200; 409 means closed/stale
	resumes  int
	inboxN   int
	reserved map[string]any // nil means 404
	requests []string

	rosterFails   int               // roster replies to refuse with 500 before serving
	rosterPages   int               // >0: serve this many filler pages before the real roster
	heartbeat     int               // 0 means 200
	resumeGarbage int               // resume replies to send as unreadable 200s
	resumeKeys    map[string]string // idempotency key -> session id already resumed
}

type fakeSession struct {
	id, role, state string
	generation      int64
	suspect         bool
	profile         json.RawMessage
}

func newFakeTeamHub(t *testing.T) (*fakeTeamHub, *httptest.Server) {
	h := &fakeTeamHub{secret: "aimem_user_" + strings.Repeat("e", 64), sessions: map[string]*fakeSession{}, resumeKeys: map[string]string{}}
	h.identity = map[string]any{"user_id": "u-1", "token_id": "t-1", "name": "pilot-a", "role": "user", "scope": "project", "task_read": "all-projects", "project": "alpha", "task_write": true, "tasks_enabled": true}
	ts := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(ts.Close)
	return h, ts
}

func (h *fakeTeamHub) view(s *fakeSession) map[string]any {
	v := map[string]any{"id": s.id, "team_id": "team-1", "generation": s.generation, "role": s.role, "state": s.state, "availability": "available", "profile_revision": 1, "last_seen_at": "2026-09-23T04:00:00Z", "suspect": s.suspect}
	if s.role == "coordinator" {
		v["coordinator_generation"] = s.generation
	}
	var p map[string]any
	json.Unmarshal(s.profile, &p)
	for k, val := range p {
		v[k] = val
	}
	return v
}

func (h *fakeTeamHub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.requests = append(h.requests, r.Method+" "+r.URL.Path)
	write := func(code int, v any) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	if r.URL.Path == "/v1/status" {
		write(200, map[string]any{"status": "ok", "version": "v0.7.0", "hub_name": "fake"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+h.secret {
		write(401, map[string]any{"error": "bad credential"})
		return
	}
	switch {
	case r.URL.Path == "/v1/access/identity":
		write(200, h.identity)
	case r.URL.Path == "/v1/projects/alpha/process":
		write(404, map[string]any{"error": "no process reference selected"})
	case strings.HasSuffix(r.URL.Path, "/join"):
		if r.Header.Get("Idempotency-Key") == "" {
			write(400, map[string]any{"error": "Idempotency-Key header is required for task writes"})
			return
		}
		h.joinKeys = append(h.joinKeys, r.Header.Get("Idempotency-Key"))
		if h.joinCode != 0 {
			write(h.joinCode, map[string]any{"error": h.joinMsg})
			return
		}
		var body struct {
			Role    string          `json:"role"`
			Profile json.RawMessage `json:"profile"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		for _, k := range h.joinKeys[:len(h.joinKeys)-1] {
			if k == r.Header.Get("Idempotency-Key") {
				// Replay: the original session.
				for _, s := range h.sessions {
					write(201, map[string]any{"protocol_version": 1, "session": h.view(s), "server_time": "2026-09-23T04:00:01Z"})
					return
				}
			}
		}
		if body.Role == "coordinator" {
			for _, s := range h.sessions {
				if s.role == "coordinator" && s.state == "active" {
					write(409, map[string]any{"error": "team coordinator slot is occupied"})
					return
				}
			}
		}
		h.nextID++
		s := &fakeSession{id: fmt.Sprintf("sess-%d", h.nextID), role: body.Role, state: "active", generation: 1, profile: body.Profile}
		h.sessions[s.id] = s
		if h.garbage > 0 {
			h.garbage--
			w.WriteHeader(201)
			w.Write([]byte("not json"))
			return
		}
		write(201, map[string]any{"protocol_version": 1, "session": h.view(s), "server_time": "2026-09-23T04:00:01Z"})
	case strings.HasSuffix(r.URL.Path, "/members"):
		if h.roster != 0 {
			write(h.roster, map[string]any{"error": "team session is closed or generation is stale"})
			return
		}
		s := h.sessions[r.URL.Query().Get("session_id")]
		if s == nil || s.state != "active" || fmt.Sprint(s.generation) != r.URL.Query().Get("generation") {
			write(409, map[string]any{"error": "team session is closed or generation is stale"})
			return
		}
		if h.rosterFails > 0 {
			h.rosterFails--
			write(500, map[string]any{"error": "team storage failure"})
			return
		}
		if h.rosterPages > 0 {
			// Filler pages of historical sessions that are not ours.
			h.rosterPages--
			filler := []map[string]any{h.view(&fakeSession{id: "old-" + r.URL.Query().Get("after"), role: "worker", state: "left", generation: 1, profile: json.RawMessage(`{"label":"old"}`)})}
			write(200, map[string]any{"protocol_version": 1, "members": filler, "next_cursor": fmt.Sprintf("c%d", h.rosterPages), "server_time": "2026-09-23T04:00:02Z"})
			return
		}
		var members []map[string]any
		for _, m := range h.sessions {
			members = append(members, h.view(m))
		}
		write(200, map[string]any{"protocol_version": 1, "members": members, "next_cursor": "", "server_time": "2026-09-23T04:00:02Z"})
	case strings.HasSuffix(r.URL.Path, "/resume"):
		key := r.Header.Get("Idempotency-Key")
		if id, seen := h.resumeKeys[key]; seen {
			// Replay: the original result, whatever the generation is now.
			write(200, map[string]any{"protocol_version": 1, "session": h.view(h.sessions[id]), "server_time": "2026-09-23T04:00:03Z"})
			return
		}
		var body struct {
			SessionID  string `json:"session_id"`
			Generation int64  `json:"generation"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		s := h.sessions[body.SessionID]
		if s == nil || s.state != "active" || s.generation != body.Generation {
			write(409, map[string]any{"error": "team session is closed or generation is stale"})
			return
		}
		h.resumes++
		s.generation++
		s.suspect = false
		h.resumeKeys[key] = s.id
		if h.resumeGarbage > 0 {
			h.resumeGarbage--
			w.WriteHeader(200)
			w.Write([]byte("not json"))
			return
		}
		write(200, map[string]any{"protocol_version": 1, "session": h.view(s), "server_time": "2026-09-23T04:00:03Z"})
	case strings.HasSuffix(r.URL.Path, "/heartbeat"):
		if h.heartbeat != 0 {
			write(h.heartbeat, map[string]any{"error": "current team enrollment and bound ordinary write session required"})
			return
		}
		write(200, map[string]any{"protocol_version": 1, "session": map[string]any{"id": "x"}})
	case strings.HasSuffix(r.URL.Path, "/assignments/reserved"):
		if h.reserved == nil {
			write(404, map[string]any{"error": "no reserved attempt"})
			return
		}
		write(200, map[string]any{"protocol_version": 1, "assignment": h.reserved, "workflow_ready": true})
	case strings.HasSuffix(r.URL.Path, "/inbox"):
		msgs := []map[string]any{}
		for i := 0; i < h.inboxN; i++ {
			msgs = append(msgs, map[string]any{"id": fmt.Sprintf("m-%d", i)})
		}
		write(200, map[string]any{"protocol_version": 1, "messages": msgs, "next_cursor": h.inboxN, "has_more": false, "server_time": "2026-09-23T04:00:04Z"})
	default:
		write(404, map[string]any{"error": "unexpected " + r.URL.Path})
	}
}

func (h *fakeTeamHub) count(suffix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.requests {
		if strings.HasSuffix(r, suffix) {
			n++
		}
	}
	return n
}

// setupCheckout binds a temporary checkout to the fake hub with a
// project-local credential and returns (checkout, state root).
func setupCheckout(t *testing.T, h *fakeTeamHub, ts *httptest.Server) (string, string) {
	t.Helper()
	root, repo := t.TempDir(), t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, Token: "checkpoint"}}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".aimem.json"), []byte(`{"project":"alpha"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := taskcred.Set(context.Background(), repo, root, h.secret); err != nil {
		t.Fatal(err)
	}
	return repo, root
}

func runSetup(t *testing.T, repo, root string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runTeamSetup(args, repo, root, &out)
	if strings.Contains(out.String(), "aimem_user_") {
		t.Fatalf("token leaked into output: %s", out.String())
	}
	return out.String(), err
}

func readState(t *testing.T, root, repo string) *teamSetupState {
	t.Helper()
	canon, _ := filepath.EvalSymlinks(repo)
	raw, err := os.ReadFile(teamSetupStatePath(root, canon))
	if err != nil {
		return nil
	}
	if strings.Contains(string(raw), "aimem_user_") {
		t.Fatal("token leaked into the state file")
	}
	var st teamSetupState
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return &st
}

func TestTeamSetupJoinsOnceAndVerifiesOnRepeat(t *testing.T) {
	h, ts := newFakeTeamHub(t)
	repo, root := setupCheckout(t, h, ts)
	out, err := runSetup(t, repo, root, "Pilot", "coordinator", "--platform", "claude-code", "--platform-version", "2.1", "--model-id", "claude-fable-5-1", "--model-source", "runtime_reported", "--model-provider", "anthropic")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"joined as coordinator: session sess-1, generation 1", "Roster (1)", "coordinator playbook", "Status: joined", "identity     user pilot-a, scope project, write granted, tasks enabled", "claude-fable-5-1 (runtime_reported)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	st := readState(t, root, repo)
	if st == nil || st.SessionID != "sess-1" || st.Generation != 1 || st.TokenID != "t-1" || st.TeamID != "team-1" || st.JoinKey != "" || st.Role != "coordinator" {
		t.Fatalf("state %+v", st)
	}
	// Second invocation: verified through the roster, no second join, no resume.
	out, err = runSetup(t, repo, root, "Pilot", "coordinator")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "already joined as coordinator: session sess-1, generation 1") || h.count("/join") != 1 || h.resumes != 0 {
		t.Fatalf("repeat invocation: joins %d resumes %d\n%s", h.count("/join"), h.resumes, out)
	}
	// A different role for the same checkout is refused, not silently switched.
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "already holds a coordinator session") {
		t.Fatalf("role switch: %v\n%s", err, out)
	}
	// JSON report carries the same facts.
	out, err = runSetup(t, repo, root, "Pilot", "coordinator", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rep teamSetupReport
	if json.Unmarshal([]byte(out), &rep) != nil || rep.Status != "joined" || rep.Session == nil || rep.Session.ID != "sess-1" {
		t.Fatalf("json report: %s", out)
	}
}

func TestTeamSetupWorkerEntersWaiting(t *testing.T) {
	h, ts := newFakeTeamHub(t)
	h.inboxN = 2
	h.reserved = map[string]any{"id": "att-1", "task_id": "task-1", "state": "OFFERED"}
	repo, root := setupCheckout(t, h, ts)
	out, err := runSetup(t, repo, root, "team-1", "worker", "--platform", "codex", "--capabilities", "go, unit-tests")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"joined as worker", "announced available", "2 unacknowledged message(s)", "attempt att-1 on task task-1 is OFFERED", "never select, claim or edit backlog tasks", "Status: joined"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if h.count("/heartbeat") != 1 || h.count("/inbox") != 1 || h.count("/assignments/reserved") != 1 {
		t.Fatalf("requests %v", h.requests)
	}
	var caps []string
	for _, s := range h.sessions {
		var p struct {
			Capabilities []string                `json:"capabilities"`
			Platform     string                  `json:"platform"`
			Model        struct{ Source string } `json:"model"`
		}
		json.Unmarshal(s.profile, &p)
		caps = p.Capabilities
		if p.Platform != "codex" || p.Model.Source != "unknown" {
			t.Fatalf("profile %s", s.profile)
		}
	}
	if len(caps) != 2 || caps[1] != "unit-tests" {
		t.Fatalf("capabilities %v", caps)
	}
}

func TestTeamSetupResumeAndStaleHandle(t *testing.T) {
	h, ts := newFakeTeamHub(t)
	repo, root := setupCheckout(t, h, ts)
	if out, err := runSetup(t, repo, root, "Pilot", "worker"); err != nil {
		t.Fatal(err, out)
	}
	// A suspect session (no heartbeat) is the restart case: resumed automatically.
	h.sessions["sess-1"].suspect = true
	out, err := runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "resumed session sess-1 (no heartbeat since") || h.resumes != 1 {
		t.Fatalf("%v resumes %d\n%s", err, h.resumes, out)
	}
	if st := readState(t, root, repo); st.Generation != 2 || st.ResumeKey != "" {
		t.Fatalf("state after resume %+v", st)
	}
	// A live session is only verified; --resume fences it explicitly.
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "already joined as worker: session sess-1, generation 2") || h.resumes != 1 {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err = runSetup(t, repo, root, "Pilot", "worker", "--resume")
	if err != nil || !strings.Contains(out, "resumed session sess-1 (--resume requested): generation 3") || h.resumes != 2 {
		t.Fatalf("%v\n%s", err, out)
	}
	// Another process advanced the handle: reported, never taken over.
	h.sessions["sess-1"].generation = 9
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "advanced by another process") || h.resumes != 2 || h.count("/join") != 1 {
		t.Fatalf("%v joins %d resumes %d\n%s", err, h.count("/join"), h.resumes, out)
	}
	// --new-session discards the handle and joins afresh.
	out, err = runSetup(t, repo, root, "Pilot", "worker", "--new-session")
	if err != nil || !strings.Contains(out, "joined as worker: session sess-2") || h.count("/join") != 2 {
		t.Fatalf("%v\n%s", err, out)
	}
	// A worker session that left outside this command looks like a stale
	// handle (the hub refuses both alike): explicit choice, no silent duplicate.
	h.sessions["sess-2"].state = "left"
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "closed or was advanced by another process") || h.count("/join") != 2 {
		t.Fatalf("%v\n%s", err, out)
	}
	// A leave through the CLI clears the handle, so the next setup joins afresh.
	canon, _ := filepath.EvalSymlinks(repo)
	noteTeamLeave(canon, root, "sess-2")
	if readState(t, root, repo) != nil {
		t.Fatal("leave did not clear the saved handle")
	}
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "joined as worker: session sess-3") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTeamSetupCoordinatorRejoinsAfterLeaveButNotOverLiveSlot(t *testing.T) {
	h, ts := newFakeTeamHub(t)
	repo, root := setupCheckout(t, h, ts)
	if out, err := runSetup(t, repo, root, "Pilot", "coordinator", "--platform", "claude-code", "--platform-version", "2.1", "--model-id", "claude-fable-5-1", "--model-source", "runtime_reported"); err != nil {
		t.Fatal(err, out)
	}
	// The old coordinator session is still active but its generation moved
	// (another process resumed it): the hub refuses the fresh join.
	h.sessions["sess-1"].generation = 5
	out, err := runSetup(t, repo, root, "Pilot", "coordinator")
	if err == nil || !strings.Contains(out, "joining afresh") || !strings.Contains(out, "coordinator slot is occupied") || h.resumes != 0 {
		t.Fatalf("%v\n%s", err, out)
	}
	// After that session left, the same command joins afresh, and a bare
	// re-run keeps the declaration the first run made.
	h.sessions["sess-1"].state = "left"
	out, err = runSetup(t, repo, root, "Pilot", "coordinator")
	if err != nil || !strings.Contains(out, "joined as coordinator: session sess-2") || !strings.Contains(out, "platform claude-code 2.1; model claude-fable-5-1 (runtime_reported)") {
		t.Fatalf("%v\n%s", err, out)
	}
	if st := readState(t, root, repo); st == nil || st.SessionID != "sess-2" {
		t.Fatalf("state %+v", st)
	}
}

func TestTeamSetupRefusalsAndHandoff(t *testing.T) {
	h, ts := newFakeTeamHub(t)
	repo, root := setupCheckout(t, h, ts)
	h.joinCode, h.joinMsg = 403, "current team enrollment and bound ordinary write session required"
	out, err := runSetup(t, repo, root, "Pilot", "coordinator")
	if err == nil || !strings.Contains(out, "not enrolled in team \"Pilot\" as coordinator") || !strings.Contains(out, `{"user_id":"u-1","coordinator":true}`) || !strings.Contains(out, "aimem teams configure alpha <TEAM_ID>") || !strings.Contains(out, "Status: blocked") {
		t.Fatalf("%v\n%s", err, out)
	}
	if readState(t, root, repo) != nil {
		t.Fatal("a definite refusal must not leave a pending join")
	}
	h.joinCode, h.joinMsg = 404, "team or project not found"
	out, _ = runSetup(t, repo, root, "Nope", "worker")
	if !strings.Contains(out, `team "Nope" not found`) || !strings.Contains(out, "aimem teams create alpha team.json") {
		t.Fatalf("%s", out)
	}
	// Occupied coordinator slot.
	h.joinCode = 0
	h.sessions["other"] = &fakeSession{id: "other", role: "coordinator", state: "active", generation: 1, profile: json.RawMessage(`{"label":"c"}`)}
	out, err = runSetup(t, repo, root, "Pilot", "coordinator")
	if err == nil || !strings.Contains(out, "coordinator slot is occupied") || !strings.Contains(out, "never freed by a timeout") {
		t.Fatalf("%v\n%s", err, out)
	}
	// Missing grant: identity stops before any join, with the grant command.
	delete(h.sessions, "other")
	h.identity["task_write"] = false
	joins := h.count("/join")
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "no current write grant") || !strings.Contains(out, "aimem access grant add alpha user u-1") || h.count("/join") != joins {
		t.Fatalf("%v\n%s", err, out)
	}
	h.identity["task_write"] = true
	h.identity["tasks_enabled"] = false
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "aimem tasks on -p alpha") {
		t.Fatalf("%v\n%s", err, out)
	}
	h.identity["tasks_enabled"] = true
	h.identity["role"] = "admin"
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "require an ordinary user token") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTeamSetupUncertainJoinReplaysWithSameKey(t *testing.T) {
	h, ts := newFakeTeamHub(t)
	h.garbage = 1
	repo, root := setupCheckout(t, h, ts)
	out, err := runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "not team protocol 1") {
		t.Fatalf("%v\n%s", err, out)
	}
	st := readState(t, root, repo)
	if st == nil || st.JoinKey == "" || st.SessionID != "" {
		t.Fatalf("pending join not recorded: %+v", st)
	}
	out, err = runSetup(t, repo, root, "Pilot", "worker", "--platform", "ignored-on-replay")
	if err != nil || !strings.Contains(out, "joined as worker: session sess-1") {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(h.joinKeys) != 2 || h.joinKeys[0] != h.joinKeys[1] || len(h.sessions) != 1 {
		t.Fatalf("replay keys %v sessions %d", h.joinKeys, len(h.sessions))
	}
}

func TestTeamSetupIgnoresForeignState(t *testing.T) {
	h, ts := newFakeTeamHub(t)
	repo, root := setupCheckout(t, h, ts)
	if out, err := runSetup(t, repo, root, "Pilot", "worker"); err != nil {
		t.Fatal(err, out)
	}
	// The same file under another credential is ignored, not revived.
	h.identity["token_id"] = "t-2"
	out, err := runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "bound to another checkout, project, hub or credential") || !strings.Contains(out, "session sess-2") {
		t.Fatalf("%v\n%s", err, out)
	}
	// The credential store refuses a missing local override; nothing reaches the hub.
	files, _ := filepath.Glob(filepath.Join(root, "task-credentials", "*.json"))
	os.Remove(files[0])
	before := len(h.requests)
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "aimem task-token set") || len(h.requests) != before {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTeamSetupUncertainResumeReplaysBeforeAnyRead(t *testing.T) {
	h, ts := newFakeTeamHub(t)
	repo, root := setupCheckout(t, h, ts)
	if out, err := runSetup(t, repo, root, "Pilot", "worker"); err != nil {
		t.Fatal(err, out)
	}
	// The resume commits generation 2 on the hub, but its reply is unreadable.
	h.sessions["sess-1"].suspect = true
	h.resumeGarbage = 1
	out, err := runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "not team protocol 1") {
		t.Fatalf("%v\n%s", err, out)
	}
	st := readState(t, root, repo)
	if st == nil || st.ResumeKey == "" || st.Generation != 1 {
		t.Fatalf("pending resume not recorded: %+v", st)
	}
	// Next run: the pending resume is replayed with its key before any roster
	// read with the stale handle; no fresh join, no second resume.
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "resumed session sess-1 (replaying an unconfirmed resume): generation 2") {
		t.Fatalf("%v\n%s", err, out)
	}
	if st := readState(t, root, repo); st.Generation != 2 || st.ResumeKey != "" {
		t.Fatalf("state after replay %+v", st)
	}
	if h.resumes != 1 || h.count("/join") != 1 || len(h.resumeKeys) != 1 {
		t.Fatalf("resumes %d joins %d keys %d", h.resumes, h.count("/join"), len(h.resumeKeys))
	}
}

func TestTeamSetupPendingJoinKeepsTeamAndRole(t *testing.T) {
	h, ts := newFakeTeamHub(t)
	h.garbage = 1
	repo, root := setupCheckout(t, h, ts)
	if out, err := runSetup(t, repo, root, "Pilot", "coordinator"); err == nil {
		t.Fatal("unreadable join reply accepted", out)
	}
	// A different role or team does not replay the key with new content and
	// does not join afresh either.
	for _, args := range [][]string{{"Pilot", "worker"}, {"Other", "coordinator"}} {
		out, err := runSetup(t, repo, root, args...)
		if err == nil || !strings.Contains(out, "has an unconfirmed coordinator join pending") || h.count("/join") != 1 {
			t.Fatalf("%v: %v joins %d\n%s", args, err, h.count("/join"), out)
		}
	}
	// The original request replays and finds the one session the hub created.
	out, err := runSetup(t, repo, root, "Pilot", "coordinator")
	if err != nil || !strings.Contains(out, "joined as coordinator: session sess-1") || len(h.sessions) != 1 {
		t.Fatalf("%v sessions %d\n%s", err, len(h.sessions), out)
	}
}

func TestTeamSetupRoleEntryRefusalsAreFailures(t *testing.T) {
	h, ts := newFakeTeamHub(t)
	repo, root := setupCheckout(t, h, ts)
	// A coordinator whose roster read fails right after the join: the
	// membership is kept and the run is not reported as complete.
	h.rosterFails = 1
	out, err := runSetup(t, repo, root, "Pilot", "coordinator")
	if err == nil || !strings.Contains(out, "roster not read after the session was recorded: HTTP 500") || !strings.Contains(out, "Status: blocked") {
		t.Fatalf("%v\n%s", err, out)
	}
	if st := readState(t, root, repo); st == nil || st.SessionID != "sess-1" {
		t.Fatalf("membership lost: %+v", st)
	}
	out, err = runSetup(t, repo, root, "Pilot", "coordinator")
	if err != nil || !strings.Contains(out, "already joined as coordinator: session sess-1") || h.count("/join") != 1 {
		t.Fatalf("%v\n%s", err, out)
	}
	// A worker whose heartbeat is refused (enrollment revoked between join
	// and entry) is not "announced available".
	h2, ts2 := newFakeTeamHub(t)
	h2.heartbeat = 403
	repo2, root2 := setupCheckout(t, h2, ts2)
	out, err = runSetup(t, repo2, root2, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "heartbeat refused: HTTP 403") || strings.Contains(out, "announced available") {
		t.Fatalf("%v\n%s", err, out)
	}
	if st := readState(t, root2, repo2); st == nil || st.SessionID != "sess-1" {
		t.Fatalf("membership lost: %+v", st)
	}
}

func TestTeamSetupIncompleteRosterNeverJoinsAgain(t *testing.T) {
	h, ts := newFakeTeamHub(t)
	repo, root := setupCheckout(t, h, ts)
	if out, err := runSetup(t, repo, root, "Pilot", "worker"); err != nil {
		t.Fatal(err, out)
	}
	// More historical pages than the command reads: our row is never
	// reached, and that is an incomplete read, not a missing membership.
	h.rosterPages = 40
	out, err := runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "roster read incomplete") || h.count("/join") != 1 {
		t.Fatalf("%v joins %d\n%s", err, h.count("/join"), out)
	}
	if st := readState(t, root, repo); st == nil || st.SessionID != "sess-1" {
		t.Fatalf("membership lost: %+v", st)
	}
	// Two pages that do include our row verify normally.
	h.rosterPages = 2
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "already joined as worker: session sess-1") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTeamSetupIntegrationRepairAndNoRepair(t *testing.T) {
	h, ts := newFakeTeamHub(t)
	repo, root := setupCheckout(t, h, ts)
	// --no-repair reports the missing wiring and writes nothing.
	out, err := runSetup(t, repo, root, "Pilot", "worker", "--no-repair")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"warn  wiring docs/SESSION-STATE.md", "warn  wiring .claude/settings.json", "warn  wiring .mcp.json", "warn  wiring opencode.json"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".mcp.json")); err == nil {
		t.Fatal("--no-repair wrote .mcp.json")
	}
	// The default run repairs and says so; the next run finds everything present.
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "repaired: Claude Code MCP registration added") || !strings.Contains(out, "repaired: handoff template created") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repo, "opencode.json")); err != nil {
		t.Fatal("repair did not write opencode.json")
	}
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || strings.Contains(out, "repaired:") || !strings.Contains(out, "ok    wiring .codex/hooks.json Codex SessionStart handoff hook present") {
		t.Fatalf("%v\n%s", err, out)
	}
	var rep teamSetupReport
	out, _ = runSetup(t, repo, root, "Pilot", "worker", "--json")
	if json.Unmarshal([]byte(out), &rep) != nil || rep.Wiring == nil || len(rep.Wiring.Findings) < 6 {
		t.Fatalf("json integration report: %s", out)
	}
}

func TestTeamSetupProjectStopHooksBlockBeforeAnyHubCall(t *testing.T) {
	h, ts := newFakeTeamHub(t)
	repo, root := setupCheckout(t, h, ts)
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.json"), []byte(`{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "aimem submit-claude"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	before := len(h.requests)
	out, err := runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "would journal every turn twice") || !strings.Contains(out, "Status: blocked") || len(h.requests) != before {
		t.Fatalf("%v requests %d\n%s", err, len(h.requests)-before, out)
	}
	out, err = runSetup(t, repo, root, "Pilot", "worker", "--allow-project-stop-hooks")
	if err != nil || !strings.Contains(out, "joined as worker") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTeamSetupArgs(t *testing.T) {
	for _, args := range [][]string{nil, {"Pilot"}, {"Pilot", "reviewer"}, {"--json", "Pilot", "worker"}, {"Pilot", "worker", "extra"}, {"Pilot", "worker", "--model-source", "guess"}, {"Pilot", "worker", "--model-id", "x"}, {"Pilot", "worker", "--model-source", "agent_reported"}, {"Pilot", "worker", "--resume", "--new-session"}} {
		if _, err := parseTeamSetupArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	o, err := parseTeamSetupArgs([]string{"My Team", "coordinator", "--label", "coord", "--model-id", "m", "--model-source", "operator_configured"})
	if err != nil || o.team != "My Team" || o.profile.Label != "coord" || o.profile.Model.ObservedAt == "" || o.profile.Platform != "unknown" {
		t.Fatalf("%v %+v", err, o)
	}
}
