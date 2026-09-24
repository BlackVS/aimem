// Package teamsetuptest is the smallest hub the onboarding core can talk
// to: identity, status, enrollment listing, join, roster, heartbeat,
// resume, reserved attempt and inbox, with switches for every refusal the
// core maps. The CLI and the stdio MCP facade are tested against the same
// one, which is what proves they share one state machine.
package teamsetuptest

import (
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
	"aimem/internal/process"
	"aimem/internal/taskcred"
)

// Hub is the smallest hub the setup command can talk to: identity,
// status, process selection, join, roster, heartbeat, resume, reserved
// attempt and inbox, with switches for every refusal the command maps.
type Hub struct {
	mu       sync.Mutex
	Secret   string
	Identity map[string]any
	JoinCode int    // 0 means 201
	JoinMsg  string // error text for a refusal
	JoinKeys []string
	Garbage  int // number of join replies to send as unreadable 201s
	Sessions map[string]*Session
	NextID   int
	Roster   int // 0 means 200; 409 means closed/stale
	Resumes  int
	InboxN   int
	Reserved map[string]any // nil means 404
	Requests []string

	RosterFails   int               // roster replies to refuse with 500 before serving
	RosterPages   int               // >0: serve this many filler pages before the real roster
	Heartbeat     int               // 0 means 200
	ResumeGarbage int               // resume replies to send as unreadable 200s
	ResumeKeys    map[string]string // idempotency key -> session id already resumed
	Mine          []map[string]any  // enrolled-teams listing; nil means enrolled in Pilot as coordinator
	MineCode      int               // non-zero: refuse the listing with this status
	// Selection is the project's process selection; nil answers "no process
	// reference selected". ProcessCode, when set, answers the process route
	// with that status and no selection message (an older hub, an outage).
	Selection   *process.Ref
	ProcessCode int
	// HeartbeatAvailability records the availability of each accepted
	// heartbeat, in order.
	HeartbeatAvailability []string
	// Version is the hub's reported release; "" means v0.7.0.
	Version string
	// InboxCode, when set, refuses inbox reads with that status.
	InboxCode int
}

// Selection is the process every New hub selects, and Checkout caches
// exactly: its repository address is unreachable, so only the cache can
// serve it.
var Selection = process.Ref{Repo: "https://127.0.0.1:1/process.git", Commit: strings.Repeat("ab", 20), Manifest: "proc/manifest.json"}

// Handbook is the selected process's handbook text.
const Handbook = "# Handbook\n\nWork only on READY tasks; review gates apply.\n"

// CacheProcess writes a complete exact-commit cache entry for ref under
// root, the way a finished fetch leaves it.
func CacheProcess(t *testing.T, root string, ref process.Ref) {
	t.Helper()
	dir := process.CacheDir(root, ref)
	for p, body := range map[string]string{
		ref.Manifest:       `{"version":1,"handbook":"proc/handbook.md","checklists":{"READY":"proc/ready.json"},"templates":{"task":"proc/task.json"}}`,
		"proc/handbook.md": Handbook,
		"proc/ready.json":  `{"state":"READY","items":[{"id":"ready.outcome","text":"Objective and criteria are concrete."}]}`,
		"proc/task.json":   `{"title":""}`,
		".complete":        ref.Manifest + "\n",
	} {
		fp := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(fp), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

type Session struct {
	ID, Role, State string
	Generation      int64
	Suspect         bool
	Profile         json.RawMessage
}

func New(t *testing.T) (*Hub, *httptest.Server) {
	sel := Selection
	h := &Hub{Secret: "aimem_user_" + strings.Repeat("e", 64), Sessions: map[string]*Session{}, ResumeKeys: map[string]string{}, Selection: &sel}
	h.Identity = map[string]any{"user_id": "u-1", "token_id": "t-1", "name": "pilot-a", "role": "user", "scope": "project", "task_read": "all-projects", "project": "alpha", "task_write": true, "tasks_enabled": true}
	ts := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(ts.Close)
	return h, ts
}

func (h *Hub) View(s *Session) map[string]any {
	v := map[string]any{"id": s.ID, "team_id": "team-1", "generation": s.Generation, "role": s.Role, "state": s.State, "availability": "available", "profile_revision": 1, "last_seen_at": "2026-09-23T04:00:00Z", "suspect": s.Suspect}
	if s.Role == "coordinator" {
		v["coordinator_generation"] = s.Generation
	}
	var p map[string]any
	json.Unmarshal(s.Profile, &p)
	for k, val := range p {
		v[k] = val
	}
	return v
}

func (h *Hub) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Requests = append(h.Requests, r.Method+" "+r.URL.Path)
	write := func(code int, v any) {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	if r.URL.Path == "/v1/status" {
		version := h.Version
		if version == "" {
			version = "v0.7.0"
		}
		write(200, map[string]any{"status": "ok", "version": version, "hub_name": "fake"})
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+h.Secret {
		write(401, map[string]any{"error": "bad credential"})
		return
	}
	switch {
	case r.URL.Path == "/v1/access/identity":
		write(200, h.Identity)
	case r.URL.Path == "/v1/projects/alpha/process":
		switch {
		case h.ProcessCode != 0:
			w.WriteHeader(h.ProcessCode)
		case h.Selection == nil:
			write(404, map[string]any{"error": "no process reference selected"})
		default:
			write(200, map[string]any{"project": "alpha", "current": h.Selection})
		}
	case r.URL.Path == "/v1/projects/alpha/teams/mine":
		if h.MineCode == 403 {
			// What a hub older than the route says: its ordinary-token gate
			// refuses every route it does not list.
			write(403, map[string]any{"error": "ordinary token is not authorized for this endpoint"})
			return
		}
		if h.MineCode != 0 {
			write(h.MineCode, map[string]any{"error": "no such route"})
			return
		}
		teams := h.Mine
		if teams == nil {
			teams = []map[string]any{{"id": "team-1", "name": "Pilot", "description": "", "revision": 1, "coordinator": true, "coordinator_active": false}}
		}
		write(200, map[string]any{"protocol_version": 1, "teams": teams, "server_time": "2026-09-23T04:00:00Z"})
	case strings.HasSuffix(r.URL.Path, "/join"):
		if r.Header.Get("Idempotency-Key") == "" {
			write(400, map[string]any{"error": "Idempotency-Key header is required for task writes"})
			return
		}
		h.JoinKeys = append(h.JoinKeys, r.Header.Get("Idempotency-Key"))
		if h.JoinCode != 0 {
			write(h.JoinCode, map[string]any{"error": h.JoinMsg})
			return
		}
		var body struct {
			Role    string          `json:"role"`
			Profile json.RawMessage `json:"profile"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		for _, k := range h.JoinKeys[:len(h.JoinKeys)-1] {
			if k == r.Header.Get("Idempotency-Key") {
				// Replay: the original session.
				for _, s := range h.Sessions {
					write(201, map[string]any{"protocol_version": 1, "session": h.View(s), "server_time": "2026-09-23T04:00:01Z"})
					return
				}
			}
		}
		if body.Role == "coordinator" {
			for _, s := range h.Sessions {
				if s.Role == "coordinator" && s.State == "active" {
					write(409, map[string]any{"error": "team coordinator slot is occupied"})
					return
				}
			}
		}
		h.NextID++
		s := &Session{ID: fmt.Sprintf("sess-%d", h.NextID), Role: body.Role, State: "active", Generation: 1, Profile: body.Profile}
		h.Sessions[s.ID] = s
		if h.Garbage > 0 {
			h.Garbage--
			w.WriteHeader(201)
			w.Write([]byte("not json"))
			return
		}
		write(201, map[string]any{"protocol_version": 1, "session": h.View(s), "server_time": "2026-09-23T04:00:01Z"})
	case strings.HasSuffix(r.URL.Path, "/members"):
		if h.Roster != 0 {
			write(h.Roster, map[string]any{"error": "team session is closed or generation is stale"})
			return
		}
		s := h.Sessions[r.URL.Query().Get("session_id")]
		if s == nil || s.State != "active" || fmt.Sprint(s.Generation) != r.URL.Query().Get("generation") {
			write(409, map[string]any{"error": "team session is closed or generation is stale"})
			return
		}
		if h.RosterFails > 0 {
			h.RosterFails--
			write(500, map[string]any{"error": "team storage failure"})
			return
		}
		if h.RosterPages > 0 {
			// Filler pages of historical sessions that are not ours.
			h.RosterPages--
			filler := []map[string]any{h.View(&Session{ID: "old-" + r.URL.Query().Get("after"), Role: "worker", State: "left", Generation: 1, Profile: json.RawMessage(`{"label":"old"}`)})}
			write(200, map[string]any{"protocol_version": 1, "members": filler, "next_cursor": fmt.Sprintf("c%d", h.RosterPages), "server_time": "2026-09-23T04:00:02Z"})
			return
		}
		var members []map[string]any
		for _, m := range h.Sessions {
			members = append(members, h.View(m))
		}
		write(200, map[string]any{"protocol_version": 1, "members": members, "next_cursor": "", "server_time": "2026-09-23T04:00:02Z"})
	case strings.HasSuffix(r.URL.Path, "/resume"):
		key := r.Header.Get("Idempotency-Key")
		if id, seen := h.ResumeKeys[key]; seen {
			// Replay: the original result, whatever the generation is now.
			write(200, map[string]any{"protocol_version": 1, "session": h.View(h.Sessions[id]), "server_time": "2026-09-23T04:00:03Z"})
			return
		}
		var body struct {
			SessionID  string `json:"session_id"`
			Generation int64  `json:"generation"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		s := h.Sessions[body.SessionID]
		if s == nil || s.State != "active" || s.Generation != body.Generation {
			write(409, map[string]any{"error": "team session is closed or generation is stale"})
			return
		}
		h.Resumes++
		s.Generation++
		s.Suspect = false
		h.ResumeKeys[key] = s.ID
		if h.ResumeGarbage > 0 {
			h.ResumeGarbage--
			w.WriteHeader(200)
			w.Write([]byte("not json"))
			return
		}
		write(200, map[string]any{"protocol_version": 1, "session": h.View(s), "server_time": "2026-09-23T04:00:03Z"})
	case strings.HasSuffix(r.URL.Path, "/leave"):
		var body struct {
			SessionID  string `json:"session_id"`
			Generation int64  `json:"generation"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		s := h.Sessions[body.SessionID]
		if s == nil || s.State != "active" || s.Generation != body.Generation {
			write(409, map[string]any{"error": "team session is closed or generation is stale"})
			return
		}
		s.State = "left"
		write(200, map[string]any{"protocol_version": 1, "session": h.View(s), "server_time": "2026-09-23T04:00:06Z"})
	case strings.HasSuffix(r.URL.Path, "/heartbeat"):
		if h.Heartbeat != 0 {
			write(h.Heartbeat, map[string]any{"error": "current team enrollment and bound ordinary write session required"})
			return
		}
		var hb struct {
			Availability string `json:"availability"`
		}
		json.NewDecoder(r.Body).Decode(&hb)
		h.HeartbeatAvailability = append(h.HeartbeatAvailability, hb.Availability)
		write(200, map[string]any{"protocol_version": 1, "session": map[string]any{"id": "x"}})
	case strings.HasSuffix(r.URL.Path, "/assignments/reserved"):
		if h.Reserved == nil {
			write(404, map[string]any{"error": "no reserved attempt"})
			return
		}
		write(200, map[string]any{"protocol_version": 1, "assignment": h.Reserved, "workflow_ready": true})
	case strings.Contains(r.URL.Path, "/assignments/"):
		if h.Reserved == nil || !strings.HasSuffix(r.URL.Path, "/assignments/"+fmt.Sprint(h.Reserved["id"])) {
			write(404, map[string]any{"error": "no such attempt"})
			return
		}
		full := map[string]any{"requirements": map[string]any{"title": "Fix the parser"}, "updated_at": "2026-09-23T04:00:05Z"}
		for k, v := range h.Reserved {
			full[k] = v
		}
		write(200, map[string]any{"protocol_version": 1, "assignment": full, "workflow_ready": true})
	case strings.HasSuffix(r.URL.Path, "/inbox") && h.InboxCode != 0:
		write(h.InboxCode, map[string]any{"error": "inbox unavailable"})
	case strings.HasSuffix(r.URL.Path, "/inbox"):
		msgs := []map[string]any{}
		for i := 0; i < h.InboxN; i++ {
			m := map[string]any{"id": fmt.Sprintf("m-%d", i), "sequence": i + 1, "kind": "question", "profile": map[string]any{"label": "coordinator"}, "payload": map[string]any{"text": fmt.Sprintf("Question %d: which test covers the parser change? %s", i, strings.Repeat("more ", 40))}}
			if i == 0 {
				m = map[string]any{"id": "m-0", "sequence": 1, "kind": "lifecycle", "lifecycle": map[string]any{"operation": "team.assignment.offer", "attempt_id": "att-1", "task_id": "task-1", "state": "OFFERED"}, "payload": map[string]any{"text": ""}}
			}
			msgs = append(msgs, m)
		}
		write(200, map[string]any{"protocol_version": 1, "messages": msgs, "next_cursor": h.InboxN, "has_more": false, "server_time": "2026-09-23T04:00:04Z"})
	default:
		write(404, map[string]any{"error": "unexpected " + r.URL.Path})
	}
}

func (h *Hub) Count(suffix string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.Requests {
		if strings.HasSuffix(r, suffix) {
			n++
		}
	}
	return n
}

// Checkout binds a temporary checkout to the fake hub with a
// project-local credential and returns (checkout, state root).
func Checkout(t *testing.T, h *Hub, ts *httptest.Server) (string, string) {
	t.Helper()
	root, repo := t.TempDir(), t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	// The wiring step reads and writes user-level locations; keep them in
	// the test's own home on every platform.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, Token: "checkpoint"}}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".aimem.json"), []byte(`{"project":"alpha"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := taskcred.Set(context.Background(), repo, root, h.Secret); err != nil {
		t.Fatal(err)
	}
	CacheProcess(t, root, Selection)
	return repo, root
}
