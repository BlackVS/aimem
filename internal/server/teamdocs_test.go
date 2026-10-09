package server

// A member's report as a hub document (aicrew's control-plane design, A0c)
// through the real routes and the fake aicrew's verified contexts.

import (
	"encoding/json"
	"strings"
	"testing"
)

func (g *reservationRig) putReport(t *testing.T, team map[string]string, project, name string, b map[string]any) identityResp {
	t.Helper()
	return g.call(t, g.tls, "PUT", "/v1/projects/"+project+"/docs/"+name, g.alice, team, body(t, b), true)
}

func TestTeamReportDocumentWrite(t *testing.T) {
	g := newReservationRig(t)
	alpha := g.alpha
	as := func(role string) map[string]string { return g.teamAs(t, role, "agent-"+role, "sess-"+role) }
	actor := "user:" + g.aliceID

	// The report says which documents team mode writes.
	var report map[string]any
	if r := g.call(t, g.tls, "GET", "/v1/access/identity", g.alice, as("worker"), "", true); r.status != 200 || json.Unmarshal(r.body, &report) != nil || report["doc_write"] != "report-*" {
		t.Fatalf("context report: %d %s", r.status, r.body)
	}

	// A worker creates a report; the revision names the member, the team
	// and the role, then the client's label, and the write is audited.
	r := g.putReport(t, as("worker"), "alpha", "report-task-1", map[string]any{"body": "# Report\nDone.\n", "base_rev": 0, "updated_by": "aicrew"})
	if r.status != 200 {
		t.Fatalf("worker report: %d %s", r.status, r.body)
	}
	doc, err := alpha.GetDoc("report-task-1", 0)
	if err != nil || doc.Rev != 1 || doc.Body != "# Report\nDone.\n" || doc.UpdatedBy != "Alice/team:team-1/worker/aicrew" {
		t.Fatalf("written report: %+v %v", doc, err)
	}
	g.auditHas(t, "team.doc.write", actor, `project="alpha"`, `doc="report-task-1"`, "rev=1", "team=team-1", "role=worker")

	// Every member role may write; the revisions are compare-and-swap.
	for i, role := range []string{"coordinator", "independent"} {
		r := g.putReport(t, as(role), "alpha", "report-task-1", map[string]any{"body": "revised by " + role, "base_rev": i + 1})
		if r.status != 200 {
			t.Fatalf("%s report: %d %s", role, r.status, r.body)
		}
	}
	if doc, _ := alpha.GetDoc("report-task-1", 0); doc.Rev != 3 || doc.UpdatedBy != "Alice/team:team-1/independent" {
		t.Fatalf("after two more writes: %+v", doc)
	}
	if r := g.putReport(t, as("worker"), "alpha", "report-task-1", map[string]any{"body": "stale", "base_rev": 1}); r.status != 409 {
		t.Fatalf("a stale base_rev: %d %s", r.status, r.body)
	}

	// Only report- documents: the project's other documents stay out of reach.
	if _, err := alpha.PutDoc("RUNBOOK", "keep me", "admin", 0, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"RUNBOOK", "reports-x", "Report-x"} {
		r := g.putReport(t, as("coordinator"), "alpha", name, map[string]any{"body": "overwritten", "base_rev": 1})
		wantRefusal(t, "document "+name, r, 400, "invalid_request", "team")
	}
	if doc, _ := alpha.GetDoc("RUNBOOK", 0); doc.Rev != 1 || doc.Body != "keep me" {
		t.Fatalf("a refused write changed RUNBOOK: %+v", doc)
	}
	g.auditHas(t, "team.refused.invalid_request", actor, `doc="RUNBOOK"`, "reason=report_name")

	// Only under the profile's grant: an ungranted, unknown or reserved
	// project is grant_denied, and nothing is written.
	for _, p := range []string{"beta", "no-such-project", "user", "group-x"} {
		r := g.putReport(t, as("worker"), p, "report-task-1", map[string]any{"body": "x", "base_rev": 0})
		wantRefusal(t, "project "+p, r, 403, "grant_denied", "team")
	}
	beta, err := g.s.reg.OpenExisting("beta")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := beta.GetDoc("report-task-1", 0); err == nil {
		t.Fatal("a refused write created a document in beta")
	}
	if _, err := g.s.reg.OpenExisting("no-such-project"); err == nil {
		t.Fatal("a refused write created the unknown project")
	}
	if r := g.putReport(t, as("worker"), "no-such-project", "report-x", map[string]any{"body": "x", "base_rev": 0}); strings.Contains(string(r.body), "no-such-project") {
		t.Fatalf("the refusal names the project: %s", r.body)
	}

	// Deleting, merging and the log stay off in team mode.
	for _, rq := range []struct{ method, path, body string }{
		{"DELETE", "/v1/projects/alpha/docs/report-task-1?base_rev=3", ""},
		{"POST", "/v1/projects/alpha/docs/report-task-1/merge", `{"body":"x","base_rev":3}`},
		{"GET", "/v1/projects/alpha/docs/report-task-1/log", ""},
	} {
		r := g.call(t, g.tls, rq.method, rq.path, g.alice, as("coordinator"), rq.body, true)
		wantRefusal(t, rq.method+" "+rq.path, r, 403, "team_operation_unsupported", "team")
	}
	if doc, _ := alpha.GetDoc("report-task-1", 0); doc.Rev != 3 || doc.Deleted {
		t.Fatalf("a refused delete changed the report: %+v", doc)
	}

	// Personal mode is unchanged: an ordinary token still writes no document.
	if r := g.call(t, g.tls, "PUT", "/v1/projects/alpha/docs/report-personal", g.alice, nil, `{"body":"x","base_rev":0}`, true); r.status != 403 {
		t.Fatalf("an ordinary token wrote a document: %d %s", r.status, r.body)
	}
	g.assertNoSecretLeak(t)
}
