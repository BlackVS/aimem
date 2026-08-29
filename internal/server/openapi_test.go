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
	public := map[string]bool{"/": true, "/admin": true, "/v1/status": true}

	s, _ := testServer(t)
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
