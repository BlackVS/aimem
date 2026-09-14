package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"aimem/internal/store"
	"aimem/internal/uuidv7"
)

// taskReq performs one request through the REAL bearer gate with an
// optional Idempotency-Key.
func taskReq(t *testing.T, h http.Handler, method, path, token, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func decodeTask(t *testing.T, w *httptest.ResponseRecorder) taskResponse {
	t.Helper()
	var out taskResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode task: %v (%s)", err, w.Body)
	}
	return out
}

// taskFixture is a hub with two ordinary projects, legacy writer/admin
// tokens, and two ordinary users: alice holds a grant and a project token
// on alpha; bob holds a cross-project read-only token.
type taskFixture struct {
	s                                *Server
	reg                              *store.Registry
	h                                http.Handler
	env, admin, writer, alice, bob   string
	aliceUser, bobUser, aliceTokenID string
	alphaInstance                    string
}

func newTaskFixture(t *testing.T) *taskFixture {
	t.Helper()
	s, reg := testServer(t)
	for _, p := range []string{"alpha", "beta"} {
		db, err := reg.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetMeta(store.TasksMetaKey, "on"); err != nil { // the admin's decision, made here
			t.Fatal(err)
		}
	}
	writer, wd, _ := NewTokenSecret()
	admin, ad, _ := NewTokenSecret()
	if err := SaveTokens(reg.Root(), []TokenEntry{{Name: "old-writer", Role: "writer", SHA256: wd}, {Name: "host-admin", Role: "admin", SHA256: ad}}); err != nil {
		t.Fatal(err)
	}
	db, err := s.openAccess(true)
	if err != nil {
		t.Fatal(err)
	}
	alice, err := db.CreateUser("admin", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := db.CreateUser("admin", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	instance, err := reg.ProjectAccessID("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetGrant("admin", instance, "user", alice.ID, true); err != nil {
		t.Fatal(err)
	}
	aliceTok, aliceSecret, err := db.Issue("admin", alice.ID, "agent", instance, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	_, bobSecret, err := db.Issue("admin", bob.ID, "reader", "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	f := &taskFixture{s: s, reg: reg, env: "env-admin-secret", admin: admin, writer: writer, alice: aliceSecret, bob: bobSecret,
		aliceUser: alice.ID, bobUser: bob.ID, aliceTokenID: aliceTok.ID, alphaInstance: instance}
	f.h = s.TCPHandler(f.env, nil)
	return f
}

const taskBody = `{"title":"ship it","objective":"green CI","state":"READY","next_action":"open PR"}`

func TestTaskRoutesAuthorization(t *testing.T) {
	f := newTaskFixture(t)
	h := f.h

	// No credential / bad credential: 401 before anything else.
	if w := taskReq(t, h, "POST", "/v1/projects/alpha/tasks", "", "k1", taskBody); w.Code != 401 {
		t.Fatalf("anonymous create: %d", w.Code)
	}
	if w := taskReq(t, h, "GET", "/v1/projects/alpha/tasks", "aimem_user_nope", "", ""); w.Code != 401 {
		t.Fatalf("bad token list: %d", w.Code)
	}
	// Alice writes her own project; the key is required and replays.
	w := taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "", taskBody)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Idempotency-Key") {
		t.Fatalf("create without key: %d %s", w.Code, w.Body)
	}
	w = taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "k1", taskBody)
	if w.Code != 201 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("alice create: %d %s %s", w.Code, w.Header().Get("Content-Type"), w.Body)
	}
	task := decodeTask(t, w)
	if task.Project != "alpha" || task.Revision != 1 || task.Links.Self != "/v1/tasks/"+task.ID || task.Links.Comments != "/v1/tasks/"+task.ID+"/comments" {
		t.Fatalf("task view: %+v", task)
	}
	if again := decodeTask(t, taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "k1", taskBody)); again.ID != task.ID {
		t.Fatalf("replay must return the original task: %s vs %s", again.ID, task.ID)
	}
	if w := taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "k1", `{"title":"different"}`); w.Code != 409 {
		t.Fatalf("key reuse with different input: %d %s", w.Code, w.Body)
	}
	// History stamps her stable ids, never her display name as identity.
	var hist struct {
		Changes []store.TaskChange `json:"changes"`
	}
	w = taskReq(t, h, "GET", "/v1/tasks/"+task.ID+"/history", f.bob, "", "")
	if w.Code != 200 {
		t.Fatalf("bob reads history: %d %s", w.Code, w.Body)
	}
	json.Unmarshal(w.Body.Bytes(), &hist)
	if len(hist.Changes) != 1 || hist.Changes[0].Actor.Kind != "user" || hist.Changes[0].Actor.UserID != f.aliceUser || hist.Changes[0].Actor.TokenID != f.aliceTokenID {
		t.Fatalf("actor stamp: %+v", hist.Changes)
	}

	// Alice's token is issued for alpha only: beta is a foreign project.
	if w := taskReq(t, h, "POST", "/v1/projects/beta/tasks", f.alice, "k2", taskBody); w.Code != 403 {
		t.Fatalf("alice writes beta: %d %s", w.Code, w.Body)
	}
	// Bob (read-only, cross-project) reads everything, writes nothing.
	if w := taskReq(t, h, "GET", "/v1/projects/alpha/tasks", f.bob, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), task.ID) {
		t.Fatalf("bob lists alpha: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "GET", "/v1/tasks/"+task.ID, f.bob, "", ""); w.Code != 200 {
		t.Fatalf("bob reads task: %d", w.Code)
	}
	if w := taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.bob, "k3", taskBody); w.Code != 403 {
		t.Fatalf("bob creates: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "POST", "/v1/tasks/"+task.ID+"/comments", f.bob, "c1", `{"body":"hi"}`); w.Code != 403 {
		t.Fatalf("bob comments: %d %s", w.Code, w.Body)
	}
	// Legacy writer token: reads, never writes tasks.
	if w := taskReq(t, h, "GET", "/v1/tasks/"+task.ID, f.writer, "", ""); w.Code != 200 {
		t.Fatalf("writer reads: %d", w.Code)
	}
	if w := taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.writer, "k4", `{"title":"x","state":"READY","expected_revision":1}`); w.Code != 403 {
		t.Fatalf("writer updates: %d %s", w.Code, w.Body)
	}
	// Admin (named token and env token) writes anywhere, stamped as admin.
	w = taskReq(t, h, "POST", "/v1/projects/beta/tasks", f.admin, "k5", taskBody)
	if w.Code != 201 {
		t.Fatalf("admin creates in beta: %d %s", w.Code, w.Body)
	}
	betaTask := decodeTask(t, w)
	w = taskReq(t, h, "PUT", "/v1/tasks/"+betaTask.ID, f.env, "k6", `{"title":"renamed","state":"IN_PROGRESS","expected_revision":1}`)
	if w.Code != 200 || decodeTask(t, w).Revision != 2 {
		t.Fatalf("env admin updates: %d %s", w.Code, w.Body)
	}
	w = taskReq(t, h, "GET", "/v1/tasks/"+betaTask.ID+"/history", f.env, "", "")
	var betaHist struct {
		Changes []store.TaskChange `json:"changes"`
	}
	json.Unmarshal(w.Body.Bytes(), &betaHist)
	if len(betaHist.Changes) != 2 || betaHist.Changes[1].Actor.Kind != "admin" || betaHist.Changes[1].Actor.Name != "env" || betaHist.Changes[1].Actor.UserID != "" {
		t.Fatalf("admin actor stamp: %+v", betaHist.Changes)
	}

	// Ordinary tokens stay out of the legacy surface.
	if w := taskReq(t, h, "GET", "/v1/projects/alpha/docs", f.alice, "", ""); w.Code != 403 {
		t.Fatalf("ordinary token on legacy route: %d", w.Code)
	}
	if w := taskReq(t, h, "GET", "/v1/access/identity?project=alpha", f.alice, "", ""); w.Code != 200 {
		t.Fatalf("identity: %d", w.Code)
	}
}

// The gate admits an ordinary token to exactly the ordinary routes: every
// other route in the table answers 403, methods matter, and dot-segment
// paths never reach a legacy handler in either direction.
// The project listing is within an ordinary token's view — it may read
// tasks in every ordinary project — but the reserved stores are not: the
// user memory DB and the knowledge groups never hold tasks. Legacy
// credentials keep the unfiltered list.
func TestProjectListForOrdinaryTokens(t *testing.T) {
	f := newTaskFixture(t)
	for _, p := range []string{store.UserScopeProject, "group-shared"} {
		if _, err := f.reg.Open(p); err != nil {
			t.Fatal(err)
		}
	}
	list := func(token string) []string {
		w := taskReq(t, f.h, "GET", "/v1/projects", token, "", "")
		if w.Code != 200 {
			t.Fatalf("GET /v1/projects: %d %s", w.Code, w.Body)
		}
		var out struct {
			Projects []string `json:"projects"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Projects
	}
	for _, tok := range []string{f.alice, f.bob} { // scoped and read-only ordinary tokens alike
		got := list(tok)
		if !slices.Contains(got, "alpha") {
			t.Fatalf("ordinary token must see the ordinary projects: %v", got)
		}
		for _, p := range got {
			if store.IsReservedProject(p) {
				t.Fatalf("ordinary token must not see reserved store %q: %v", p, got)
			}
		}
	}
	for _, tok := range []string{f.admin, f.writer, f.env} {
		got := list(tok)
		if !slices.Contains(got, store.UserScopeProject) || !slices.Contains(got, "group-shared") {
			t.Fatalf("legacy credential's listing changed: %v", got)
		}
	}
}

// Task enablement is an admin decision recorded on the hub: a disabled
// project refuses every mutation — create, update, comment — from every
// credential, the local operator and admin tokens included, and keeps its
// reads; the setting itself is writable by the host console and admin
// tokens only, and every credential reads it through the identity route.
func TestTaskEnablementGate(t *testing.T) {
	f := newTaskFixture(t)
	create := func(token string, want int) string {
		t.Helper()
		w := taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", token, "k-"+uuidv7.New(), `{"title":"gate"}`)
		if w.Code != want {
			t.Fatalf("create as %q: %d %s (want %d)", token[:6], w.Code, w.Body, want)
		}
		var out struct {
			ID string `json:"id"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		return out.ID
	}
	id := create(f.alice, 201)
	// Switch off through the admin API; the writer token may not.
	if w := taskReq(t, f.h, "PUT", "/v1/projects/alpha/meta/tasks", f.writer, "", `{"value":"off"}`); w.Code != 403 {
		t.Fatalf("writer token switched tasks: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "PUT", "/v1/projects/alpha/meta/tasks", f.admin, "", `{"value":"maybe"}`); w.Code != 400 {
		t.Fatalf("bad value accepted: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "PUT", "/v1/projects/alpha/meta/tasks", f.admin, "", `{"value":"off"}`); w.Code != 200 {
		t.Fatalf("admin could not switch tasks off: %d %s", w.Code, w.Body)
	}
	for _, tok := range []string{f.alice, f.admin, f.env} {
		create(tok, 403)
	}
	// The local operator (socket, no bearer) is refused too.
	w := taskReq(t, f.s.Handler(), "POST", "/v1/projects/alpha/tasks", "", "k-local", `{"title":"gate"}`)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "not enabled") {
		t.Fatalf("local operator wrote into a disabled project: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "PUT", "/v1/tasks/"+id, f.alice, "k-upd", `{"title":"gate","state":"READY","expected_revision":1}`); w.Code != 403 {
		t.Fatalf("update in a disabled project: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "POST", "/v1/tasks/"+id+"/comments", f.alice, "k-cmt", `{"body":"x"}`); w.Code != 403 {
		t.Fatalf("comment in a disabled project: %d %s", w.Code, w.Body)
	}
	// Reads stay: the list, the task and the identity's answer.
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks", f.alice, "", ""); w.Code != 200 {
		t.Fatalf("list in a disabled project: %d", w.Code)
	}
	if w := taskReq(t, f.h, "GET", "/v1/tasks/"+id, f.bob, "", ""); w.Code != 200 {
		t.Fatalf("read in a disabled project: %d", w.Code)
	}
	for _, tok := range []string{f.alice, f.admin, f.writer} {
		w := taskReq(t, f.h, "GET", "/v1/access/identity?project=alpha", tok, "", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"tasks_enabled":false`) {
			t.Fatalf("identity for %q must say tasks are off: %d %s", tok[:6], w.Code, w.Body)
		}
	}
	if w := taskReq(t, f.h, "GET", "/v1/access/identity?project=nope", f.admin, "", ""); w.Code != 404 {
		t.Fatalf("identity for an unknown project: %d %s", w.Code, w.Body)
	}
	// The host console switches it back on; writes resume.
	w = taskReq(t, f.s.Handler(), "PUT", "/v1/projects/alpha/meta/tasks", "", "", `{"value":"on"}`)
	if w.Code != 200 {
		t.Fatalf("host console could not switch tasks on: %d %s", w.Code, w.Body)
	}
	create(f.alice, 201)
	if w := taskReq(t, f.h, "GET", "/v1/access/identity?project=alpha", f.alice, "", ""); !strings.Contains(w.Body.String(), `"tasks_enabled":true`) {
		t.Fatalf("identity after re-enable: %s", w.Body)
	}
}

func TestOrdinaryTokenGateMatrix(t *testing.T) {
	f := newTaskFixture(t)
	// The admitted set is pinned here, independently of the map the gate
	// consults: widening it is a deliberate, reviewed change.
	want := []string{"GET /v1/projects/{p}/tasks", "POST /v1/projects/{p}/tasks", "GET /v1/tasks/{id}", "PUT /v1/tasks/{id}",
		"GET /v1/tasks/{id}/history", "GET /v1/tasks/{id}/comments", "POST /v1/tasks/{id}/comments", "GET /v1/tasks/{id}/comments/{c}", "POST /mcp",
		"GET /v1/projects",                                                                                                              // the listing, read only, reserved stores filtered (TestProjectListForOrdinaryTokens)
		"GET /v1/projects/{p}/process",                                                                                                  // the selected process reference (TestProcessReferenceSelection)
		"GET /v1/projects/{p}/epics", "POST /v1/projects/{p}/epics", "GET /v1/projects/{p}/epics/{e}", "PUT /v1/projects/{p}/epics/{e}", // epics (TestEpicRoutes)
		"GET /v1/access/directory"} // the identity directory (TestAccessDirectory)
	if len(ordinaryRoutes) != len(want) {
		t.Fatalf("ordinary surface changed: %v", ordinaryRoutes)
	}
	for _, p := range want {
		if !ordinaryRoutes[p] {
			t.Fatalf("ordinary surface lost %q", p)
		}
	}
	mcpHits := 0
	h := f.s.TCPHandler(f.env, map[string]http.Handler{"/mcp": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mcpHits++ })})
	fill := strings.NewReplacer("{p}", "alpha", "{id}", uuidv7.New(), "{c}", uuidv7.New(), "{s}", "s1", "{key}", "about",
		"{name}", "RUNBOOK", "{instance}", "x", "{kind}", "user", "{g}", "g", "{u}", "u", "{id...}", "x", "{$}", "", "{e}", uuidv7.New())
	public := f.s.publicGETs()
	for _, rt := range f.s.Routes() {
		path := fill.Replace(rt.Pattern)
		if path == "" {
			path = "/"
		}
		body := ""
		if rt.Method != "GET" && rt.Method != "DELETE" {
			body = "{}"
		}
		w := taskReq(t, h, rt.Method, path, f.alice, "", body)
		switch {
		case public[path] != nil || (rt.Method == "GET" && path == "/v1/access/identity"):
		case rt.Ordinary():
			if w.Code == 401 || w.Code == 403 {
				t.Errorf("%s %s: ordinary route refused at the gate: %d %s", rt.Method, rt.Pattern, w.Code, w.Body)
			}
		default:
			if w.Code != 403 {
				t.Errorf("%s %s: non-ordinary route reachable by an ordinary token: %d", rt.Method, rt.Pattern, w.Code)
			}
		}
	}
	if w := taskReq(t, h, "POST", "/mcp", f.alice, "", "{}"); w.Code == 403 || mcpHits != 1 {
		t.Fatalf("POST /mcp must reach the dispatcher: %d hits=%d", w.Code, mcpHits)
	}
	if w := taskReq(t, h, "GET", "/mcp", f.alice, "", ""); w.Code != 403 || mcpHits != 1 {
		t.Fatalf("GET /mcp: %d hits=%d", w.Code, mcpHits)
	}
	if w := taskReq(t, h, "PUT", "/v1/projects/alpha/tasks", f.alice, "", "{}"); w.Code != 403 {
		t.Fatalf("PUT on the list route: %d", w.Code)
	}
	// A non-canonical path that would clean to a task route is refused at
	// the gate itself, not left to the mux's redirect.
	if w := taskReq(t, h, "GET", "/v1/logs/../tasks/"+uuidv7.New(), f.alice, "", ""); w.Code != 403 {
		t.Fatalf("non-canonical path admitted: %d", w.Code)
	}
	// The actor derivation fails closed for a legacy writer even without
	// the authorization step.
	wid, _ := f.s.authenticate(f.env, f.writer)
	wr := httptest.NewRequest("POST", "/", nil)
	wr = wr.WithContext(withIdentity(wr.Context(), wid))
	if a := taskActor(wr); a.Kind == "admin" || a.Kind == "user" {
		t.Fatalf("writer must not become a trusted actor: %+v", a)
	}
	for _, p := range []string{"/v1/projects/alpha/tasks/../docs", "/v1/tasks/../projects/alpha/docs",
		"/v1/tasks/x/../../projects/alpha/memories", "/v1/tasks/..%2Fprojects%2Falpha%2Fdocs", "/v1/tasks/%2e%2e/projects/alpha/docs"} {
		w := taskReq(t, h, "GET", p, f.alice, "", "")
		if w.Code == 200 || w.Code == 201 {
			t.Fatalf("%s reached a handler: %d %s", p, w.Code, w.Body)
		}
		if loc := w.Header().Get("Location"); loc != "" {
			if w2 := taskReq(t, h, "GET", loc, f.alice, "", ""); w2.Code != 403 {
				t.Fatalf("%s -> %s: %d", p, loc, w2.Code)
			}
		}
	}
}

func TestTaskRoutesCASCommentsAndPermissionChanges(t *testing.T) {
	f := newTaskFixture(t)
	h := f.h
	task := decodeTask(t, taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "k1", taskBody))

	// Stale revision: 409 with the current task; nothing written.
	w := taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u1", `{"title":"v2","state":"READY","expected_revision":1}`)
	if w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	w = taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u2", `{"title":"stale","state":"READY","expected_revision":1}`)
	if w.Code != 409 {
		t.Fatalf("stale update: %d %s", w.Code, w.Body)
	}
	var conflict struct {
		Error   string     `json:"error"`
		Current store.Task `json:"current"`
	}
	json.Unmarshal(w.Body.Bytes(), &conflict)
	if conflict.Current.Revision != 2 || conflict.Current.Title != "v2" {
		t.Fatalf("conflict body: %+v", conflict)
	}
	// Strict bodies.
	for name, body := range map[string]string{
		"unknown field": `{"title":"x","state":"READY","expected_revision":2,"author":"me"}`,
		"trailing":      `{"title":"x","state":"READY","expected_revision":2} {}`,
		"not object":    `[1]`,
	} {
		if w := taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u3-"+name, body); w.Code != 400 {
			t.Fatalf("%s: %d %s", name, w.Code, w.Body)
		}
	}
	if w := taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u4", `{"title":"x","state":"READY","expected_revision":2,"objective":"`+strings.Repeat("a", maxTaskRequestBytes)+`"}`); w.Code != 413 {
		t.Fatalf("oversized body: %d", w.Code)
	}
	if w := taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u5", `{"title":"","state":"READY","expected_revision":2}`); w.Code != 400 {
		t.Fatalf("validation: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u5b", `{"title":"x","state":"READY","expected_revision":2}`+strings.Repeat(" ", maxTaskRequestBytes)); w.Code != 413 {
		t.Fatalf("oversized trailing bytes: %d", w.Code)
	}
	// Assignees must name a known identity; the 409 carries the full view.
	if w := taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u5c", `{"title":"x","state":"READY","expected_revision":2,"assignee":{"kind":"user","id":"`+uuidv7.New()+`"}}`); w.Code != 400 || !strings.Contains(w.Body.String(), "assignee") {
		t.Fatalf("unknown assignee: %d %s", w.Code, w.Body)
	}
	w = taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u5d", `{"title":"x","state":"READY","expected_revision":1,"assignee":{"kind":"user","id":"`+f.bobUser+`"}}`)
	var conflictView struct {
		Current taskResponse `json:"current"`
	}
	json.Unmarshal(w.Body.Bytes(), &conflictView)
	if w.Code != 409 || conflictView.Current.Project != "alpha" || conflictView.Current.Links.Self != "/v1/tasks/"+task.ID {
		t.Fatalf("409 must carry the task in its served shape: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u5e", `{"title":"x","state":"READY","expected_revision":2,"assignee":{"kind":"user","id":"`+f.bobUser+`"}}`); w.Code != 200 {
		t.Fatalf("assign to a known user: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u5f", `{"title":"v2","state":"READY","expected_revision":3}`); w.Code != 200 {
		t.Fatalf("clear assignee: %d %s", w.Code, w.Body)
	}
	// List-route validation.
	for _, q := range []string{"?include_archived=yes", "?assignee=alice", "?limit=-1", "?limit=x", "?state=DOING"} {
		if w := taskReq(t, h, "GET", "/v1/projects/alpha/tasks"+q, f.bob, "", ""); w.Code != 400 {
			t.Fatalf("list %s: %d %s", q, w.Code, w.Body)
		}
	}
	if w := taskReq(t, h, "GET", "/v1/projects/nope/tasks", f.bob, "", ""); w.Code != 404 {
		t.Fatalf("list unknown project: %d", w.Code)
	}
	if w := taskReq(t, h, "GET", "/v1/projects/alpha/tasks?assignee=user/"+f.bobUser, f.bob, "", ""); w.Code != 200 || strings.Contains(w.Body.String(), task.ID) {
		t.Fatalf("assignee filter after clearing: %d %s", w.Code, w.Body)
	}
	w = taskReq(t, h, "GET", "/v1/projects/alpha/tasks", f.bob, "", "")
	if !strings.Contains(w.Body.String(), `"self":"/v1/tasks/`+task.ID+`"`) || strings.Contains(w.Body.String(), `"next_cursor"`) {
		t.Fatalf("list rows carry links and the last page has no cursor: %s", w.Body)
	}
	// Comments: created, replayed, listed, fetched, wrong parent hidden.
	w = taskReq(t, h, "POST", "/v1/tasks/"+task.ID+"/comments", f.alice, "c1", `{"body":"first **note**"}`)
	if w.Code != 201 {
		t.Fatalf("comment: %d %s", w.Code, w.Body)
	}
	var c commentResponse
	json.Unmarshal(w.Body.Bytes(), &c)
	if c.Actor.UserID != f.aliceUser || c.Links.Self != "/v1/tasks/"+task.ID+"/comments/"+c.ID {
		t.Fatalf("comment view: %+v", c)
	}
	w = taskReq(t, h, "POST", "/v1/tasks/"+task.ID+"/comments", f.alice, "c1", `{"body":"first **note**"}`)
	var again commentResponse
	json.Unmarshal(w.Body.Bytes(), &again)
	if again.ID != c.ID {
		t.Fatalf("comment replay: %s vs %s", again.ID, c.ID)
	}
	if got := decodeTask(t, taskReq(t, h, "GET", "/v1/tasks/"+task.ID, f.alice, "", "")); got.Revision != 4 {
		t.Fatalf("comment bumped the revision: %d", got.Revision)
	}
	if w := taskReq(t, h, "GET", "/v1/tasks/"+task.ID+"/comments/"+c.ID, f.bob, "", ""); w.Code != 200 {
		t.Fatalf("get comment: %d", w.Code)
	}
	other := decodeTask(t, taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "k2", taskBody))
	if w := taskReq(t, h, "GET", "/v1/tasks/"+other.ID+"/comments/"+c.ID, f.bob, "", ""); w.Code != 404 {
		t.Fatalf("wrong parent: %d", w.Code)
	}
	w = taskReq(t, h, "GET", "/v1/tasks/"+task.ID+"/comments?limit=1", f.bob, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), c.ID) || strings.Contains(w.Body.String(), `"next_cursor"`) {
		t.Fatalf("single-page comments must carry no cursor: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "GET", "/v1/tasks/"+task.ID+"/comments?limit=500", f.bob, "", ""); w.Code != 400 {
		t.Fatalf("limit bound: %d", w.Code)
	}
	// Archive, then a new comment is refused while the replay still works.
	w = taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u6", `{"title":"v2","state":"DONE","archived":true,"expected_revision":4}`)
	if w.Code != 200 {
		t.Fatalf("archive: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "POST", "/v1/tasks/"+task.ID+"/comments", f.alice, "c2", `{"body":"late"}`); w.Code != 409 {
		t.Fatalf("comment on archived: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "POST", "/v1/tasks/"+task.ID+"/comments", f.alice, "c1", `{"body":"first **note**"}`); w.Code != 201 {
		t.Fatalf("replay after archive: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "GET", "/v1/projects/alpha/tasks", f.bob, "", ""); w.Code != 200 || strings.Contains(w.Body.String(), task.ID) || !strings.Contains(w.Body.String(), other.ID) {
		t.Fatalf("archived task listed by default, or listing broken: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "GET", "/v1/projects/alpha/tasks?include_archived=true&state=DONE", f.bob, "", ""); !strings.Contains(w.Body.String(), task.ID) {
		t.Fatalf("archived task with filter: %s", w.Body)
	}

	// Permission changes take effect on the next attempt: a removed grant
	// refuses, a replay with the same key is refused too (receipts never
	// grant), a restored grant allows, a revoked token is 401.
	db, _ := f.s.openAccess(false)
	if err := db.SetGrant("admin", f.alphaInstance, "user", f.aliceUser, false); err != nil {
		t.Fatal(err)
	}
	if w := taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "k9", taskBody); w.Code != 403 {
		t.Fatalf("after grant removal: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "k1", taskBody); w.Code != 403 {
		t.Fatalf("replay after grant removal: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "GET", "/v1/tasks/"+other.ID, f.alice, "", ""); w.Code != 200 {
		t.Fatalf("reads survive grant removal: %d", w.Code)
	}
	if err := db.SetGrant("admin", f.alphaInstance, "user", f.aliceUser, true); err != nil {
		t.Fatal(err)
	}
	if w := taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "k9", taskBody); w.Code != 201 {
		t.Fatalf("after grant restored: %d %s", w.Code, w.Body)
	}
	if err := db.SetUser("admin", f.aliceUser, "Alice", true); err != nil {
		t.Fatal(err)
	}
	if w := taskReq(t, h, "GET", "/v1/tasks/"+other.ID, f.alice, "", ""); w.Code != 401 {
		t.Fatalf("disabled user: %d", w.Code)
	}
	if err := db.SetUser("admin", f.aliceUser, "Alice", false); err != nil {
		t.Fatal(err)
	}
	if err := db.Revoke("admin", f.aliceTokenID); err != nil {
		t.Fatal(err)
	}
	if w := taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "k10", taskBody); w.Code != 401 {
		t.Fatalf("revoked token: %d", w.Code)
	}
	// Expiry: a token issued for two seconds (the store truncates expiry
	// to whole seconds) is refused once it has passed.
	_, short, err := db.Issue("admin", f.aliceUser, "short", f.alphaInstance, time.Now().Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if w := taskReq(t, h, "GET", "/v1/tasks/"+other.ID, short, "", ""); w.Code != 200 {
		t.Fatalf("fresh short token: %d", w.Code)
	}
	time.Sleep(2100 * time.Millisecond)
	if w := taskReq(t, h, "POST", "/v1/projects/alpha/tasks", short, "k11", taskBody); w.Code != 401 {
		t.Fatalf("expired token: %d", w.Code)
	}
}

// A storage fault is a 500 that names nothing internal, and a task in a
// readable project is still found past an unreadable sibling.
func TestTaskRoutesStorageFaultMapping(t *testing.T) {
	f := newTaskFixture(t)
	task := decodeTask(t, taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, "k1", taskBody))
	broken := filepath.Join(f.reg.Root(), "projects", "aaa-broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "journal.db"), []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := taskReq(t, f.h, "GET", "/v1/tasks/"+task.ID, f.bob, "", ""); w.Code != 200 {
		t.Fatalf("task past an unreadable sibling: %d %s", w.Code, w.Body)
	}
	w := taskReq(t, f.h, "GET", "/v1/tasks/"+uuidv7.New(), f.bob, "", "")
	if w.Code != 500 {
		t.Fatalf("inconclusive lookup must be a fault, not a miss: %d %s", w.Code, w.Body)
	}
	// Project-addressed routes: an existing project that cannot be opened
	// is a fault, an absent one is a miss, and neither body names internals.
	for _, c := range []struct {
		method, path, key, body string
		want                    int
	}{
		{"GET", "/v1/projects/aaa-broken/tasks", "", "", 500},
		{"POST", "/v1/projects/aaa-broken/tasks", "kb", taskBody, 500},
		{"GET", "/v1/projects/absent/tasks", "", "", 404},
		{"POST", "/v1/projects/absent/tasks", "ka", taskBody, 404},
	} {
		w := taskReq(t, f.h, c.method, c.path, f.admin, c.key, c.body)
		if w.Code != c.want {
			t.Fatalf("%s %s: %d %s", c.method, c.path, w.Code, w.Body)
		}
		if b := strings.ToLower(w.Body.String()); strings.Contains(b, "sqlite") || strings.Contains(b, "journal.db") || strings.Contains(b, "migrat") {
			t.Fatalf("%s %s leaks internals: %s", c.method, c.path, w.Body)
		}
	}
	// A stat failure that is not "absent" (POSIX: no search permission on
	// the projects directory) is a fault too, never a 404.
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		f.reg.Close() // no cached handle: the route must stat the directory
		dir := filepath.Join(f.reg.Root(), "projects")
		if err := os.Chmod(dir, 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
		w := taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks", f.admin, "", "")
		if w.Code != 500 || strings.Contains(strings.ToLower(w.Body.String()), "permission") {
			t.Fatalf("inaccessible projects directory: %d %s", w.Code, w.Body)
		}
		os.Chmod(dir, 0o755)
	}
	body := strings.ToLower(w.Body.String())
	rootHint := strings.ToLower(filepath.Base(filepath.Dir(f.reg.Root()))) // the temp dir's test-named parent
	if strings.Contains(body, "aaa-broken") || strings.Contains(body, "sqlite") || strings.Contains(body, "journal.db") || strings.Contains(body, rootHint) {
		t.Fatalf("500 body leaks internals: %s", w.Body)
	}
}

func TestTaskRoutesProjectsRenameAndReservedScopes(t *testing.T) {
	f := newTaskFixture(t)
	h := f.h
	if w := taskReq(t, h, "POST", "/v1/projects/nope/tasks", f.admin, "k1", taskBody); w.Code != 404 {
		t.Fatalf("unknown project: %d %s", w.Code, w.Body)
	}
	if _, err := f.reg.OpenExisting("nope"); err == nil {
		t.Fatal("a task write must never create a project")
	}
	for _, p := range []string{"user", "group-kb"} {
		if w := taskReq(t, h, "POST", "/v1/projects/"+p+"/tasks", f.admin, "k1", taskBody); w.Code != 400 {
			t.Fatalf("reserved %s: %d %s", p, w.Code, w.Body)
		}
	}
	task := decodeTask(t, taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "k1", taskBody))
	if w := taskReq(t, h, "GET", "/v1/tasks/"+strings.Repeat("0", 36), f.admin, "", ""); w.Code != 404 {
		t.Fatalf("missing task: %d", w.Code)
	}
	// Rename: the task keeps its id and links; the project resolves anew.
	if err := f.reg.Rename("alpha", "alpha-next"); err != nil {
		t.Fatal(err)
	}
	got := decodeTask(t, taskReq(t, h, "GET", "/v1/tasks/"+task.ID, f.admin, "", ""))
	if got.Project != "alpha-next" || got.Links.Project != "/v1/projects/alpha-next" || got.ID != task.ID {
		t.Fatalf("after rename: %+v", got)
	}
	// The access instance moved with the directory: alice still writes.
	if w := taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u1", `{"title":"after rename","state":"READY","expected_revision":1}`); w.Code != 200 {
		t.Fatalf("write after rename: %d %s", w.Code, w.Body)
	}
	// A project recreated under the old name inherits nothing.
	if _, err := f.reg.Open("alpha"); err != nil {
		t.Fatal(err)
	}
	if w := taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "k2", taskBody); w.Code != 403 {
		t.Fatalf("name reuse: %d %s", w.Code, w.Body)
	}
}

// The task page is public chrome: served without a credential, under the
// console's CSP, holding no data; its script parses (see adminjs_test.go
// for why that check exists) and it asks only the routes an ordinary
// token may reach.
func TestTasksPageIsPublicChrome(t *testing.T) {
	f := newTaskFixture(t)
	w := taskReq(t, f.h, "GET", "/tasks", "", "", "")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("tasks page: %d %v", w.Code, w.Header())
	}
	// One policy for both pages, and it closes framing, form posts and
	// base overrides on top of the subresource and connect restrictions.
	csp := w.Header().Get("Content-Security-Policy")
	admin := taskReq(t, f.h, "GET", "/admin", "", "", "")
	if a := admin.Header().Get("Content-Security-Policy"); csp != a {
		t.Fatalf("pages disagree on CSP:\n%s\n%s", csp, a)
	}
	if c := w.Header().Get("Cache-Control"); c != "no-cache" || c != admin.Header().Get("Cache-Control") {
		t.Fatalf("pages must not be cached: %q vs %q", c, admin.Header().Get("Cache-Control"))
	}
	for _, d := range []string{"default-src 'none'", "connect-src 'self'", "frame-ancestors 'none'", "form-action 'none'", "base-uri 'none'"} {
		if !strings.Contains(csp, d) {
			t.Fatalf("CSP lacks %q: %s", d, csp)
		}
	}
	page := string(tasksHTML)
	open, closeAt := strings.Index(page, "<script>"), strings.LastIndex(page, "</script>")
	if open < 0 || closeAt < open {
		t.Fatal("tasks.html has no <script> block")
	}
	if bad := scanJSStrings(page[open+len("<script>") : closeAt]); len(bad) > 0 {
		t.Fatalf("unterminated string literals at script lines %v", bad)
	}
	// The page's call surface is pinned exactly: every api(...) call site's
	// path expression is listed here, api() is the only egress (one fetch
	// in the whole page), and each path is a task route, the identity
	// check, or the project listing (every credential class may call it;
	// the catch keeps the free-text fallback if it ever fails). A new call
	// is a reviewed edit.
	want := map[string]bool{
		`"/v1/access/identity?project="+encodeURIComponent(project)`: true,
		`"/v1/access/identity"`: true,
		`"/v1/projects"`:        true,
		`"/v1/projects/"+encodeURIComponent(PROJ)+"/tasks?"+q`:                               true,
		`"/v1/projects/"+encodeURIComponent(project)+"/tasks"`:                               true,
		`"/v1/projects/"+encodeURIComponent(project)+"/epics?include_retired=true"`:          true,
		`"/v1/tasks/"+encodeURIComponent(id)`:                                                true,
		`"/v1/tasks/"+encodeURIComponent(CUR.id)+"/history?limit=20&after="+HIST.after`:      true,
		`"/v1/tasks/"+encodeURIComponent(CUR.id)+"/comments?limit=20&after="+CMT.after`:      true,
		`"/v1/tasks/"+encodeURIComponent(CUR.id)+"/comments/"+encodeURIComponent(commentID)`: true,
		`"/v1/tasks/"+encodeURIComponent(id)+"/comments"`:                                    true,
	}
	got := map[string]bool{}
	for _, expr := range apiCallSites(page) {
		if !want[expr] {
			t.Fatalf("page calls api(%s), not in the pinned call surface", expr)
		}
		got[expr] = true
	}
	for expr := range want {
		if !got[expr] {
			t.Fatalf("page no longer calls api(%s)", expr)
		}
	}
	if n := strings.Count(page, "fetch("); n != 1 {
		t.Fatalf("api() must be the page's only egress: %d fetch( sites", n)
	}
	// The board is the same rows and the same write: it reads through the
	// list route and moves through the task route (both already pinned).
	// Its columns are the page's STATES, which must be the store's states
	// exactly — a task in a state the page does not know has no column.
	m := regexp.MustCompile(`const STATES = \[([^\]]*)\];`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("page has no STATES literal")
	}
	var pageStates []string
	for _, q := range strings.Split(m[1], ",") {
		pageStates = append(pageStates, strings.Trim(strings.TrimSpace(q), `"`))
	}
	if strings.Join(pageStates, ",") != strings.Join(store.TaskStates, ",") {
		t.Fatalf("page STATES %v differ from store.TaskStates %v", pageStates, store.TaskStates)
	}
	// An archived task has no card: a move that reads or returns one
	// archived (by another client meanwhile) must take it off the board —
	// and the newest revision seen must outlive the card, so a delayed
	// older response cannot put it back.
	// A stored token rejected at load is named as the remembered one (the
	// user did not type it); a token just typed keeps the plain message.
	// The edit form keeps a task's current epic even when the project's
	// epics are not loaded (a direct task link), and the epic filter is
	// cleared before the first query of another project.
	for _, want := range []string{`view=board`, `ondrop=`, `if(t.archived){ if(from) countCol(from); return; }`, `if(SEEN[t.id] > t.revision) return;`,
		"${t&&t.epic&&!EPICS[t.epic]?`<option value=\"${esc(t.epic)}\" selected>", `EPICS = {}; EPICS_PROJ = project; FILTER.epic = "";`, `const gen = ++EPICS_GEN;`,
		`if(LINK_KINDS[r.kind] && /^https?:\/\//i.test(ref)) body =`, `function parseRefs(text){`, `candidate_refs:parseRefs(g("candidate_refs")), evidence_refs:parseRefs(g("evidence_refs")),`, `if(gen!==EPICS_GEN) return;`, `EPICS = got;`, `loadEpics(CUR.project);`,
		`let TOK_REMEMBERED = !!TOK;`, `TOK_REMEMBERED=false;`,
		`TOK_REMEMBERED?"The token remembered in this browser from an earlier visit was rejected (invalid, expired, revoked, or the user is disabled) — paste a current one.":"That token was rejected (invalid, expired, revoked, or the user is disabled)."`} {
		if !strings.Contains(page, want) {
			t.Fatalf("board: page lacks %q", want)
		}
	}
	if !strings.Contains(page, "catch(_){ PROJECTS = null; }") {
		t.Fatal("project listing must be optional for ordinary tokens")
	}
	// The write decision is the identity endpoint's task_write answer.
	// The enablement note lives in its own element (the list loader
	// rewrites listNote on every page and filter), shown by the identity
	// answer for the project on both the list and the board.
	for _, want := range []string{`<div id="tasksOff" class="dim" hidden></div>`, `$("tasksOff").hidden = !TASKS_OFF[asked];`, `$("tasksOff").hidden = !TASKS_OFF[project];`, `$("tasksOff").hidden = true;`} {
		if !strings.Contains(page, want) {
			t.Fatalf("page lost the enablement note wiring: %s", want)
		}
	}
	if !strings.Contains(page, `TASKS_OFF[project] = r.tasks_enabled===false;`) || !strings.Contains(page, `if(r.tasks_enabled===false) return false;`) || !strings.Contains(page, `return ME.role==="admin" || !!r.task_write;`) {
		t.Fatal("page must decide writes from the identity endpoint's task_write")
	}
	// A deep link into the console's task view lands here, and the console
	// does not boot while forwarding.
	console := string(adminHTML)
	if !strings.Contains(console, `location.replace("/tasks?"`) || !strings.Contains(console, "if(TOK && !FORWARDING) boot();") {
		t.Fatal("console must forward /admin?task= to the task page without booting")
	}
}

// apiCallSites returns the first-argument expression of every api(...)
// call in the page (parentheses balanced, up to the first top-level comma).
func apiCallSites(page string) []string {
	var out []string
	for i := 0; ; {
		j := strings.Index(page[i:], "api(")
		if j < 0 {
			break
		}
		start := i + j + len("api(")
		if strings.HasSuffix(page[:i+j], "function ") { // the definition itself
			i = start
			continue
		}
		depth, k := 0, start
		for ; k < len(page); k++ {
			c := page[k]
			if c == '(' {
				depth++
			} else if c == ')' {
				if depth == 0 {
					break
				}
				depth--
			} else if c == ',' && depth == 0 {
				break
			}
		}
		out = append(out, strings.TrimSpace(page[start:k]))
		i = k
	}
	return out
}

// The local unix socket carries no identity: the operator's CLI works and
// is stamped "local".
func TestTaskRoutesLocalSocketIsOperator(t *testing.T) {
	s, reg := testServer(t)
	h := s.Handler()
	if _, err := reg.Open("alpha"); err != nil {
		t.Fatal(err)
	}
	// The operator switches tasks on through the same socket first: the
	// gate applies to the operator too.
	if w := taskReq(t, h, "PUT", "/v1/projects/alpha/meta/tasks", "", "", `{"value":"on"}`); w.Code != 200 {
		t.Fatalf("operator could not enable tasks: %d %s", w.Code, w.Body)
	}
	w := taskReq(t, h, "POST", "/v1/projects/alpha/tasks", "", "k1", taskBody)
	if w.Code != 201 {
		t.Fatalf("local create: %d %s", w.Code, w.Body)
	}
	task := decodeTask(t, w)
	var hist struct {
		Changes []store.TaskChange `json:"changes"`
	}
	json.Unmarshal(taskReq(t, h, "GET", "/v1/tasks/"+task.ID+"/history", "", "", "").Body.Bytes(), &hist)
	if len(hist.Changes) != 1 || hist.Changes[0].Actor.Kind != "admin" || hist.Changes[0].Actor.Name != "local" {
		t.Fatalf("local actor: %+v", hist.Changes)
	}
}

// MCPPrincipal binds in-process task dispatch to the request's identity
// and refuses anything that is not a task route.
func TestMCPPrincipalDispatch(t *testing.T) {
	f := newTaskFixture(t)
	// Simulate what the bearer wrapper does: authenticate, stamp identity.
	id, ok := f.s.authenticate(f.env, f.alice)
	if !ok {
		t.Fatal("alice must authenticate")
	}
	r := httptest.NewRequest("POST", "/mcp", nil)
	r = r.WithContext(withIdentity(r.Context(), id))
	call, tasksOnly := f.s.MCPPrincipal(r)
	if !tasksOnly {
		t.Fatal("ordinary token must be tasks-only")
	}
	status, body, err := call(r.Context(), "POST", "/v1/projects/alpha/tasks", map[string]string{"Idempotency-Key": "m1"}, []byte(taskBody))
	if err != nil || status != 201 {
		t.Fatalf("dispatch create: %d %v %s", status, err, body)
	}
	var task taskResponse
	if json.Unmarshal(body, &task); task.Project != "alpha" || task.Revision != 1 {
		t.Fatalf("dispatched task view: %+v", task)
	}
	status, body, _ = call(r.Context(), "POST", "/v1/projects/beta/tasks", map[string]string{"Idempotency-Key": "m2"}, []byte(taskBody))
	if status != 403 {
		t.Fatalf("dispatch is bound to alice's authority: %d %s", status, body)
	}
	if _, _, err := call(r.Context(), "GET", "/v1/projects/alpha/docs", nil, nil); err == nil {
		t.Fatal("non-task route must not be dispatchable")
	}
	// The identity is bound by the principal, whatever context the
	// dispatcher passes: a bare context still acts as alice, never as the
	// local operator.
	status, body, err = call(httptest.NewRequest("GET", "/", nil).Context(), "POST", "/v1/projects/beta/tasks", map[string]string{"Idempotency-Key": "m3"}, []byte(taskBody))
	if err != nil || status != 403 {
		t.Fatalf("bare context must still be alice: %d %v %s", status, err, body)
	}
	if _, _, err := call(r.Context(), "POST", "/mcp", nil, nil); err == nil {
		t.Fatal("/mcp must not be re-entrant")
	}
	// The admin principal keeps legacy tools visible.
	adminID, _ := f.s.authenticate(f.env, f.admin)
	ar := httptest.NewRequest("POST", "/mcp", nil)
	ar = ar.WithContext(withIdentity(ar.Context(), adminID))
	if _, only := f.s.MCPPrincipal(ar); only {
		t.Fatal("admin must not be tasks-only")
	}
	if c, only := f.s.MCPPrincipal(httptest.NewRequest("POST", "/mcp", nil)); c != nil || only {
		t.Fatal("no identity: no task caller, legacy tools untouched")
	}
}

// Epic routes: reads on the ordinary surface, writes authorized like task
// writes (grant, enablement), the revision conflict carrying the current
// epic, retirement refusing new task assignments but keeping existing
// ones, and the epic filter on the task list.
func TestEpicRoutes(t *testing.T) {
	f := newTaskFixture(t)
	w := taskReq(t, f.h, "POST", "/v1/projects/alpha/epics", f.alice, "e1", `{"title":"release 0.5","objective":"typed refs","target":"v0.5.0"}`)
	if w.Code != 201 {
		t.Fatalf("create epic: %d %s", w.Code, w.Body)
	}
	var e struct {
		ID       string                `json:"id"`
		Revision int64                 `json:"revision"`
		State    string                `json:"state"`
		Links    struct{ Self string } `json:"links"`
	}
	json.Unmarshal(w.Body.Bytes(), &e)
	if e.State != "OPEN" || e.Links.Self != "/v1/projects/alpha/epics/"+e.ID {
		t.Fatalf("epic view: %s", w.Body)
	}
	if w := taskReq(t, f.h, "POST", "/v1/projects/alpha/epics", f.writer, "e2", `{"title":"nope"}`); w.Code != 403 {
		t.Fatalf("writer token created an epic: %d", w.Code)
	}
	if w := taskReq(t, f.h, "POST", "/v1/projects/alpha/epics", f.bob, "e3", `{"title":"nope"}`); w.Code != 403 {
		t.Fatalf("read-only token created an epic: %d", w.Code)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/epics/"+e.ID, f.bob, "", ""); w.Code != 200 {
		t.Fatalf("read by read-only token: %d", w.Code)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/epics/"+uuidv7.New(), f.bob, "", ""); w.Code != 404 {
		t.Fatalf("missing epic: %d", w.Code)
	}
	// A task under the epic, and the filter.
	w = taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, "t1", `{"title":"under","epic":"`+e.ID+`"}`)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"epic":"`+e.ID+`"`) {
		t.Fatalf("task under epic: %d %s", w.Code, w.Body)
	}
	task := decodeTask(t, w)
	taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, "t2", `{"title":"elsewhere"}`)
	w = taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks?epic="+e.ID, f.bob, "", "")
	if w.Code != 200 || strings.Count(w.Body.String(), `"id":"`) != 1 || !strings.Contains(w.Body.String(), `"epic":"`+e.ID+`"`) {
		t.Fatalf("epic filter: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks?epic=nope", f.bob, "", ""); w.Code != 400 {
		t.Fatalf("bad epic filter: %d", w.Code)
	}
	if w := taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, "t3", `{"title":"orphan","epic":"`+uuidv7.New()+`"}`); w.Code != 400 {
		t.Fatalf("missing epic on a task: %d %s", w.Code, w.Body)
	}
	// Stale revision conflicts with the current epic in the body.
	w = taskReq(t, f.h, "PUT", "/v1/projects/alpha/epics/"+e.ID, f.alice, "e4", `{"title":"release 0.5","state":"RETIRED","expected_revision":9}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"current"`) {
		t.Fatalf("stale epic update: %d %s", w.Code, w.Body)
	}
	// Retire; existing assignment survives; a new one is refused; the
	// list hides it unless asked.
	w = taskReq(t, f.h, "PUT", "/v1/projects/alpha/epics/"+e.ID, f.alice, "e5", `{"title":"release 0.5","state":"RETIRED","expected_revision":1}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"RETIRED"`) {
		t.Fatalf("retire: %d %s", w.Code, w.Body)
	}
	w = taskReq(t, f.h, "PUT", "/v1/tasks/"+task.ID, f.alice, "t1b", `{"title":"under (edited)","state":"READY","epic":"`+e.ID+`","expected_revision":1}`)
	if w.Code != 200 {
		t.Fatalf("existing assignment after retirement: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, "t4", `{"title":"late","epic":"`+e.ID+`"}`); w.Code != 400 {
		t.Fatalf("new assignment to a retired epic: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/epics", f.bob, "", ""); w.Code != 200 || strings.Contains(w.Body.String(), e.ID) {
		t.Fatalf("retired epic listed by default: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/epics?include_retired=true", f.bob, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), e.ID) {
		t.Fatalf("retired epic on request: %d %s", w.Code, w.Body)
	}
	// Tasks off: epic mutations refused, reads kept.
	if w := taskReq(t, f.h, "PUT", "/v1/projects/alpha/meta/tasks", f.admin, "", `{"value":"off"}`); w.Code != 200 {
		t.Fatalf("switch off: %d", w.Code)
	}
	if w := taskReq(t, f.h, "POST", "/v1/projects/alpha/epics", f.admin, "e6", `{"title":"while off"}`); w.Code != 403 {
		t.Fatalf("epic create with tasks off: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/epics?include_retired=true", f.alice, "", ""); w.Code != 200 {
		t.Fatalf("epic read with tasks off: %d", w.Code)
	}
}

// The identity directory returns exactly id, kind, name and enabled for
// every user and group, to any credential class, and nothing else.
func TestAccessDirectory(t *testing.T) {
	f := newTaskFixture(t)
	for _, tok := range []string{f.alice, f.bob, f.writer, f.admin} {
		w := taskReq(t, f.h, "GET", "/v1/access/directory", tok, "", "")
		if w.Code != 200 {
			t.Fatalf("directory for %q: %d %s", tok[:6], w.Code, w.Body)
		}
		var out struct {
			Identities []map[string]any `json:"identities"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Identities) < 2 {
			t.Fatalf("directory shape: %s", w.Body)
		}
		for _, id := range out.Identities {
			if len(id) != 4 || id["id"] == nil || id["kind"] == nil || id["name"] == nil || id["enabled"] == nil {
				t.Fatalf("directory entry must be exactly id/kind/name/enabled: %v", id)
			}
		}
		if !strings.Contains(w.Body.String(), `"name":"Alice"`) || strings.Contains(w.Body.String(), "token") || strings.Contains(w.Body.String(), "grant") {
			t.Fatalf("directory content: %s", w.Body)
		}
	}
}

// Over HTTP: typed references round-trip; a string array is refused with
// the reason; a wrong kind is refused.
func TestTypedReferencesOverHTTP(t *testing.T) {
	f := newTaskFixture(t)
	w := taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, "r1", `{"title":"typed","candidate_refs":[{"kind":"pr","ref":"https://example.com/org/repo/pull/7","note":"the candidate"}],"evidence_refs":[{"kind":"text","ref":"reviewed"}]}`)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"kind":"pr"`) || !strings.Contains(w.Body.String(), `"note":"the candidate"`) {
		t.Fatalf("typed create: %d %s", w.Code, w.Body)
	}
	w = taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, "r2", `{"title":"legacy","candidate_refs":["https://example.com/pull/7"]}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "objects {kind, ref") {
		t.Fatalf("legacy strings must be refused with the shape: %d %s", w.Code, w.Body)
	}
	w = taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, "r3", `{"title":"bad","evidence_refs":[{"kind":"pr","ref":"7"}]}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "http(s) URL") {
		t.Fatalf("bare pr number must be refused: %d %s", w.Code, w.Body)
	}
}
