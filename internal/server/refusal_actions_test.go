package server

// Task 19f6: team-context refusals name the denied operation and the
// caller's own role, choose their one next action by (code, role, operation),
// and never advise another credential, a personal-mode retry or a bypass.

import (
	"encoding/json"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"aimem/internal/uuidv7"
)

// negated drops the clauses a next action forbids ("do not switch
// credentials", "never reuse the key"), which are not advice.
var negated = regexp.MustCompile(`(?i)\b(do not|don't|never)\b[^.;,]*`)

// substitutionAdvice is what a team-mode next action must never advise.
var substitutionAdvice = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bpersonal\b`),
	regexp.MustCompile(`(?i)\b(another|other|different|legacy|admin|writer|project-scoped)\s+(\w+\s+)?(credential|token|bearer)s?\b`),
	regexp.MustCompile(`(?i)\buse\b[^.;]*\b(credential|token|bearer)s?\b`),
	regexp.MustCompile(`(?i)\b(bypass|fall ?back|work around|circumvent|switch)\b`),
}

func advises(next string) string {
	rest := negated.ReplaceAllString(next, "")
	for _, re := range substitutionAdvice {
		if m := re.FindString(rest); m != "" {
			return m
		}
	}
	return ""
}

// Every next action a team-mode caller can receive (every code of both
// envelopes, with the team-mode and role overrides) is free of substitution
// or bypass advice.
func TestTeamNextActionsNeverAdviseSubstitution(t *testing.T) {
	// The detector itself: it catches advice and lets a negation through.
	for _, bad := range []string{"Retry in personal mode.", "Use your personal token.", "Use another credential.", "Bypass the gate with the admin token.",
		"Switch to the legacy writer token.", "Fall back to the project-scoped token."} {
		if advises(bad) == "" {
			t.Fatalf("the detector misses %q", bad)
		}
	}
	for _, fine := range []string{"Request an authorized grant change for the team; do not switch credentials.", "Begin the step again through aicrew; never reuse the proof or the key."} {
		if m := advises(fine); m != "" {
			t.Fatalf("the detector flags %q (%s)", fine, m)
		}
	}
	checked := 0
	check := func(where, next string) {
		t.Helper()
		checked++
		if m := advises(next); m != "" {
			t.Errorf("%s: %q advises %q", where, next, m)
		}
	}
	for code, ref := range identityRefusals {
		next := ref.next
		if o, ok := teamNextActions[code]; ok {
			next = o
		}
		check("identity "+code, next)
	}
	for code, spec := range reservationRefusals {
		check("reservation "+code, spec.next)
	}
	for k, next := range roleNextActions {
		check(k.code+"/"+k.role+"/"+k.action, next)
	}
	if checked < 40 {
		t.Fatalf("checked only %d next actions", checked)
	}
}

func decodeRefusal(t *testing.T, r identityResp) identityRefusalBody {
	t.Helper()
	var e identityRefusalBody
	if err := json.Unmarshal(r.body, &e); err != nil || e.Code == "" || e.CorrelationID == "" || e.NextAction == "" {
		t.Fatalf("not a refusal envelope: %d %s", r.status, r.body)
	}
	return e
}

// The worker's direct claim, the independent member's claim without a
// proof and the coordinator's update each get their role's next action, the
// denied operation and the caller's own role, through the real routes.
func TestTeamRefusalsNameTheActionAndTheRole(t *testing.T) {
	g := newReservationRig(t)
	task := g.readyTask(t, g.alpha)
	claim := body(t, map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "external", "work_ref": "w-1"}})
	update := body(t, map[string]any{"expected_revision": task.Revision, "reservation_id": uuidv7.New(), "fence": "1",
		"content": map[string]any{"title": task.Title, "state": "IN_PROGRESS"}})
	for _, c := range []struct {
		name, role, suffix, body, action, next string
	}{
		{"a worker's direct claim", "worker", "/claim", claim, "reservation.claim", roleNextActions[nextActionKey{"role_forbidden", "worker", "reservation.claim"}]},
		{"an independent claim without a proof", "independent", "/claim", claim, "reservation.claim", roleNextActions[nextActionKey{"role_forbidden", "independent", "reservation.claim"}]},
		{"a coordinator's update", "coordinator", "/update", update, "reservation.update", roleNextActions[nextActionKey{"role_forbidden", "coordinator", "reservation.update"}]},
	} {
		r := g.call(t, g.tls, "POST", g.rpath(task.ID, c.suffix), g.alice, rhdr("k-"+uuidv7.New(), g.teamAs(t, c.role, "agent-"+c.role, "sess-"+c.role)), c.body, true)
		e := decodeRefusal(t, r)
		if r.status != 403 || e.Code != "role_forbidden" || e.ActiveMode != "team" || e.ActiveRole != c.role || e.DeniedAction != c.action || e.NextAction != c.next || c.next == "" {
			t.Fatalf("%s: %d %s", c.name, r.status, r.body)
		}
	}
	// The worker's claim tells it to check its aicrew inbox.
	if !strings.Contains(roleNextActions[nextActionKey{"role_forbidden", "worker", "reservation.claim"}], "aicrew inbox") {
		t.Fatal("a worker's claim does not point at the aicrew inbox")
	}
	// A personal-only operation in team mode is refused at the gate, before
	// aicrew vouches for a role: its route, no role, the code's next action.
	r := g.call(t, g.tls, "POST", "/v1/projects/alpha/tasks", g.alice, g.teamAs(t, "worker", "agent-worker", "sess-w"), `{"title":"x"}`, true)
	if e := decodeRefusal(t, r); e.Code != "team_operation_unsupported" || e.DeniedAction != "POST /v1/projects/{p}/tasks" || e.ActiveRole != "" ||
		e.NextAction != identityRefusals["team_operation_unsupported"].next {
		t.Fatalf("a personal-only operation in team mode: %d %s", r.status, r.body)
	}
	// A verified context without a grant on the project: its role and the
	// read's route, never the project's contents.
	beta, err := g.s.reg.Open("beta")
	if err != nil {
		t.Fatal(err)
	}
	other := g.readyTask(t, beta)
	r = g.call(t, g.tls, "GET", "/v1/tasks/"+other.ID, g.alice, g.teamAs(t, "worker", "agent-worker", "sess-w"), "", true)
	if e := decodeRefusal(t, r); e.Code != "grant_denied" || e.ActiveRole != "worker" || e.DeniedAction != "GET /v1/tasks/{id}" ||
		strings.Contains(string(r.body), other.Title) {
		t.Fatalf("grant denied: %d %s", r.status, r.body)
	}
	// A project-scoped token in team mode is told to start through aicrew,
	// not to use another credential.
	r = g.call(t, g.tls, "GET", "/v1/tasks/"+task.ID, g.project, g.teamAs(t, "worker", "agent-worker", "sess-w"), "", true)
	if e := decodeRefusal(t, r); e.Code != "credential_scope_forbidden" || e.NextAction != teamNextActions["credential_scope_forbidden"] {
		t.Fatalf("a project-scoped token in team mode: %d %s", r.status, r.body)
	}
	// A personal refusal names the operation, no role, and keeps the code's
	// next action.
	r = g.call(t, g.tls, "POST", g.rpath(task.ID, "/transfer"), g.alice, rhdr("k-transfer", nil), body(t, map[string]any{
		"expected_revision": task.Revision, "reservation_id": uuidv7.New(), "fence": "1", "holder": map[string]any{"mode": "external", "work_ref": "w"}}), true)
	if e := decodeRefusal(t, r); e.Code != "role_forbidden" || e.ActiveMode != "personal" || e.ActiveRole != "" || e.DeniedAction != "reservation.transfer" ||
		e.NextAction != reservationRefusals["role_forbidden"].next {
		t.Fatalf("a personal transfer: %d %s", r.status, r.body)
	}
	g.assertNoSecretLeak(t)
}

// Every member reservation route names its operation.
func TestReservationActionNames(t *testing.T) {
	base := "/v1/projects/alpha/tasks/" + uuidv7.New() + "/reservation"
	for path, want := range map[string]string{
		base: "reservation.status", base + "/claim": "reservation.claim", base + "/transfer": "reservation.transfer",
		base + "/update": "reservation.update", base + "/release": "reservation.release", base + "/finalize": "reservation.finalize",
		base + "/receipts/claim/some-key": "reservation.receipt", "/v1/tasks/x": "",
	} {
		if got := reservationAction(httptest.NewRequest("GET", path, nil)); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}
}
