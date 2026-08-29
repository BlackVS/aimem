package server

// The OpenAPI spec (DESIGN-hub-sync): hand-written JSON, embedded, and
// served bearer-gated — the three-route unauthenticated surface is an
// invariant, and the file is in the public repo for anyone without a
// token. What keeps it truthful is openapi_test.go: every route in the
// table must appear in the spec with the right role, and vice versa,
// so drift is a CI failure rather than a slow lie.

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.json
var openAPISpec []byte

func (s *Server) openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(openAPISpec)
}
