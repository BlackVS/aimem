package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/hubtest"
)

const (
	a9Writer = "aimem_writer_CHECKPOINT"
	a9Task   = "aimem_user_TASK"
)

// a9Hub is a fake hub that records every request's route and credential and
// answers the identity check with identity.
type a9Hub struct {
	*httptest.Server
	mu       sync.Mutex
	seen     []string // "METHOD path bearer"
	identity http.HandlerFunc
}

func (h *a9Hub) saw() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.seen...)
}

// sentOn reports whether bearer reached any docs or collections route.
func (h *a9Hub) sentOn(bearer string) []string {
	var out []string
	for _, s := range h.saw() {
		if strings.HasSuffix(s, " "+bearer) && (strings.Contains(s, "/docs") || strings.Contains(s, "/collections")) {
			out = append(out, s)
		}
	}
	return out
}

func newA9Hub(t *testing.T, identity http.HandlerFunc) *a9Hub {
	h := &a9Hub{identity: identity}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/access/identity", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+a9Task {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.identity(w, r)
	})
	mux.HandleFunc("GET /v1/projects/{p}/docs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"docs": []map[string]any{{"name": "RUNBOOK-" + r.PathValue("p"), "rev": 1}}})
	})
	mux.HandleFunc("GET /v1/projects/{p}/docs/{name}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"name": r.PathValue("name"), "rev": 2, "body": "the body"})
	})
	mux.HandleFunc("PUT /v1/projects/{p}/docs/{name}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"rev": 3})
	})
	mux.HandleFunc("GET /v1/projects/{p}/collections/{c}/records", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"records": []map[string]any{{"id": "r1", "rev": 1}}})
	})
	mux.HandleFunc("GET /v1/projects/{p}/collections/{c}/records/{id...}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id"), "rev": 1, "body": json.RawMessage(`{"a":1}`)})
	})
	mux.HandleFunc("PUT /v1/projects/{p}/collections/{c}/records/{id...}", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id"), "rev": 2})
	})
	h.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, r.Method+" "+r.URL.Path+" "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		h.mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(h.Close)
	return h
}

// a9Checkout makes a state root whose default hub entry "hubA" holds the
// checkpoint token a9Writer and, when task is set, the task token a9Task,
// and a checkout pinning projQ with group fw declared, as cwd.
func a9Checkout(t *testing.T, hub *a9Hub, task bool, extra string) (root, dir string) {
	root = t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	entry := &adapter.HubConfig{URL: hub.URL, Pin: hubtest.Pin(hub.Server), Token: a9Writer}
	if task {
		entry.TaskToken = a9Task
	}
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hubA": entry}, "hubA"); err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(t.TempDir(), "checkout")
	os.MkdirAll(dir, 0o755)
	cfg := `{"project":"projQ","groups":["fw"],"collections":[{"name":"api","scope":"group:fw"}]` + extra + `}`
	if err := os.WriteFile(filepath.Join(dir, ".aimem.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	return root, dir
}

func a9Call(name, scope string) *toolParams {
	var p toolParams
	p.Name = name
	p.Arguments.Scope = scope
	switch name {
	case "read_doc", "update_doc":
		p.Arguments.Name = "RUNBOOK"
		p.Arguments.Body = "new body\n"
		p.Arguments.BaseRev = 2
	case "list_records", "get_record", "put_record":
		p.Arguments.Collection = "notes"
		p.Arguments.ID = "r1"
		p.Arguments.Body = `{"a":2}`
		p.Arguments.BaseRev = 1
	}
	return &p
}

func a9Run(p *toolParams) (string, error) {
	s := &srv{project: "projQ", groups: []string{"group-fw"}}
	if strings.HasSuffix(p.Name, "_doc") || p.Name == "list_docs" {
		return s.docTool(p)
	}
	return s.colTool(p)
}

var a9Tools = []string{"list_docs", "read_doc", "update_doc", "list_records", "get_record", "put_record"}

func a9Identity(write bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("project") != "projQ" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"user_id": "u1", "token_id": "t1", "name": "member", "role": "user",
			"scope": "user", "project": "projQ", "task_write": write, "label": "laptop"})
	}
}

// A task credential that may not read the pinned project stops every tool
// before the checkpoint credential is sent, group scopes included.
func TestA9RefusedProjectSendsNoCheckpointCredential(t *testing.T) {
	for _, tool := range a9Tools {
		for _, scope := range []string{"", "group:fw"} {
			t.Run(tool+"/"+scope, func(t *testing.T) {
				hub := newA9Hub(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
				a9Checkout(t, hub, true, "")
				_, err := a9Run(a9Call(tool, scope))
				if err == nil || !strings.Contains(err.Error(), "may not read this project") || !strings.Contains(err.Error(), `hub "hubA"`) {
					t.Fatalf("not refused, or the refusal names no project and hub: %v", err)
				}
				if got := hub.sentOn(a9Writer); len(got) > 0 {
					t.Fatalf("the checkpoint credential reached the hub: %v", got)
				}
			})
		}
	}
}

// An identity answer that is neither a 404 nor a grant confirms nothing: a
// server error, a refused credential, a body that cannot be read, an answer
// naming no user, a timeout.
func TestA9UnclearIdentityAnswerIsNotConfirmed(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"server error": func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) },
		"forbidden":    func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusForbidden) },
		"not json":     func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>proxy</html>")) },
		"no user": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"name":"ci","role":"writer","project":"projQ"}`))
		},
		"empty object":  func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) },
		"redirect away": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) },
		"timeout": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		},
	}
	for name, identity := range cases {
		t.Run(name, func(t *testing.T) {
			if name == "timeout" {
				old := taskCheckTimeout
				taskCheckTimeout = 300 * time.Millisecond
				t.Cleanup(func() { taskCheckTimeout = old })
			}
			hub := newA9Hub(t, identity)
			a9Checkout(t, hub, true, "")
			for _, tool := range a9Tools {
				msg, err := a9Run(a9Call(tool, ""))
				if err == nil || !strings.Contains(err.Error(), "refused") {
					t.Fatalf("%s: proceeded on an unclear identity answer: %q %v", tool, msg, err)
				}
			}
			if got := hub.sentOn(a9Writer); len(got) > 0 {
				t.Fatalf("the checkpoint credential reached the hub: %v", got)
			}
			if got := hub.sentOn(a9Task); len(got) > 0 {
				t.Fatalf("the task credential reached a docs route: %v", got)
			}
		})
	}
}

// A confirmed project: the document list and read go out with the task
// credential, the rest with the checkpoint credential, and each result
// names the hub, the credential and the confirmation.
func TestA9ConfirmedProjectNamesHubAndCredential(t *testing.T) {
	hub := newA9Hub(t, a9Identity(true))
	a9Checkout(t, hub, true, "")
	for _, tool := range a9Tools {
		msg, err := a9Run(a9Call(tool, ""))
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if !strings.Contains(msg, `hub "hubA" (`+hub.URL+`)`) || !strings.Contains(msg, "project projQ, confirmed by your task credential") {
			t.Fatalf("%s: the result does not name the hub and the confirmation: %q", tool, msg)
		}
		wantTask := tool == "list_docs" || tool == "read_doc"
		if got := strings.Contains(msg, `your task credential "laptop"`); got != wantTask {
			t.Fatalf("%s: the result names the wrong credential: %q", tool, msg)
		}
	}
	for _, s := range hub.saw() {
		isRead := strings.HasPrefix(s, "GET ") && strings.Contains(s, "/projects/projQ/docs")
		switch {
		case isRead && !strings.HasSuffix(s, " "+a9Task):
			t.Fatalf("a document read went out without the task credential: %s", s)
		case !isRead && strings.Contains(s, "/projects/") && !strings.HasSuffix(s, " "+a9Writer):
			t.Fatalf("a write or record call went out without the checkpoint credential: %s", s)
		}
	}
}

// A task credential that reads but may not write the project allows the
// reads and refuses update_doc and put_record before anything is written.
func TestA9ReadOnlyGrantRefusesProjectWrites(t *testing.T) {
	hub := newA9Hub(t, a9Identity(false))
	a9Checkout(t, hub, true, "")
	for _, tool := range a9Tools {
		_, err := a9Run(a9Call(tool, ""))
		write := tool == "update_doc" || tool == "put_record"
		if write != (err != nil) {
			t.Fatalf("%s: write=%v, err=%v", tool, write, err)
		}
		if write && !strings.Contains(err.Error(), "may read this project but not write it") {
			t.Fatalf("%s: the refusal does not say why: %v", tool, err)
		}
	}
	for _, s := range hub.saw() {
		if strings.HasPrefix(s, "PUT ") {
			t.Fatalf("a write reached the hub: %s", s)
		}
	}
}

// Without a task credential on the hub entry the tools proceed with the
// checkpoint credential and every result says the scope is not confirmed.
func TestA9NoTaskCredentialProceedsUnconfirmed(t *testing.T) {
	hub := newA9Hub(t, a9Identity(true))
	a9Checkout(t, hub, false, "")
	for _, tool := range a9Tools {
		msg, err := a9Run(a9Call(tool, ""))
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if !strings.Contains(msg, "scope not confirmed") || !strings.Contains(msg, "the machine's checkpoint credential") {
			t.Fatalf("%s: the result is not marked: %q", tool, msg)
		}
	}
	for _, s := range hub.saw() {
		if strings.Contains(s, "/access/identity") {
			t.Fatalf("an identity check went out without a task credential: %s", s)
		}
	}
}

// A repository that marks itself as requiring a local task credential does
// not turn a machine that holds one into a machine without one.
func TestA9LocalMarkerDoesNotSkipTheCheck(t *testing.T) {
	hub := newA9Hub(t, a9Identity(true))
	a9Checkout(t, hub, true, `,"task_credential":"local"`)
	_, err := a9Run(a9Call("read_doc", ""))
	if err == nil || !strings.Contains(err.Error(), "task credential cannot be used") {
		t.Fatalf("the check was skipped: %v", err)
	}
	if got := hub.sentOn(a9Writer); len(got) > 0 {
		t.Fatalf("the checkpoint credential reached the hub: %v", got)
	}
}

// A group scope is labelled as declared by the repository, whether or not
// the pinned project was confirmed, and goes out with the checkpoint
// credential (no task credential reads a group space).
func TestA9GroupScopeIsDeclaredNotConfirmed(t *testing.T) {
	for _, task := range []bool{true, false} {
		hub := newA9Hub(t, a9Identity(true))
		a9Checkout(t, hub, task, "")
		for _, tool := range []string{"read_doc", "list_records"} {
			msg, err := a9Run(a9Call(tool, "group:fw"))
			if err != nil {
				t.Fatalf("task=%v %s: %v", task, tool, err)
			}
			if !strings.Contains(msg, "group space group-fw as declared by this repository's .aimem.json, not confirmed") {
				t.Fatalf("task=%v %s: the group is not labelled: %q", task, tool, msg)
			}
		}
		if got := hub.sentOn(a9Task); len(got) > 0 {
			t.Fatalf("task=%v: the task credential was sent to a group space: %v", task, got)
		}
	}
	// The default listing reads the declared groups with the checkpoint
	// credential beside the project read with the task credential.
	hub := newA9Hub(t, a9Identity(true))
	a9Checkout(t, hub, true, "")
	msg, err := a9Run(a9Call("list_docs", ""))
	if err != nil || !strings.Contains(msg, "RUNBOOK-group-fw") || !strings.Contains(msg, "group spaces: the machine's checkpoint credential") {
		t.Fatalf("the default listing: %q %v", msg, err)
	}
}

// The first use from a checkout is announced with the hub, project and
// groups; the same scope again is not; a change of any of them is.
func TestA9FirstUseAndChangeAreAnnounced(t *testing.T) {
	hub := newA9Hub(t, a9Identity(true))
	_, dir := a9Checkout(t, hub, false, "")
	first, err := a9Run(a9Call("list_docs", ""))
	if err != nil || !strings.HasPrefix(first, "[aimem: first use of shared docs and records from this checkout") ||
		!strings.Contains(first, `hub "hubA" (`+hub.URL+`), project projQ, groups group-fw`) {
		t.Fatalf("the first use is not announced: %q %v", first, err)
	}
	again, err := a9Run(a9Call("read_doc", ""))
	if err != nil || strings.Contains(again, "first use") || strings.Contains(again, "CHANGED") {
		t.Fatalf("an unchanged scope was announced again: %q %v", again, err)
	}
	s := &srv{project: "projQ", groups: []string{"group-fw", "group-ops"}}
	changed, err := s.docTool(a9Call("list_docs", ""))
	if err != nil || !strings.Contains(changed, "scope CHANGED") || !strings.Contains(changed, "groups group-fw, group-ops; was") {
		t.Fatalf("a changed group list is not announced: %q %v", changed, err)
	}
	// The announcement is per checkout: another checkout announces its own.
	other := filepath.Join(filepath.Dir(dir), "other")
	os.MkdirAll(other, 0o755)
	os.WriteFile(filepath.Join(other, ".aimem.json"), []byte(`{"project":"projQ","groups":["fw"]}`), 0o600)
	t.Chdir(other)
	if msg, err := a9Run(a9Call("list_docs", "")); err != nil || !strings.Contains(msg, "first use") {
		t.Fatalf("another checkout is not announced: %q %v", msg, err)
	}
}
