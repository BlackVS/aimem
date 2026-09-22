package store

import (
	"errors"
	"reflect"
	"testing"
)

// inboxOf reads and delivers the unacknowledged inbox of a session.
func inboxOf(t *testing.T, d *DB, team Team, a TeamAuditContext, h TeamSessionHandle) []TeamMessage {
	t.Helper()
	page, err := d.TeamMessages(team.ID, h, 0, 100, true, a)
	if err != nil {
		t.Fatal(err)
	}
	return page.Messages
}

func lifecycleOnly(msgs []TeamMessage) []TeamMessage {
	out := []TeamMessage{}
	for _, m := range msgs {
		if m.Kind == "lifecycle" {
			out = append(out, m)
		}
	}
	return out
}

func TestLifecycleMessagesReachCounterparts(t *testing.T) {
	_, d, team, a, admin, c := assignmentFixture(t)
	coordinator, worker := c.TeamSessionHandle, c.Worker
	offer := mustOffer(t, d, team, a, c, "offer")
	// Offer: worker only, with the acting coordinator handle and no sender session.
	got := inboxOf(t, d, team, a, worker)
	if len(got) != 1 || got[0].Kind != "lifecycle" || got[0].Lifecycle == nil || got[0].Lifecycle.Operation != "team.assignment.offer" || got[0].Lifecycle.AttemptID != offer.ID || got[0].Lifecycle.State != "OFFERED" || got[0].Lifecycle.TaskState != "READY" || got[0].Lifecycle.ActorKind != "user" || got[0].Lifecycle.Session == nil || *got[0].Lifecycle.Session != coordinator || got[0].SenderID != "" || got[0].Recipient.Kind != "participants" || got[0].TaskID != c.TaskID || got[0].AttemptID != offer.ID {
		t.Fatalf("offer lifecycle: %+v", got)
	}
	if extra := inboxOf(t, d, team, a, coordinator); len(extra) != 0 {
		t.Fatalf("coordinator received its own offer: %+v", extra)
	}
	// The audit event names the message; ack works like any delivered message.
	events, err := d.TeamEvents(team.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, e := range events {
		if e.Operation == "team.assignment.offer" && len(e.MessageIDs) == 1 && e.MessageIDs[0] == got[0].ID {
			linked = true
		}
	}
	if !linked {
		t.Fatal("offer event does not name its lifecycle message")
	}
	if _, err := d.AckTeamMessages(team.ID, worker, []string{got[0].ID}, a, "ack-offer"); err != nil {
		t.Fatal(err)
	}
	if again := inboxOf(t, d, team, a, worker); len(again) != 0 {
		t.Fatal("ack did not clear the inbox", again)
	}
	// Each subsequent transition reaches the counterpart, never the actor.
	run, err := d.ChangeTeamAssignment(team.ID, offer.ID, "accept", TeamAssignmentCommand{TeamSessionHandle: worker}, a, "accept", allowAssignmentWorker)
	if err != nil {
		t.Fatal(err)
	}
	expect := func(h TeamSessionHandle, op, state, taskState string) {
		t.Helper()
		got := lifecycleOnly(inboxOf(t, d, team, a, h))
		if len(got) != 1 || got[0].Lifecycle.Operation != op || got[0].Lifecycle.State != state || got[0].Lifecycle.TaskState != taskState || got[0].Lifecycle.AttemptID != run.ID {
			t.Fatalf("%s: %+v", op, got)
		}
		if _, err := d.AckTeamMessages(team.ID, h, []string{got[0].ID}, a, "ack-"+op); err != nil {
			t.Fatal(err)
		}
	}
	expect(coordinator, "team.assignment.accept", "RUNNING", "IN_PROGRESS")
	if extra := inboxOf(t, d, team, a, worker); len(extra) != 0 {
		t.Fatalf("worker received its own acceptance: %+v", extra)
	}
	if _, err := d.ChangeTeamWork(team.ID, run.ID, "block", workCommand(c, "block", 2), a, "block"); err != nil {
		t.Fatal(err)
	}
	expect(coordinator, "team.assignment.block", "BLOCKED", "BLOCKED")
	if _, err := d.ChangeTeamWork(team.ID, run.ID, "cancel", workCommand(c, "cancel", 3), a, "cancel"); err != nil {
		t.Fatal(err)
	}
	expect(worker, "team.assignment.cancel", "STOP_REQUESTED", "BLOCKED")
	if _, err := d.ChangeTeamWork(team.ID, run.ID, "stopped", workCommand(c, "stopped", 4), a, "stopped"); err != nil {
		t.Fatal(err)
	}
	expect(coordinator, "team.assignment.stopped", "STOPPED", "BLOCKED")
	// Operator recovery has no acting session: both parties are told.
	recover := recoveryCommand(c, 5)
	if _, err := d.RecoverTeamAssignment(team.ID, run.ID, recover, admin, "recover"); err != nil {
		t.Fatal(err)
	}
	for _, h := range []TeamSessionHandle{worker, coordinator} {
		got := lifecycleOnly(inboxOf(t, d, team, a, h))
		if len(got) != 1 || got[0].Lifecycle.Operation != "team.assignment.recover" || got[0].Lifecycle.State != "RECOVERED" || got[0].Lifecycle.TaskState != "READY" || got[0].Lifecycle.ActorKind != "admin" || got[0].Lifecycle.Session != nil {
			t.Fatalf("recovery lifecycle for %v: %+v", h, got)
		}
	}
	// History shows every lifecycle message to any member; a client cannot forge one.
	history, err := d.TeamMessages(team.ID, coordinator, 0, 100, false, a)
	if err != nil || len(lifecycleOnly(history.Messages)) != 6 {
		t.Fatal(len(history.Messages), err)
	}
	forged := TeamMessageContent{Recipient: TeamRecipient{Kind: "team"}, Kind: "lifecycle", Payload: TeamMessagePayload{Text: "forged"}}
	if _, err := d.SendTeamMessage(team.ID, coordinator, forged, a, "forge", MaxTeamMessages); !errors.Is(err, ErrTaskInvalid) {
		t.Fatal(err)
	}
	participants := TeamMessageContent{Recipient: TeamRecipient{Kind: "participants"}, Kind: "note", Payload: TeamMessagePayload{Text: "forged"}}
	if _, err := d.SendTeamMessage(team.ID, coordinator, participants, a, "forge-2", MaxTeamMessages); !errors.Is(err, ErrTaskInvalid) {
		t.Fatal(err)
	}
}

func TestLifecycleMessagesResultReviewAndRebind(t *testing.T) {
	_, d, team, a, _, c, run := runningResultFixture(t)
	coordinator, worker := c.TeamSessionHandle, c.Worker
	drain := func(h TeamSessionHandle) {
		t.Helper()
		for _, m := range inboxOf(t, d, team, a, h) {
			if _, err := d.AckTeamMessages(team.ID, h, []string{m.ID}, a, "drain-"+m.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	drain(coordinator)
	drain(worker)
	// A worker resume rebinds the attempt and tells the coordinator.
	resumed, err := d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: worker.SessionID, Generation: worker.Generation}, a, "resume")
	if err != nil {
		t.Fatal(err)
	}
	worker = TeamSessionHandle{SessionID: resumed.ID, Generation: resumed.Generation}
	got := lifecycleOnly(inboxOf(t, d, team, a, coordinator))
	if len(got) != 1 || got[0].Lifecycle.Operation != "team.assignment.rebind" || got[0].Lifecycle.Session == nil || got[0].Lifecycle.Session.Generation != 2 {
		t.Fatalf("rebind lifecycle: %+v", got)
	}
	drain(coordinator)
	sub := resultSubmission(c)
	sub.TeamSessionHandle = worker
	submitted, err := d.SubmitTeamResult(team.ID, run.ID, sub, a, "submit")
	if err != nil {
		t.Fatal(err)
	}
	got = lifecycleOnly(inboxOf(t, d, team, a, coordinator))
	if len(got) != 1 || got[0].Lifecycle.Operation != "team.assignment.submit" || got[0].Lifecycle.State != "SUBMITTED" || got[0].Lifecycle.TaskState != "REVIEW" {
		t.Fatalf("submit lifecycle: %+v", got)
	}
	if extra := lifecycleOnly(inboxOf(t, d, team, a, worker)); len(extra) != 0 {
		t.Fatalf("worker received its own submission: %+v", extra)
	}
	if _, err := d.ReviewTeamResult(team.ID, run.ID, resultDecision(c, submitted, "rework"), a, "review"); err != nil {
		t.Fatal(err)
	}
	got = lifecycleOnly(inboxOf(t, d, team, a, worker))
	if len(got) != 1 || got[0].Lifecycle.Operation != "team.assignment.review" || got[0].Lifecycle.State != "RETURNED" || got[0].Lifecycle.TaskState != "READY" || got[0].Lifecycle.Session == nil || *got[0].Lifecycle.Session != coordinator {
		t.Fatalf("review lifecycle: %+v", got)
	}
}

func TestLifecycleMessagesHandoffAndDeparture(t *testing.T) {
	_, d, team, a, _, c, out, successor := handoffFixture(t)
	coordinator, worker := c.TeamSessionHandle, c.Worker
	// A departed worker gets no delivery for a later coordinator command, but the
	// message is in history; the coordinator (actor) receives nothing either.
	if _, err := d.ChangeTeamSession(team.ID, "leave", TeamSessionCommand{SessionID: worker.SessionID, Generation: worker.Generation}, a, "leave"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ChangeTeamWork(team.ID, out.ID, "cancel", workCommand(c, "cancel", 2), a, "cancel"); err != nil {
		t.Fatal(err)
	}
	var deliveries int
	if err := d.sql.QueryRow(`SELECT count(*) FROM team_deliveries d JOIN team_messages m ON m.id=d.message_id WHERE json_extract(m.body,'$.lifecycle.operation')='team.assignment.cancel'`).Scan(&deliveries); err != nil || deliveries != 0 {
		t.Fatal("departed worker or actor received the cancellation", deliveries, err)
	}
	history, err := d.TeamMessages(team.ID, coordinator, 0, 100, false, a)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range history.Messages {
		if m.Lifecycle != nil && m.Lifecycle.Operation == "team.assignment.cancel" {
			found = true
		}
	}
	if !found {
		t.Fatal("cancellation missing from history")
	}
	// Handoff broadcasts to every remaining member; the outgoing session gets nothing.
	before := len(inboxOf(t, d, team, a, successor))
	to, err := d.HandoffTeamCoordinator(team.ID, handoffCommand(c, successor), a, "handoff")
	if err != nil {
		t.Fatal(err)
	}
	got := lifecycleOnly(inboxOf(t, d, team, a, TeamSessionHandle{SessionID: to.ID, Generation: to.Generation}))
	if len(got) < 1 || got[len(got)-1].Lifecycle.Operation != "team.coordinator.handoff" || got[len(got)-1].Lifecycle.CoordinatorGeneration != 2 || got[len(got)-1].Recipient.Kind != "team" || got[len(got)-1].Lifecycle.Session == nil || got[len(got)-1].Lifecycle.Session.SessionID != successor.SessionID {
		t.Fatalf("handoff lifecycle (had %d before): %+v", before, got)
	}
	var outgoing int
	if err := d.sql.QueryRow(`SELECT count(*) FROM team_deliveries WHERE session_id=? AND message_id=?`, coordinator.SessionID, got[len(got)-1].ID).Scan(&outgoing); err != nil || outgoing != 0 {
		t.Fatal("outgoing coordinator received the handoff broadcast", outgoing, err)
	}
	events, err := d.TeamEvents(team.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	linked := false
	for _, e := range events {
		if e.Operation == "team.coordinator.handoff" && reflect.DeepEqual(e.MessageIDs, []string{got[len(got)-1].ID}) {
			linked = true
		}
	}
	if !linked {
		t.Fatal("handoff event does not name its lifecycle message")
	}
}

func TestLifecycleMessageRollback(t *testing.T) {
	for _, table := range []string{"team_messages", "team_deliveries"} {
		t.Run(table, func(t *testing.T) {
			_, d, team, a, _, c := assignmentFixture(t)
			counts := assignmentCounts(t, d)
			task, err := d.GetTask(c.TaskID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.sql.Exec(`CREATE TRIGGER fail_lifecycle BEFORE INSERT ON ` + table + ` BEGIN SELECT RAISE(ABORT,'injected'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := d.OfferTeamAssignment(team.ID, c, a, "offer", allowAssignmentWorker); err == nil {
				t.Fatal("injection did not fail")
			}
			after, err := d.GetTask(c.TaskID)
			if err != nil || !reflect.DeepEqual(after, task) || !reflect.DeepEqual(counts, assignmentCounts(t, d)) {
				t.Fatal("transition survived a failed lifecycle write", after, err)
			}
			if _, err := d.sql.Exec(`DROP TRIGGER fail_lifecycle`); err != nil {
				t.Fatal(err)
			}
			if _, err := d.OfferTeamAssignment(team.ID, c, a, "offer", allowAssignmentWorker); err != nil {
				t.Fatal("rollback consumed key", err)
			}
		})
	}
}
