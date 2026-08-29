package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/adapter"
)

// Strict clients (OpenCode) validate the tools list as JSON Schema and
// reject the whole thing over one invalid field. "required": null (a nil
// slice marshaled) took the aimem server down for every tool.
func TestToolDefsAreValidSchema(t *testing.T) {
	b, err := json.Marshal(toolDefs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"required":null`) {
		t.Fatal(`toolDefs contain "required": null — omit the field instead`)
	}
	for _, td := range toolDefs {
		schema, ok := td["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("tool %v has no inputSchema object", td["name"])
		}
		if req, present := schema["required"]; present {
			if _, ok := req.([]string); !ok {
				t.Fatalf("tool %v: required must be a string slice, got %T", td["name"], req)
			}
		}
	}
}

func TestUpdateDocRewritesBoundFile(t *testing.T) {
	// DESIGN-shared-docs §4b: a successful update_doc on a bound doc is a
	// push followed by a pull — the local file and the sidecar rev/hash
	// must match the write, or the next checkpoint's hash-publish fights
	// the agent's own edit with a spurious conflict.
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/projects/{p}/docs/{name}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Body    string `json:"body"`
			BaseRev int64  `json:"base_rev"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		json.NewEncoder(w).Encode(map[string]any{"rev": body.BaseRev + 1})
	})
	hub := httptest.NewServer(mux)
	defer hub.Close()

	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{
		"home": {URL: hub.URL, Token: "t"},
	}, "home"); err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	os.WriteFile(filepath.Join(proj, ".aimem.json"), []byte(`{"docs":["docs/RUNBOOK.md"]}`), 0o600)
	os.WriteFile(filepath.Join(proj, "docs", "RUNBOOK.md"), []byte("old\n"), 0o644)
	t.Chdir(proj)

	s := &srv{project: "projX"}
	var p toolParams
	p.Name = "update_doc"
	p.Arguments.Name = "RUNBOOK"
	p.Arguments.Body = "new body\n"
	p.Arguments.BaseRev = 3
	msg, err := s.docTool(&p)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(proj, "docs", "RUNBOOK.md"))
	if string(got) != "new body\n" {
		t.Fatalf("bound file not rewritten: %q (msg %q)", got, msg)
	}
	c := adapter.NewClient(root)
	if rev := c.DocSyncRev("projX", "RUNBOOK"); rev != 4 {
		t.Fatalf("sidecar rev = %d, want 4", rev)
	}
	if c.DocSyncHash("projX", "RUNBOOK") != adapter.DocBodyHash([]byte("new body\n")) {
		t.Fatal("sidecar hash not recorded")
	}
	// An unbound doc name leaves the filesystem alone.
	p.Arguments.Name = "ELSEWHERE"
	if _, err := s.docTool(&p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(proj, "docs", "ELSEWHERE.md")); !os.IsNotExist(err) {
		t.Fatal("unbound doc conjured a local file")
	}
}

func TestUpdateDocRefusesSessionState(t *testing.T) {
	// The handoff is file-only by ruling: update_doc must refuse it and
	// point at the file flow, even with a hub configured.
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("SESSION-STATE refusal must happen before any hub call")
	}))
	defer hub.Close()
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{
		"home": {URL: hub.URL, Token: "t"},
	}, "home"); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	s := &srv{project: "projX"}
	var p toolParams
	p.Name = "update_doc"
	p.Arguments.Name = "SESSION-STATE"
	p.Arguments.Body = "x"
	if _, err := s.docTool(&p); err == nil ||
		!strings.Contains(err.Error(), "file-only") {
		t.Fatalf("SESSION-STATE not refused: %v", err)
	}
}

func TestUpdateDocConflictHandsBackBothSides(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/projects/{p}/docs/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{
			"error": "stale base_rev", "rev": 9, "updated_by": "minis/opencode",
			"body": "the hub's current text",
		})
	})
	hub := httptest.NewServer(mux)
	defer hub.Close()
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{
		"home": {URL: hub.URL, Token: "t"},
	}, "home"); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	s := &srv{project: "projX"}
	var p toolParams
	p.Name = "update_doc"
	p.Arguments.Name = "RUNBOOK"
	p.Arguments.Body = "mine"
	p.Arguments.BaseRev = 3
	_, err := s.docTool(&p)
	if err == nil {
		t.Fatal("conflict swallowed")
	}
	// The error is the agent's merge input: new rev, the other writer,
	// and the current body must all be present.
	for _, want := range []string{"CONFLICT", "rev 9", "minis/opencode", "the hub's current text", "base_rev 9"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("conflict message missing %q: %v", want, err)
		}
	}
}
