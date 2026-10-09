package server

// aicrewd's board.read feed (aicrew's control-plane design, A0), served over
// the real TLS listener with real peer credentials.

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"aimem/internal/store"
)

type boardRig struct {
	*teamRegRig
	board, boardID string
	tasks          map[string]string // title -> task ID
}

func newBoardRig(t *testing.T) *boardRig {
	t.Helper()
	g := &boardRig{teamRegRig: newTeamRegRig(t), tasks: map[string]string{}}
	g.boardID, g.board = g.issueCredentialFor(t, "aicrew-example", "board.read", time.Now().Add(24*time.Hour))
	for team, name := range map[string]string{teamA: "pilot", teamB: "reviewers"} {
		if r := g.reg(t, g.register, "aicrew-example", team, name); r.status != 200 {
			t.Fatalf("register %s: %d %s", name, r.status, r.body)
		}
	}
	for _, p := range []string{"beta", "gamma", "delta"} {
		if _, err := g.s.reg.Open(p); err != nil {
			t.Fatal(err)
		}
	}
	// teamA: alpha and beta; teamB: beta again and gamma. delta is granted
	// only to the other peer's team.
	for _, gr := range [][2]string{{teamA, "alpha"}, {teamA, "beta"}, {teamB, "beta"}, {teamB, "gamma"}} {
		g.grant(t, gr[0], gr[1], true)
	}
	if r := g.admin(t, "PUT", "/v1/identity/peers/other-peer/teams/"+teamTheirs+"/grants/delta", ""); r.status != 200 {
		t.Fatalf("grant delta: %d %s", r.status, r.body)
	}
	return g
}

func (g *boardRig) grant(t *testing.T, team, project string, on bool) {
	t.Helper()
	method := "PUT"
	if !on {
		method = "DELETE"
	}
	if r := g.admin(t, method, "/v1/identity/peers/aicrew-example/teams/"+team+"/grants/"+project, ""); r.status != 200 {
		t.Fatalf("%s grant %s %s: %d %s", method, team, project, r.status, r.body)
	}
}

var boardActor = store.TaskActor{Kind: "admin", Name: "host-admin"}

// task creates a task in project, or moves an existing one to state.
func (g *boardRig) task(t *testing.T, project, title, state string) {
	t.Helper()
	db, err := g.s.reg.OpenExisting(project)
	if err != nil {
		t.Fatal(err)
	}
	c := store.TaskContent{Title: title, State: state, Objective: "secret objective " + title}
	id, ok := g.tasks[title]
	if !ok {
		task, err := db.CreateTask(c, boardActor, "create-"+title)
		if err != nil {
			t.Fatal(err)
		}
		g.tasks[title] = task.ID
		return
	}
	cur, err := db.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateTask(id, c, cur.Revision, boardActor, "update-"+title+"-"+state); err != nil {
		t.Fatal(err)
	}
}

func (g *boardRig) read(t *testing.T, bearer, query string) identityResp {
	t.Helper()
	return g.call(t, g.tls, "GET", "/v1/identity/peers/aicrew-example/board-changes"+query, bearer, v1, "", true)
}

func (g *boardRig) page(t *testing.T, cursor string, limit int) boardReadView {
	t.Helper()
	q := "?limit=" + strconv.Itoa(limit)
	if cursor != "" {
		q += "&cursor=" + url.QueryEscape(cursor)
	}
	r := g.read(t, g.board, q)
	var v boardReadView
	if r.status != 200 || json.Unmarshal(r.body, &v) != nil {
		t.Fatalf("read %s: %d %s", q, r.status, r.body)
	}
	if strings.Contains(string(r.body), "secret objective") || strings.Contains(string(r.body), `"title"`) {
		t.Fatalf("the feed carries task content: %s", r.body)
	}
	return v
}

// drain reads from cursor until more is false and returns every change and
// the final cursor.
func (g *boardRig) drain(t *testing.T, cursor string, limit int) ([]boardChangeView, string) {
	t.Helper()
	var all []boardChangeView
	for i := 0; ; i++ {
		v := g.page(t, cursor, limit)
		if again := g.page(t, cursor, limit); !equalPages(v, again) {
			t.Fatalf("the same cursor gave %+v, then %+v", v, again)
		}
		all = append(all, v.Changes...)
		if !v.More {
			return all, v.Cursor
		}
		if len(v.Changes) == 0 || i > 50 {
			t.Fatalf("more without progress: %+v", v)
		}
		cursor = v.Cursor
	}
}

func equalPages(a, b boardReadView) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func changeKeys(cs []boardChangeView, g *boardRig) []string {
	byID := map[string]string{}
	for title, id := range g.tasks {
		byID[id] = title
	}
	var out []string
	for _, c := range cs {
		out = append(out, c.Project+"/"+byID[c.TaskID]+":"+c.From+">"+c.To)
	}
	return out
}

func TestBoardRead(t *testing.T) {
	g := newBoardRig(t)
	g.task(t, "alpha", "a1", "BACKLOG")
	g.task(t, "alpha", "a1", "READY")
	g.task(t, "alpha", "a2", "READY")
	g.task(t, "beta", "b1", "READY")
	g.task(t, "beta", "b1", "READY") // an edit that keeps the state
	g.task(t, "gamma", "c1", "BACKLOG")
	g.task(t, "delta", "d1", "READY") // granted only to another peer's team

	all, cursor := g.drain(t, "", 2)
	got := strings.Join(changeKeys(all, g), " ")
	want := "alpha/a1:>BACKLOG alpha/a1:BACKLOG>READY alpha/a2:>READY beta/b1:>READY gamma/c1:>BACKLOG"
	if got != want {
		t.Fatalf("feed:\n got %s\nwant %s", got, want)
	}
	if c := all[1]; c.Revision != 2 || c.At == "" || c.TaskID != g.tasks["a1"] {
		t.Fatalf("entry: %+v", c)
	}
	// Nothing new: an empty page with the same cursor.
	if v := g.page(t, cursor, 2); len(v.Changes) != 0 || v.More || v.Cursor != cursor {
		t.Fatalf("idle read: %+v", v)
	}
	// One read covers all teams; new changes appear after the cursor.
	g.task(t, "gamma", "c1", "READY")
	g.task(t, "alpha", "a1", "IN_PROGRESS")
	all, cursor = g.drain(t, cursor, 100)
	if got := strings.Join(changeKeys(all, g), " "); got != "alpha/a1:READY>IN_PROGRESS gamma/c1:BACKLOG>READY" {
		t.Fatalf("after the cursor: %s", got)
	}

	// A disabled team's grants leave the feed; beta stays through teamA.
	if r := g.admin(t, "PUT", "/v1/identity/peers/aicrew-example/teams/"+teamB, `{"disabled":true}`); r.status != 200 {
		t.Fatalf("disable: %d %s", r.status, r.body)
	}
	g.task(t, "gamma", "c1", "DONE")
	g.task(t, "beta", "b1", "DONE")
	all, cursor = g.drain(t, cursor, 100)
	if got := strings.Join(changeKeys(all, g), " "); got != "beta/b1:READY>DONE" {
		t.Fatalf("with teamB disabled: %s", got)
	}
	// A revoked grant stops the project's feed; given back, the feed
	// resumes where the peer stopped.
	g.grant(t, teamA, "alpha", false)
	g.task(t, "alpha", "a2", "BLOCKED")
	all, cursor = g.drain(t, cursor, 100)
	if len(all) != 0 {
		t.Fatalf("a revoked project is still read: %v", changeKeys(all, g))
	}
	g.grant(t, teamA, "alpha", true)
	all, _ = g.drain(t, cursor, 100)
	if got := strings.Join(changeKeys(all, g), " "); got != "alpha/a2:READY>BLOCKED" {
		t.Fatalf("after the grant came back: %s", got)
	}
	g.auditHas(t, "board.read", "peer:aicrew-example", "service=aicrew-example")
	g.assertNoSecretLeak(t)
}

func TestBoardReadRefusals(t *testing.T) {
	g := newBoardRig(t)
	g.task(t, "alpha", "a1", "READY")
	inst, err := g.s.reg.ExistingProjectAccessID("alpha")
	if err != nil {
		t.Fatal(err)
	}
	const actor = "peer:aicrew-example"
	for _, q := range []string{"?limit=0", "?limit=501", "?limit=x"} {
		if r := g.read(t, g.board, q); r.status != 400 || r.code() != "invalid_request" {
			t.Fatalf("%s: %d %s", q, r.status, r.body)
		}
	}
	g.auditHas(t, "board.read.refused.invalid_request", actor, "credential="+g.boardID)
	for name, cursor := range map[string]string{
		"garbage":       "b1.not-base64!",
		"no prefix":     boardCursor{Service: "aicrew-example"}.encode()[3:],
		"another peer":  boardCursor{Service: "other-peer", Positions: map[string]int64{inst: 1}}.encode(),
		"zero position": boardCursor{Service: "aicrew-example", Positions: map[string]int64{inst: 0}}.encode(),
	} {
		if r := g.read(t, g.board, "?cursor="+url.QueryEscape(cursor)); r.status != 400 || r.code() != "invalid_cursor" {
			t.Fatalf("%s cursor: %d %s", name, r.status, r.body)
		}
	}
	g.auditHas(t, "board.read.refused.invalid_cursor", actor, "credential="+g.boardID)
	ahead := boardCursor{Service: "aicrew-example", Positions: map[string]int64{inst: 99}}.encode()
	if r := g.read(t, g.board, "?cursor="+url.QueryEscape(ahead)); r.status != 409 || r.code() != "cursor_ahead" {
		t.Fatalf("a cursor past the feed: %d %s", r.status, r.body)
	}
	g.auditHas(t, "board.read.refused.cursor_ahead", actor, "credential="+g.boardID)
	// Only a board.read credential of the path's own peer reads, and it
	// reaches nothing else.
	for name, bearer := range map[string]string{"team.read": g.teamRegRig.read, "team.register": g.register, "identity.redeem": g.redeem} {
		if r := g.read(t, bearer, ""); r.status != 403 || r.code() != "peer_forbidden" {
			t.Fatalf("%s credential: %d %s", name, r.status, r.body)
		}
	}
	g.auditHas(t, "board.read.refused.peer_forbidden", actor, "credential="+g.readID)
	if r := g.call(t, g.tls, "GET", "/v1/identity/peers/other-peer/board-changes", g.board, v1, "", true); r.status != 403 || r.code() != "peer_forbidden" {
		t.Fatalf("another peer's path: %d %s", r.status, r.body)
	}
	for _, path := range []string{"/v1/identity/peers/aicrew-example/team-reads", "/v1/projects/alpha/tasks", "/v1/identity/peers/aicrew-example/teams"} {
		if r := g.call(t, g.tls, "GET", path, g.board, v1, "", true); r.status != 403 {
			t.Fatalf("board credential on %s: %d %s", path, r.status, r.body)
		}
	}
	if r := g.read(t, g.alice, ""); r.status != 401 || r.code() != "peer_unauthenticated" {
		t.Fatalf("individual bearer: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.plain, "GET", "/v1/identity/peers/aicrew-example/board-changes", g.board, v1, "", true); r.status != 403 || r.code() != "tls_required" {
		t.Fatalf("plain HTTP: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "GET", "/v1/identity/peers/aicrew-example/board-changes", g.board, nil, "", true); r.status != 400 || r.code() != "unsupported_version" {
		t.Fatalf("no version: %d %s", r.status, r.body)
	}
	// The read scope's per-credential bound.
	now := time.Now()
	g.s.readLimit = &readScopeLimiter{now: func() time.Time { return now }}
	for i := 0; i < readScopePerMinute; i++ {
		if r := g.read(t, g.board, ""); r.status != 200 {
			t.Fatalf("read %d: %d %s", i, r.status, r.body)
		}
	}
	if r := g.read(t, g.board, ""); r.status != 429 || r.code() != "rate_limited" {
		t.Fatalf("over the bound: %d %s", r.status, r.body)
	}
	g.assertNoSecretLeak(t)
}

// Each entry carries the task's required capability at the change's
// revision; an empty one means the project's own.
func TestBoardReadCarriesTheRequiredCapability(t *testing.T) {
	g := newBoardRig(t)
	db, err := g.s.reg.OpenExisting("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTask(store.TaskContent{Title: "net", State: "READY", RequiredCapability: "ops: network-x"}, boardActor, "cap-1"); err != nil {
		t.Fatal(err)
	}
	g.task(t, "alpha", "plain", "READY")
	v := g.page(t, "", 10)
	if len(v.Changes) != 2 || v.Changes[0].RequiredCapability != "ops: network-x" || v.Changes[1].RequiredCapability != "" {
		t.Fatalf("feed: %+v", v.Changes)
	}
	r := g.read(t, g.board, "")
	if !strings.Contains(string(r.body), `"required_capability":""`) {
		t.Fatalf("an empty capability is not stated: %s", r.body)
	}
}
