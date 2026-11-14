package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"aimem/internal/uuidv7"
)

// Task reads outside team mode need a grant on the project (audit A-10).
// Bob holds a read-only token and a grant on alpha, none on beta: he reads
// alpha; every read of beta answers exactly as a project or task that does
// not exist, and the hub's log records the refusal with his user and the
// project, never task content. A revoked grant refuses the next read; a
// restored one allows it.
func TestTaskReadsNeedAGrant(t *testing.T) {
	f := newTaskFixture(t)
	var logs bytes.Buffer
	f.s.log = slog.New(slog.NewJSONHandler(&logs, nil))
	create := func(path, key, body string) string {
		t.Helper()
		w := taskReq(t, f.h, "POST", path, f.admin, key, body)
		if w.Code != 201 {
			t.Fatalf("create %s: %d %s", path, w.Code, w.Body)
		}
		var out struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.ID == "" {
			t.Fatalf("create %s: %s", path, w.Body)
		}
		return out.ID
	}
	alphaTask := create("/v1/projects/alpha/tasks", "a1", `{"title":"alpha work"}`)
	secret := "beta-confidential-title"
	betaTask := create("/v1/projects/beta/tasks", "b1", `{"title":"`+secret+`"}`)
	betaComment := create("/v1/tasks/"+betaTask+"/comments", "b2", `{"body":"beta comment"}`)
	betaEpic := create("/v1/projects/beta/epics", "b3", `{"title":"beta epic"}`)

	get := func(token, path string) (int, string) {
		t.Helper()
		w := taskReq(t, f.h, "GET", path, token, "", "")
		return w.Code, w.Body.String()
	}
	// Each read of beta answers as the same read of something that does
	// not exist: one status, one body.
	missing := uuidv7.New()
	for _, c := range []struct{ ungranted, unknown string }{
		{"/v1/projects/beta/tasks", "/v1/projects/nosuch/tasks"},
		{"/v1/projects/beta/tasks/counts", "/v1/projects/nosuch/tasks/counts"},
		{"/v1/projects/beta/epics", "/v1/projects/nosuch/epics"},
		{"/v1/projects/beta/epics/" + betaEpic, "/v1/projects/nosuch/epics/" + betaEpic},
		{"/v1/tasks/" + betaTask, "/v1/tasks/" + missing},
		{"/v1/tasks/" + betaTask + "/history", "/v1/tasks/" + missing + "/history"},
		{"/v1/tasks/" + betaTask + "/comments", "/v1/tasks/" + missing + "/comments"},
		{"/v1/tasks/" + betaTask + "/comments/" + betaComment, "/v1/tasks/" + missing + "/comments/" + betaComment},
		{"/v1/tasks/" + betaTask[:20], "/v1/tasks/" + missing[:20]}, // a unique prefix
	} {
		gs, gb := get(f.bob, c.ungranted)
		us, ub := get(f.bob, c.unknown)
		if gs != 404 || gs != us || gb != ub || !strings.Contains(gb, "holds no grant") || !strings.Contains(gb, "aimem access grant") {
			t.Fatalf("%s: %d %s; unknown %s: %d %s", c.ungranted, gs, gb, c.unknown, us, ub)
		}
		if strings.Contains(gb, secret) || strings.Contains(gb, betaTask) {
			t.Fatalf("%s: the refusal leaks the task: %s", c.ungranted, gb)
		}
	}
	// The admin reads beta; bob reads alpha.
	if s, b := get(f.admin, "/v1/tasks/"+betaTask); s != 200 {
		t.Fatalf("admin reads beta: %d %s", s, b)
	}
	if s, b := get(f.bob, "/v1/tasks/"+alphaTask); s != 200 {
		t.Fatalf("bob reads alpha: %d %s", s, b)
	}
	// The refusal is in the hub's log, with the user and the project and
	// without task content.
	var refused bool
	for _, line := range strings.Split(logs.String(), "\n") {
		if !strings.Contains(line, "task read refused") {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line: %v %s", err, line)
		}
		if rec["user_id"] == f.bobUser && rec["project"] == "beta" {
			refused = true
		}
		if strings.Contains(line, secret) || strings.Contains(line, "beta comment") {
			t.Fatalf("the refusal log carries task content: %s", line)
		}
	}
	if !refused {
		t.Fatalf("no refusal logged for bob on beta:\n%s", logs.String())
	}
	// Revoked: the next read is refused, the project leaves bob's list;
	// restored: both come back.
	db, err := f.s.openAccess(false)
	if err != nil {
		t.Fatal(err)
	}
	projects := func() []string {
		t.Helper()
		s, b := get(f.bob, "/v1/projects")
		var out struct {
			Projects []string `json:"projects"`
		}
		if s != 200 || json.Unmarshal([]byte(b), &out) != nil {
			t.Fatalf("project list: %d %s", s, b)
		}
		return out.Projects
	}
	if got := projects(); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("bob's projects: %v", got)
	}
	if err := db.SetGrant("admin", f.alphaInstance, "user", f.bobUser, false); err != nil {
		t.Fatal(err)
	}
	if s, b := get(f.bob, "/v1/tasks/"+alphaTask); s != 404 || !strings.Contains(b, "holds no grant") {
		t.Fatalf("read after revocation: %d %s", s, b)
	}
	if s, b := get(f.bob, "/v1/projects/alpha/tasks"); s != 404 {
		t.Fatalf("list after revocation: %d %s", s, b)
	}
	if got := projects(); len(got) != 0 {
		t.Fatalf("bob's projects after revocation: %v", got)
	}
	if err := db.SetGrant("admin", f.alphaInstance, "user", f.bobUser, true); err != nil {
		t.Fatal(err)
	}
	if s, b := get(f.bob, "/v1/tasks/"+alphaTask); s != 200 {
		t.Fatalf("read after the grant is restored: %d %s", s, b)
	}
}
