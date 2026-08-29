package server

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"aimem/internal/schema"
)

func syncReq(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestSyncEventsRoundTrip(t *testing.T) {
	s, reg := testServer(t)
	line := `{"project_id":"proj-a","groups":["group-g"],"event":{"schema_version":` +
		schemaVersionStr() + `,"idempotency_key":"sync-k1","client":"claude-code","session_id":"s1","turn_id":"t1","kind":"turn","outcome":"ok","ts":"2026-08-29T00:00:00Z","user_request":"hi"}}`
	// Push: JSONL in, idempotent per line (the second copy is a no-op).
	w := syncReq(t, s, "POST", "/v1/sync/events", line+"\n"+line+"\n")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"submitted":2`) {
		t.Fatalf("push: %d %s", w.Code, w.Body)
	}
	db, _ := reg.OpenExisting("proj-a")
	if tl, err := db.Timeline("s1", 0); err != nil || len(tl) != 1 {
		t.Fatalf("idempotent import broke: %d events, err=%v", len(tl), err)
	}
	// Groups meta rode along, like the real-time push path.
	if g, _ := db.GetMeta("groups"); g != `["group-g"]` {
		t.Fatalf("groups meta not stamped: %q", g)
	}
	// Pull: filtered stream out.
	w = syncReq(t, s, "GET", "/v1/sync/events?projects=proj-a&since=", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"sync-k1"`) {
		t.Fatalf("pull: %d %s", w.Code, w.Body)
	}
	// The filter excludes other projects entirely.
	if w := syncReq(t, s, "GET", "/v1/sync/events?projects=other", ""); strings.Contains(w.Body.String(), "sync-k1") {
		t.Fatal("filter leaked another project's events")
	}
	// A GET must never create a project (the v0.1.26 lesson).
	syncReq(t, s, "GET", "/v1/sync/events?projects=ghost-project", "")
	ids, _ := reg.Projects()
	for _, id := range ids {
		if id == "ghost-project" {
			t.Fatal("sync GET resurrected a nonexistent project")
		}
	}
}

func TestSyncMemoriesAndConfigRoundTrip(t *testing.T) {
	s, reg := testServer(t)
	mem := `{"project_id":"proj-m","memory":{"id":"m1","text":"the fact","kind":"fact","confidence":0.8,"created_at":"2026-08-29T00:00:00Z","actor":"test","tags":["chapter:a","topic"]}}`
	run := `{"project_id":"proj-m","curate_run":{"id":"r1","ts":"2026-08-29T00:00:00Z","host":"x","events_read":3,"written":1}}`
	w := syncReq(t, s, "POST", "/v1/sync/memories", mem+"\n"+run+"\n")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"imported":2`) {
		t.Fatalf("memories push: %d %s", w.Code, w.Body)
	}
	db, _ := reg.OpenExisting("proj-m")
	if mems, _ := db.Memories(false); len(mems) != 1 || mems[0].ID != "m1" {
		t.Fatalf("memory not merged: %+v", mems)
	}
	w = syncReq(t, s, "GET", "/v1/sync/memories?projects=proj-m", "")
	if !strings.Contains(w.Body.String(), `"m1"`) || !strings.Contains(w.Body.String(), `"curate_run"`) {
		t.Fatalf("memories pull missing rows: %s", w.Body)
	}

	// Group config: fill-only in, echoed out.
	cfg := `{"project":"group-g","key":"about","value":"shared infra"}`
	if w := syncReq(t, s, "POST", "/v1/sync/group-config", cfg+"\n"); w.Code != 200 ||
		!strings.Contains(w.Body.String(), `"applied":1`) {
		t.Fatalf("config push: %d %s", w.Code, w.Body)
	}
	// Divergent value keeps local (fill-only) — applied stays 0.
	cfg2 := `{"project":"group-g","key":"about","value":"OTHER"}`
	if w := syncReq(t, s, "POST", "/v1/sync/group-config", cfg2+"\n"); !strings.Contains(w.Body.String(), `"applied":0`) {
		t.Fatalf("fill-only overwrote: %s", w.Body)
	}
	gdb, _ := reg.OpenExisting("group-g")
	if v, _ := gdb.GetMeta("about"); v != "shared infra" {
		t.Fatalf("config diverged: %q", v)
	}
	w = syncReq(t, s, "GET", "/v1/sync/group-config?projects=group-g", "")
	if !strings.Contains(w.Body.String(), "shared infra") {
		t.Fatalf("config pull: %s", w.Body)
	}
}

func schemaVersionStr() string { return strconv.Itoa(schema.Version) }
