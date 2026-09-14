package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aimem/internal/store"
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
		if _, err := reg.Open(p); err != nil {
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
	if w.Code != 201 {
		t.Fatalf("alice create: %d %s", w.Code, w.Body)
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
	json.Unmarshal(w.Body.Bytes(), &hist)
	if len(hist.Changes) != 2 || hist.Changes[1].Actor.Kind != "admin" || hist.Changes[1].Actor.Name != "env" {
		t.Fatalf("admin actor stamp: %+v", hist.Changes)
	}

	// Ordinary tokens stay out of the legacy surface.
	if w := taskReq(t, h, "GET", "/v1/projects/alpha/docs", f.alice, "", ""); w.Code != 403 {
		t.Fatalf("ordinary token on legacy route: %d", w.Code)
	}
	if w := taskReq(t, h, "GET", "/v1/projects/alpha/tasks/../docs", f.alice, "", ""); w.Code == 200 {
		t.Fatalf("path games must not reach legacy routes: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "GET", "/v1/access/identity?project=alpha", f.alice, "", ""); w.Code != 200 {
		t.Fatalf("identity: %d", w.Code)
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
	if got := decodeTask(t, taskReq(t, h, "GET", "/v1/tasks/"+task.ID, f.alice, "", "")); got.Revision != 2 {
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
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"next_cursor"`) {
		t.Fatalf("comments page: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "GET", "/v1/tasks/"+task.ID+"/comments?limit=500", f.bob, "", ""); w.Code != 400 {
		t.Fatalf("limit bound: %d", w.Code)
	}
	// Archive, then a new comment is refused while the replay still works.
	w = taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u6", `{"title":"v2","state":"DONE","archived":true,"expected_revision":2}`)
	if w.Code != 200 {
		t.Fatalf("archive: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "POST", "/v1/tasks/"+task.ID+"/comments", f.alice, "c2", `{"body":"late"}`); w.Code != 409 {
		t.Fatalf("comment on archived: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "POST", "/v1/tasks/"+task.ID+"/comments", f.alice, "c1", `{"body":"first **note**"}`); w.Code != 201 {
		t.Fatalf("replay after archive: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, h, "GET", "/v1/projects/alpha/tasks", f.bob, "", ""); strings.Contains(w.Body.String(), task.ID) {
		t.Fatal("archived task listed by default")
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

// The local unix socket carries no identity: the operator's CLI works and
// is stamped "local".
func TestTaskRoutesLocalSocketIsOperator(t *testing.T) {
	s, reg := testServer(t)
	if _, err := reg.Open("alpha"); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
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
	r := httptest.NewRequest("POST", "/mcp", nil).WithContext(withIdentity(httptest.NewRequest("POST", "/mcp", nil).Context(), id))
	call, tasksOnly := f.s.MCPPrincipal(r)
	if !tasksOnly {
		t.Fatal("ordinary token must be tasks-only")
	}
	status, body, err := call(r.Context(), "POST", "/v1/projects/alpha/tasks", map[string]string{"Idempotency-Key": "m1"}, []byte(taskBody))
	if err != nil || status != 201 {
		t.Fatalf("dispatch create: %d %v %s", status, err, body)
	}
	var task taskResponse
	json.Unmarshal(body, &task)
	status, body, _ = call(r.Context(), "POST", "/v1/projects/beta/tasks", map[string]string{"Idempotency-Key": "m2"}, []byte(taskBody))
	if status != 403 {
		t.Fatalf("dispatch is bound to alice's authority: %d %s", status, body)
	}
	if _, _, err := call(r.Context(), "GET", "/v1/projects/alpha/docs", nil, nil); err == nil {
		t.Fatal("non-task route must not be dispatchable")
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
	_ = fmt.Sprint(task.ID)
}
