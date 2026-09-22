package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The playbook templates under docs/examples/team are driven through the real
// validators and storage operations with fixture identifiers, so a template
// that drifts from the contract fails here rather than in a pilot.
const teamExamplesDir = "../../docs/examples/team"

type teamExamples struct {
	t    *testing.T
	used map[string]bool
	ids  map[string]string
}

// load reads one template, substitutes the identifier placeholders and decodes
// it strictly: an unknown field in a template is a drift, not a tolerance.
func (e *teamExamples) load(name string, v any) {
	e.t.Helper()
	raw, err := os.ReadFile(filepath.Join(teamExamplesDir, name))
	if err != nil {
		e.t.Fatal(err)
	}
	text := string(raw)
	for k, id := range e.ids {
		text = strings.ReplaceAll(text, "{{"+k+"}}", id)
	}
	if strings.Contains(text, "{{") {
		e.t.Fatalf("%s: unresolved placeholder in %s", name, text)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		e.t.Fatalf("%s: %v", name, err)
	}
	e.used[name] = true
}

func TestTeamPlaybookTemplatesAreAccepted(t *testing.T) {
	_, d, team, a, _, c := assignmentFixture(t)
	e := &teamExamples{t: t, used: map[string]bool{}, ids: map[string]string{"coordinator": c.TeamSessionHandle.SessionID, "worker": c.Worker.SessionID, "task": c.TaskID}}
	revision := func() int64 {
		t.Helper()
		task, err := d.GetTask(c.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		return task.Revision
	}

	// profile.json: a third member joins with the template profile; unknown stays unknown.
	var join struct {
		Role    string      `json:"role"`
		Profile TeamProfile `json:"profile"`
	}
	e.load("profile.json", &join)
	third, err := d.JoinTeam(team.ID, join.Role, join.Profile, a, "join-template")
	if err != nil || third.Role != "worker" || third.Model.Source != "unknown" || third.Model.ID != "unknown" || len(third.Capabilities) != 3 {
		t.Fatalf("profile template: %+v %v", third, err)
	}

	// offer.json, then decline.json with a reason, then a second offer and accept.json.
	var offer TeamOffer
	e.load("offer.json", &offer)
	offer.TeamSessionHandle, offer.CoordinatorGeneration, offer.ExpectedRevision = c.TeamSessionHandle, c.CoordinatorGeneration, revision()
	offer.Worker = c.Worker
	if offer.SuitabilityRationale == "" || offer.CostRationale == "" {
		t.Fatal("offer template lacks a rationale")
	}
	first, err := d.OfferTeamAssignment(team.ID, offer, a, "offer-1", allowAssignmentWorker)
	if err != nil {
		t.Fatal("offer template:", err)
	}
	e.ids["attempt"] = first.ID
	var decline struct {
		TeamAssignmentCommand
		Attempt string `json:"attempt"`
	}
	e.load("decline.json", &decline)
	if decline.Attempt != first.ID || decline.Reason == "" {
		t.Fatal("decline template", decline)
	}
	decline.TeamSessionHandle = c.Worker
	if got, err := d.ChangeTeamAssignment(team.ID, decline.Attempt, "decline", decline.TeamAssignmentCommand, a, "decline", allowAssignmentWorker); err != nil || got.State != "DECLINED" {
		t.Fatal("decline template:", got, err)
	}
	offer.ExpectedRevision = revision()
	second, err := d.OfferTeamAssignment(team.ID, offer, a, "offer-2", allowAssignmentWorker)
	if err != nil {
		t.Fatal(err)
	}
	e.ids["attempt"] = second.ID
	var accept struct {
		TeamAssignmentCommand
		Attempt string `json:"attempt"`
	}
	e.load("accept.json", &accept)
	accept.TeamSessionHandle = c.Worker
	if got, err := d.ChangeTeamAssignment(team.ID, accept.Attempt, "accept", accept.TeamAssignmentCommand, a, "accept", allowAssignmentWorker); err != nil || got.State != "RUNNING" {
		t.Fatal("accept template:", got, err)
	}

	// question-factual.json from the worker; the four answer shapes and the escalation.
	var question TeamMessageContent
	e.load("question-factual.json", &question)
	asked, err := d.SendTeamMessage(team.ID, c.Worker, question, a, "question", MaxTeamMessages)
	if err != nil || asked.Kind != "question" || asked.Payload.Deadline == "" || len(asked.Payload.Refs) < 2 {
		t.Fatalf("question template: %+v %v", asked, err)
	}
	e.ids["question"] = asked.ID
	for _, name := range []string{"answer-known.json", "answer-unknown.json", "answer-conflicting.json", "answer-expired.json"} {
		var answer TeamMessageContent
		e.load(name, &answer)
		sent, err := d.SendTeamMessage(team.ID, c.TeamSessionHandle, answer, a, name, MaxTeamMessages)
		if err != nil || sent.Kind != "answer" || sent.ReplyTo != asked.ID || len(sent.Payload.Refs) == 0 {
			t.Fatalf("%s: %+v %v", name, sent, err)
		}
		// Every answer states its sources and freshness in the text or refs.
		if !strings.Contains(sent.Payload.Text+refsText(sent.Payload.Refs), "observed 20") {
			t.Fatalf("%s: no observation time", name)
		}
	}
	var escalation TeamMessageContent
	e.load("escalation.json", &escalation)
	sent, err := d.SendTeamMessage(team.ID, c.TeamSessionHandle, escalation, a, "escalation", MaxTeamMessages)
	if err != nil || sent.Kind != "blocker" || sent.Recipient.Kind != "team" {
		t.Fatalf("escalation template: %+v %v", sent, err)
	}
	for _, part := range []string{"1. Question:", "2. Task:", "3. Attempted resolution:", "4. Evidence:", "5. Options:", "6. Impact:", "7. Exact pending request:"} {
		if !strings.Contains(sent.Payload.Text, part) {
			t.Fatal("escalation template lacks", part)
		}
	}

	// block.json, resume-work.json, submit.json, review.json on the running attempt.
	var work struct {
		TeamWorkCommand
		Attempt string `json:"attempt"`
	}
	for _, step := range []struct{ file, op, state string }{{"block.json", "block", "BLOCKED"}, {"resume-work.json", "resume-work", "RUNNING"}} {
		work = struct {
			TeamWorkCommand
			Attempt string `json:"attempt"`
		}{}
		e.load(step.file, &work)
		work.TeamSessionHandle, work.ExpectedRevision = c.Worker, revision()
		if got, err := d.ChangeTeamWork(team.ID, work.Attempt, step.op, work.TeamWorkCommand, a, step.op); err != nil || got.State != step.state {
			t.Fatalf("%s: %+v %v", step.file, got, err)
		}
	}
	var submit struct {
		TeamResultSubmission
		Attempt string `json:"attempt"`
	}
	e.load("submit.json", &submit)
	submit.TeamSessionHandle, submit.ExpectedRevision = c.Worker, revision()
	submitted, err := d.SubmitTeamResult(team.ID, submit.Attempt, submit.TeamResultSubmission, a, "submit")
	if err != nil || submitted.State != "SUBMITTED" || submitted.Result == nil || len(submitted.Result.EvidenceRefs) < 3 {
		t.Fatalf("submit template: %+v %v", submitted, err)
	}
	e.ids["result"] = submitted.Result.ID
	var review struct {
		TeamResultDecision
		Attempt string `json:"attempt"`
	}
	e.load("review.json", &review)
	review.TeamSessionHandle, review.CoordinatorGeneration, review.ExpectedRevision = c.TeamSessionHandle, c.CoordinatorGeneration, revision()
	if got, err := d.ReviewTeamResult(team.ID, review.Attempt, review.TeamResultDecision, a, "review"); err != nil || got.State != "ACCEPTED" {
		t.Fatalf("review template: %+v %v", got, err)
	}

	// Every template in the directory is exercised above; a new file must be wired in.
	entries, err := os.ReadDir(teamExamplesDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !e.used[entry.Name()] {
			t.Fatalf("template %s is not exercised by this test", entry.Name())
		}
	}
}

func refsText(refs []TaskRef) string {
	var b strings.Builder
	for _, r := range refs {
		b.WriteString(r.Ref)
		b.WriteString(r.Note)
	}
	return b.String()
}
