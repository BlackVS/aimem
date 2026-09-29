package mcp

// Task 19d8: the pilot's knowledge tools for scoped callers. On the hub's
// /mcp an ordinary token reads under its own grant, never the trusted local
// client; in a team conversation they reach the hub with the pinned context.
// Any scope but the project is refused before any hub call.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seed writes knowledge as the hub admin, over the real gate.
func (f *hubFixture) seed(t *testing.T, method, path, body string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+f.env)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("seed %s %s: %d %s", method, path, w.Code, w.Body)
	}
}

func TestHubMCPKnowledgeReadsUseTheCallersGrant(t *testing.T) {
	f := newHub(t)
	for _, p := range []string{"alpha", "beta"} {
		f.seed(t, "PUT", "/v1/projects/"+p+"/docs/RUNBOOK", `{"body":"the `+p+` runbook","base_rev":0,"updated_by":"admin"}`)
		f.seed(t, "POST", "/v1/projects/"+p+"/memories", `{"text":"the `+p+` convention is recorded","actor":"admin"}`)
	}
	call := func(token, name string, args map[string]any) (string, bool) {
		return toolText(f.rpc(t, token, "tools/call", map[string]any{"name": name, "arguments": args}))
	}
	// Alice's project-scoped token with a grant on alpha.
	for name, want := range map[string]string{"recall_memory": "the alpha convention", "list_docs": "RUNBOOK  rev 1", "read_doc": "the alpha runbook"} {
		args := map[string]any{"project": "alpha", "query": "convention", "name": "RUNBOOK"}
		if text, isErr := call(f.alice, name, args); isErr || !strings.Contains(text, want) {
			t.Errorf("%s alpha: %v %q", name, isErr, text)
		}
		args["scope"] = "project"
		if text, isErr := call(f.alice, name, args); isErr || !strings.Contains(text, want) {
			t.Errorf("%s alpha, scope project: %v %q", name, isErr, text)
		}
		// Beta is not granted; the refusal names no content.
		args["project"] = "beta"
		if text, isErr := call(f.alice, name, args); !isErr || strings.Contains(text, "beta") && strings.Contains(text, "runbook") {
			t.Errorf("%s beta: %v %q", name, isErr, text)
		}
		// Another scope is refused by the tool.
		for _, scope := range []string{"user", "both", "group:x"} {
			args := map[string]any{"project": "alpha", "query": "convention", "name": "RUNBOOK", "scope": scope}
			if text, isErr := call(f.alice, name, args); !isErr || !strings.Contains(text, "project-scoped") {
				t.Errorf("%s scope %s: %v %q", name, scope, isErr, text)
			}
		}
		// A read-only token and a user without grants read nothing.
		for _, token := range []string{f.reader, f.stranger} {
			args := map[string]any{"project": "alpha", "query": "convention", "name": "RUNBOOK"}
			if text, isErr := call(token, name, args); !isErr || strings.Contains(text, "alpha") && !strings.Contains(text, "error") {
				t.Errorf("%s without a grant: %v %q", name, isErr, text)
			}
		}
	}
	if text, isErr := call(f.alice, "read_doc", map[string]any{"name": "RUNBOOK"}); !isErr || !strings.Contains(text, "project argument is required") {
		t.Errorf("no project on the hub: %v %q", isErr, text)
	}
}

func TestTeamModeKnowledgeReadsReachTheHubWithTheContext(t *testing.T) {
	h := newTeamHub(t)
	h.custom = func(w http.ResponseWriter, r *http.Request, session string) bool {
		switch r.URL.Path {
		case "/v1/projects/alpha/memories/recall":
			if r.URL.Query().Get("q") != "convention" {
				return false
			}
			json.NewEncoder(w).Encode(map[string]any{"memories": []map[string]any{{"id": "m1", "text": "the alpha convention",
				"kind": "convention", "confidence": 0.9, "corroboration": 2, "created_at": "2026-09-01T00:00:00Z"}}})
		case "/v1/projects/alpha/docs":
			json.NewEncoder(w).Encode(map[string]any{"docs": []map[string]any{{"name": "RUNBOOK", "rev": 3, "updated_at": "2026-09-02", "updated_by": "admin"}}})
		case "/v1/projects/alpha/docs/RUNBOOK":
			if r.URL.RawQuery != "" {
				return false
			}
			json.NewEncoder(w).Encode(map[string]any{"name": "RUNBOOK", "rev": 3, "updated_at": "2026-09-02", "updated_by": "admin", "body": "the alpha runbook"})
		case "/v1/projects/beta/docs":
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"code":"grant_denied","message":"The team has no grant on this project.","active_mode":"team","retryable":false,"next_action":"Request an authorized grant change for the team.","correlation_id":"c-2"}`))
		default:
			return false
		}
		return true
	}
	root := teamRoot(t, h, nil)
	h.addSession(teamHandle('A'), "sess-1")
	s := newTeamSrv(writeSession(t, root, h.ts.URL, "sess-1", teamHandle('A')), root, "alpha")

	// Another scope is refused before anything reaches the hub.
	for _, name := range []string{"recall_memory", "list_docs", "read_doc"} {
		for _, scope := range []string{"user", "both", "group:x"} {
			if text, isErr := teamCall(t, s, name, map[string]any{"query": "x", "name": "RUNBOOK", "scope": scope}); !isErr || !strings.Contains(text, "project-scoped") {
				t.Errorf("%s scope %s: %v %s", name, scope, isErr, text)
			}
		}
	}
	if len(h.requests()) != 0 {
		t.Fatal("a refused scope reached the hub")
	}
	for name, want := range map[string]string{"recall_memory": "the alpha convention", "list_docs": "RUNBOOK  rev 3", "read_doc": "the alpha runbook"} {
		if text, isErr := teamCall(t, s, name, map[string]any{"query": "convention", "name": "RUNBOOK"}); isErr || !strings.Contains(text, want) {
			t.Errorf("%s: %v %s", name, isErr, text)
		}
	}
	// Every call carried the pinned handle; the first verified the context.
	reqs := h.requests()
	if len(reqs) == 0 || reqs[0].path != "/v1/access/identity" {
		t.Fatalf("the context was not verified first: %+v", reqs)
	}
	for _, r := range reqs {
		if r.handle != teamHandle('A') {
			t.Fatalf("a call without the pinned handle: %+v", r)
		}
	}
	// A hub refusal passes through with its code, and the next tool
	// verifies the context again.
	if text, isErr := teamCall(t, s, "list_docs", map[string]any{"project": "beta"}); !isErr || !strings.Contains(text, "grant_denied") {
		t.Fatalf("a refused project: %v %s", isErr, text)
	}
	n := len(h.requests())
	teamCall(t, s, "list_docs", map[string]any{})
	if after := h.requests(); len(after) < n+2 || after[n].path != "/v1/access/identity" {
		t.Fatalf("no re-verification after a refusal: %+v", after[n:])
	}
}
