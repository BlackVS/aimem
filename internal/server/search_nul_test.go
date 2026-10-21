package server

// A NUL in the terms of every FTS route is a 400 "invalid query", refused
// before the query reaches SQLite (which ends a string at NUL).

import (
	"strings"
	"testing"
)

func TestFTSRoutesRefuseNUL(t *testing.T) {
	s, _ := testServer(t)
	h := s.Handler()
	if w := req(t, h, "POST", "/v1/projects/proj-nul/memories", `{"text":"the convention is recorded","actor":"admin"}`); w.Code != 200 {
		t.Fatalf("seed: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/v1/projects/proj-nul/search?q=a%20%00", "/v1/projects/proj-nul/memories/recall?q=a%20%00"} {
		if w := req(t, h, "GET", path, ""); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid query") {
			t.Errorf("%s: %d %s", path, w.Code, w.Body)
		}
	}
	f := newTaskFixture(t)
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks?q=a%20%00", f.bob, "", ""); w.Code != 400 || !strings.Contains(w.Body.String(), "invalid query") {
		t.Errorf("task search: %d %s", w.Code, w.Body)
	}
}
