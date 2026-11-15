package server

// Task 01a1dd8c-4919 made personal mode answer an unreadable project as an
// unknown one and kept team mode's answers. These tests pin team mode for a
// member who holds the team profile's grant and no personal grant, the
// case in which the two modes would differ.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withoutPersonalGrant removes Alice's personal grant on alpha; the team
// profile's grant on alpha stays.
func (g *reservationRig) withoutPersonalGrant(t *testing.T) {
	t.Helper()
	if err := g.db.SetGrant("admin", g.alphaInstance, "user", g.aliceID, false); err != nil {
		t.Fatal(err)
	}
	if r := g.call(t, g.tls, "GET", "/v1/projects/alpha/tasks", g.alice, nil, "", true); r.status != 404 {
		t.Fatalf("Alice still reads alpha in personal mode: %d %s", r.status, r.body)
	}
}

// A team claim, triage and comment by such a member are decided by the
// profile's grant; the personal-mode hiding never applies.
func TestTeamWritesNeedNoPersonalGrant(t *testing.T) {
	g := newReservationRig(t)
	g.selectProcess(t, testPin)
	g.withoutPersonalGrant(t)
	coord := func() map[string]string { return g.teamAs(t, "coordinator", "agent-coord", "sess-c") }

	task := g.backlogTask(t)
	triaged := g.triage(t, g.alice, coord(), task.ID, map[string]any{"expected_revision": task.Revision, "state": "READY"})
	if triaged.status != 200 {
		t.Fatalf("team triage: %d %s", triaged.status, triaged.body)
	}
	if r := g.call(t, g.tls, "POST", "/v1/tasks/"+task.ID+"/comments", g.alice, merge(coord(), map[string]string{"Idempotency-Key": "guard-c"}), `{"body":"assessed"}`, true); r.status != 201 {
		t.Fatalf("team comment: %d %s", r.status, r.body)
	}

	ready := g.readyTask(t, g.alpha)
	f := g.factAs("coordinator", "agent-coord", "sess-c", "offer", "claim", ready.ID, "guard-offer")
	f["offer_ref"], f["process"] = "offer-g", pinOf(testPin)
	f["intended_worker"] = map[string]any{"user_id": g.aliceID, "agent_id": "agent-worker"}
	g.answerFact(f)
	proof := testProof(t)
	g.secrets = append(g.secrets, proof)
	claim := body(t, map[string]any{"expected_revision": ready.Revision, "holder": map[string]any{"mode": "external", "work_ref": "offer-g"}, "coordination_proof": proof})
	o := decodeOutcome(t, g.call(t, g.tls, "POST", g.rpath(ready.ID, "/claim"), g.alice, rhdr("guard-offer", coord()), claim, true))
	if o.Receipt.VerifiedMode != "team" {
		t.Fatalf("team claim: %+v", o)
	}
	for _, suffix := range []string{"", "/receipts/claim/guard-offer"} {
		if r := g.call(t, g.tls, "GET", g.rpath(ready.ID, suffix), g.alice, rhdr("", coord()), "", true); r.status != 200 {
			t.Fatalf("team read %q: %d %s", suffix, r.status, r.body)
		}
	}
	g.assertNoSecretLeak(t)
}

// Every reservation route, in a team conversation by a role with a path
// for it, on a project the profile is not granted: grant_denied, as
// before the task (the same answers at the base commit).
func TestTeamReservationRoutesOnAnUngrantedProject(t *testing.T) {
	g := newReservationRig(t)
	g.withoutPersonalGrant(t)
	beta, err := g.s.reg.Open("beta")
	if err != nil {
		t.Fatal(err)
	}
	task := g.readyTask(t, beta)
	bodies := map[string]string{
		"claim":    body(t, map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "external", "work_ref": "w"}, "coordination_proof": "p"}),
		"transfer": body(t, map[string]any{"expected_revision": task.Revision, "reservation_id": "r", "fence": "1", "holder": map[string]any{"mode": "external", "work_ref": "w"}, "coordination_proof": "p"}),
		"update":   body(t, map[string]any{"expected_revision": task.Revision, "reservation_id": "r", "fence": "1", "content": map[string]any{"title": "t", "state": "IN_PROGRESS"}}),
		"release":  body(t, map[string]any{"expected_revision": task.Revision, "reservation_id": "r", "fence": "1", "reason": "done", "coordination_proof": "p"}),
		"finalize": body(t, map[string]any{"expected_revision": task.Revision, "reservation_id": "r", "fence": "1", "content": map[string]any{"title": "t", "state": "DONE"}, "terminal_evidence": []string{"e"}, "coordination_proof": "p"}),
	}
	for _, pattern := range reservationRoutePatterns {
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.NewReplacer("{p}", "beta", "{task_id}", task.ID, "{operation}", "claim", "{request_key}", "k").Replace(path)
		op := path[strings.LastIndex(path, "/")+1:]
		role := "worker"
		if op == "claim" {
			role = "coordinator"
		}
		r := g.call(t, g.tls, method, path, g.alice, rhdr("guard-"+op, g.teamAs(t, role, "agent-"+role, "sess-"+role)), bodies[op], true)
		wantRefusal(t, "team "+pattern, r, 403, "grant_denied", "team")
	}
}

// authorizeTaskWrite's team branch: no team route reaches it today (team
// triage and comments decide through authorizeTriageWrite, and every other
// team write is refused at the gate), so it is pinned directly. A team
// context is decided by the personal write decision with the old 403s,
// never by the personal hiding.
func TestAuthorizeTaskWriteTeamBranch(t *testing.T) {
	g := newReservationRig(t)
	g.withoutPersonalGrant(t)
	ctx := g.team(t, g.aliceIdentity, nil)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/projects/alpha/tasks", nil).WithContext(ctx)
	if g.s.authorizeTaskWrite(w, r, "alpha") {
		t.Fatal("a team write without a personal write grant was authorized")
	}
	if w.Code != 403 || !strings.Contains(w.Body.String(), "does not permit writes to this project") {
		t.Fatalf("team branch: %d %s", w.Code, w.Body)
	}
}
