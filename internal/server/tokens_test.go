package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func authedReq(t *testing.T, h http.Handler, method, path, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestNamedTokensRolesAndStamping(t *testing.T) {
	s, reg := testServer(t)
	// A writer and an admin in tokens.json, plus the env token.
	wSecret, wDigest, err := NewTokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	aSecret, aDigest, _ := NewTokenSecret()
	if err := SaveTokens(reg.Root(), []TokenEntry{
		{Name: "dmbunker", Role: "writer", SHA256: wDigest},
		{Name: "ops", Role: "admin", SHA256: aDigest},
	}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", s.Handler())
	authed := s.authWrapper("envsekrit", mux)

	// Writer: normal surface yes, admin surface 403 with a message that
	// names the token and role.
	if w := authedReq(t, authed, "GET", "/v1/projects", wSecret, ""); w.Code != 200 {
		t.Fatalf("writer on writer route: %d", w.Code)
	}
	if w := authedReq(t, authed, "GET", "/v1/logs", wSecret, ""); w.Code != 403 ||
		!strings.Contains(w.Body.String(), "dmbunker") {
		t.Fatalf("writer on admin route: %d %s", w.Code, w.Body)
	}
	// Admin and env tokens pass the admin surface.
	for _, tok := range []string{aSecret, "envsekrit"} {
		if w := authedReq(t, authed, "GET", "/v1/logs", tok, ""); w.Code != 200 {
			t.Fatalf("admin route with %q-class token: %d", tok[:3], w.Code)
		}
	}
	// Garbage and empty are refused.
	if w := authedReq(t, authed, "GET", "/v1/projects", "nope", ""); w.Code != 401 {
		t.Fatalf("bad token accepted: %d", w.Code)
	}
	if w := authedReq(t, authed, "GET", "/v1/projects", "", ""); w.Code != 401 {
		t.Fatalf("missing token accepted: %d", w.Code)
	}

	// Doc writes by a NAMED token are stamped with its name; the env
	// token keeps the legacy label untouched.
	put := `{"body":"# doc","base_rev":0,"updated_by":"host/cli"}`
	if w := authedReq(t, authed, "PUT", "/v1/projects/proj-t/docs/NOTE", wSecret, put); w.Code != 200 {
		t.Fatalf("doc put: %d %s", w.Code, w.Body)
	}
	db, _ := reg.OpenExisting("proj-t")
	doc, err := db.GetDoc("NOTE", 0)
	if err != nil || doc.UpdatedBy != "dmbunker/host/cli" {
		t.Fatalf("writer not stamped: %+v err=%v", doc, err)
	}
	put2 := `{"body":"# doc v2","base_rev":1,"updated_by":"host/cli"}`
	if w := authedReq(t, authed, "PUT", "/v1/projects/proj-t/docs/NOTE", "envsekrit", put2); w.Code != 200 {
		t.Fatalf("doc put env: %d %s", w.Code, w.Body)
	}
	if doc, _ := db.GetDoc("NOTE", 0); doc.UpdatedBy != "host/cli" {
		t.Fatalf("env token should not stamp: %+v", doc)
	}
}

func TestTokenRegistryRoundTrip(t *testing.T) {
	root := t.TempDir()
	if got := LoadTokens(root); got != nil {
		t.Fatalf("missing file should be empty registry: %v", got)
	}
	secret, digest, err := NewTokenSecret()
	if err != nil || len(secret) != 64 || len(digest) != 64 {
		t.Fatalf("secret shape: %d/%d err=%v", len(secret), len(digest), err)
	}
	if HashToken(secret) != digest {
		t.Fatal("digest mismatch")
	}
	if err := SaveTokens(root, []TokenEntry{{Name: "a", Role: "writer", SHA256: digest}}); err != nil {
		t.Fatal(err)
	}
	got := LoadTokens(root)
	if len(got) != 1 || got[0].Name != "a" || got[0].SHA256 != digest {
		t.Fatalf("roundtrip: %+v", got)
	}
}
