package server

// The OpenAPI parity test (DESIGN-hub-sync): the embedded spec and the
// route table must describe the same surface with the same auth
// contract, in both directions, or CI fails — a spec that drifts is a
// slow lie, and this project has already paid for docs that drifted.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAPIMatchesRouteTable(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]struct {
			XRole string `json:"x-role"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatalf("embedded spec is not valid JSON: %v", err)
	}
	s, _ := testServer(t)
	public := map[string]bool{}
	for path := range s.publicGETs() {
		public[path] = true
	}
	seen := map[string]string{} // "METHOD path" -> expected role
	for _, rt := range s.Routes() {
		path := rt.Pattern
		if path == "/{$}" {
			path = "/"
		}
		want := "writer"
		switch {
		case rt.Admin:
			want = "admin"
		case public[path]:
			want = "public"
		}
		key := strings.ToLower(rt.Method) + " " + path
		seen[key] = want

		ops, ok := spec.Paths[path]
		if !ok {
			t.Errorf("route %s %s missing from openapi.json", rt.Method, path)
			continue
		}
		op, ok := ops[strings.ToLower(rt.Method)]
		if !ok {
			t.Errorf("method %s missing from openapi.json path %s", rt.Method, path)
			continue
		}
		if op.XRole != want {
			t.Errorf("%s %s: spec says x-role %q, route table says %q",
				rt.Method, path, op.XRole, want)
		}
	}
	// Reverse: nothing in the spec that the server does not serve.
	for path, ops := range spec.Paths {
		for method := range ops {
			if _, ok := seen[method+" "+path]; !ok {
				t.Errorf("openapi.json documents %s %s but no such route exists", method, path)
			}
		}
	}
}

// Team route descriptions state the current readiness contract; stale
// "not ready" wording would contradict the responses.
func TestOpenAPITeamDescriptionsStateReadiness(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]struct {
			Description string `json:"description"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatal(err)
	}
	stale := []string{"workflow_ready:false", "workflow_ready is false", "workflow_ready stays false", "MCP assignment tools yet", "are still deferred"}
	teamRoutes := 0
	for path, ops := range spec.Paths {
		if !strings.Contains(path, "/teams/") {
			continue
		}
		for method, op := range ops {
			teamRoutes++
			for _, phrase := range stale {
				if strings.Contains(op.Description, phrase) {
					t.Errorf("%s %s still says %q", method, path, phrase)
				}
			}
		}
	}
	if teamRoutes < 30 {
		t.Fatalf("team routes in the spec: %d", teamRoutes)
	}
}

// The two task write routes describe the typed reference shape (the
// bodies themselves are deliberately loose objects, so the description
// is the contract the spec carries).
func TestOpenAPIDescribesTypedReferences(t *testing.T) {
	var spec struct {
		Paths map[string]map[string]struct {
			Description string `json:"description"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatal(err)
	}
	for _, rt := range []struct{ path, method string }{{"/v1/projects/{p}/tasks", "post"}, {"/v1/tasks/{id}", "put"}} {
		d := spec.Paths[rt.path][rt.method].Description
		if !strings.Contains(d, "typed references {kind, ref, note?, scope?}") || !strings.Contains(d, "a bare string is refused with 400") {
			t.Errorf("%s %s does not describe typed references: %q", rt.method, rt.path, d)
		}
	}
}
