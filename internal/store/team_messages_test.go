package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"aimem/internal/uuidv7"
)

func messageMember(t *testing.T, d *DB, team Team, a TeamAuditContext, key string) TeamSessionHandle {
	t.Helper()
	s, err := d.JoinTeam(team.ID, "worker", testProfile(), a, key)
	if err != nil {
		t.Fatal(err)
	}
	return TeamSessionHandle{s.ID, s.Generation}
}
func messageContent(kind string, r TeamRecipient) TeamMessageContent {
	return TeamMessageContent{Kind: kind, Recipient: r, Payload: TeamMessagePayload{Text: "Which interface?"}}
}

func TestTeamMessageRoutingAndAcknowledgement(t *testing.T) {
	_, d, team, a, _ := sessionFixture(t)
	sender := messageMember(t, d, team, a, "sender")
	receiver := messageMember(t, d, team, a, "receiver")
	c := messageContent("question", TeamRecipient{Kind: "member", ID: receiver.SessionID})
	m, err := d.SendTeamMessage(team.ID, sender, c, a, "send", MaxTeamMessages)
	if err != nil {
		t.Fatal(err)
	}
	if m.SenderID != sender.SessionID || m.ProfileRevision != 1 || m.Sequence < 1 {
		t.Fatalf("sender snapshot %+v", m)
	}
	if _, err = d.AckTeamMessages(team.ID, receiver, []string{m.ID}, a, "unseen"); !errors.Is(err, ErrTeamMessageUndelivered) {
		t.Fatalf("unseen ack %v", err)
	}
	own, err := d.TeamMessages(team.ID, sender, 0, 50, true, a)
	if err != nil || len(own.Messages) != 0 {
		t.Fatalf("routing %+v %v", own, err)
	}
	history, err := d.TeamMessages(team.ID, sender, 0, 50, false, a)
	if err != nil || len(history.Messages) != 1 {
		t.Fatalf("team visibility %+v %v", history, err)
	}
	// Reading history is not inbox delivery and cannot authorize acknowledgement.
	if _, err = d.AckTeamMessages(team.ID, sender, []string{m.ID}, a, "not-recipient"); !errors.Is(err, ErrTeamMessageUndelivered) {
		t.Fatal(err)
	}
	inbox, err := d.TeamMessages(team.ID, receiver, 0, 50, true, a)
	if err != nil || len(inbox.Messages) != 1 || inbox.HasMore || inbox.Next != m.Sequence {
		t.Fatalf("inbox %+v %v", inbox, err)
	}
	again, err := d.TeamMessages(team.ID, receiver, 0, 50, true, a)
	if err != nil || len(again.Messages) != 1 {
		t.Fatal("read acknowledged", err)
	}
	var count int
	if err = d.sql.QueryRow(`SELECT delivery_count FROM team_deliveries WHERE message_id=? AND session_id=?`, m.ID, receiver.SessionID).Scan(&count); err != nil || count != 2 {
		t.Fatal(count, err)
	}
	if _, err = d.AckTeamMessages(team.ID, receiver, []string{m.ID}, a, "ack"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.AckTeamMessages(team.ID, receiver, []string{m.ID}, a, "ack"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.AckTeamMessages(team.ID, receiver, []string{m.ID}, a, "ack-again"); err != nil {
		t.Fatal(err)
	}
	inbox, err = d.TeamMessages(team.ID, receiver, 0, 50, true, a)
	if err != nil || len(inbox.Messages) != 0 {
		t.Fatal(inbox, err)
	}
	answer := messageContent("answer", TeamRecipient{Kind: "member", ID: sender.SessionID})
	answer.ReplyTo = m.ID
	reply, err := d.SendTeamMessage(team.ID, receiver, answer, a, "answer", MaxTeamMessages)
	if err != nil || reply.ReplyTo != m.ID {
		t.Fatal(reply, err)
	}
	// Broadcast recipients are fixed now; later sessions see history only.
	broadcast, err := d.SendTeamMessage(team.ID, sender, messageContent("note", TeamRecipient{Kind: "team"}), a, "broadcast", MaxTeamMessages)
	if err != nil {
		t.Fatal(err)
	}
	later := messageMember(t, d, team, a, "later")
	late, err := d.TeamMessages(team.ID, later, 0, 100, true, a)
	if err != nil || len(late.Messages) != 0 {
		t.Fatal(late, err)
	}
	late, err = d.TeamMessages(team.ID, later, 0, 1, false, a)
	if err != nil || len(late.Messages) != 1 || !late.HasMore {
		t.Fatal(late, err)
	}
	last, err := d.TeamMessages(team.ID, later, late.Next, 100, false, a)
	if err != nil || len(last.Messages) != 2 || last.Messages[1].ID != broadcast.ID {
		t.Fatal(last, err)
	}
	events, err := d.TeamEvents(team.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), "Which interface?") {
		t.Fatal("audit duplicates message body")
	}
}

func TestTeamMessageConcurrentRetryAndValidation(t *testing.T) {
	r, d, team, a, admin := sessionFixture(t)
	h := messageMember(t, d, team, a, "sender")
	c := messageContent("note", TeamRecipient{Kind: "team"})
	var wg sync.WaitGroup
	results := make([]TeamMessage, 10)
	errs := make([]error, 10)
	for i := range results {
		wg.Go(func() { results[i], errs[i] = d.SendTeamMessage(team.ID, h, c, a, "same", 1) })
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil || results[i].ID != results[0].ID {
			t.Fatal(results, errs)
		}
	}
	if _, err := d.SendTeamMessage(team.ID, h, c, a, "over", 1); !errors.Is(err, ErrTeamMessageQuota) {
		t.Fatal(err)
	}
	modified := c
	modified.Payload.Text = "different"
	if _, err := d.SendTeamMessage(team.ID, h, modified, a, "same", 1); !errors.Is(err, ErrTaskRetryConflict) {
		t.Fatal(err)
	}
	for _, kind := range []string{"assignment", "accept", "result"} {
		bad := c
		bad.Kind = kind
		if _, err := d.SendTeamMessage(team.ID, h, bad, a, kind, 100); !errors.Is(err, ErrTaskInvalid) {
			t.Fatal(err)
		}
	}
	bad := c
	bad.Kind = "answer"
	bad.ReplyTo = results[0].ID
	if _, err := d.SendTeamMessage(team.ID, h, bad, a, "bad-answer", 100); !errors.Is(err, ErrTaskInvalid) {
		t.Fatal(err)
	}
	bad = c
	bad.AttemptID = uuidv7.New()
	if _, err := d.SendTeamMessage(team.ID, h, bad, a, "attempt", 100); !errors.Is(err, ErrTaskInvalid) {
		t.Fatal(err)
	}
	bad = c
	bad.TaskID = uuidv7.New()
	if _, err := d.SendTeamMessage(team.ID, h, bad, a, "task", 100); !errors.Is(err, ErrTaskNotFound) {
		t.Fatal(err)
	}
	task, err := d.CreateTask(TaskContent{Title: "reference"}, admin.Actor, "task")
	if err != nil {
		t.Fatal(err)
	}
	c.TaskID = task.ID
	c.Payload.Refs = []TaskRef{{Kind: "task", Ref: task.ID}}
	if _, err = d.SendTeamMessage(team.ID, h, c, a, "valid-task", 100); err != nil {
		t.Fatal(err)
	}
	bad = c
	bad.Payload.Refs = []TaskRef{{Kind: "task", Ref: uuidv7.New()}}
	if _, err = d.SendTeamMessage(team.ID, h, bad, a, "missing-ref", 100); !errors.Is(err, ErrTaskNotFound) {
		t.Fatal(err)
	}
	other, err := r.ConfigureTeam("alpha", "", 0, TeamContent{Name: "other", Enrollment: team.Enrollment}, admin, "other")
	if err != nil {
		t.Fatal(err)
	}
	otherH := messageMember(t, d, other, a, "other")
	bad = c
	bad.ReplyTo = results[0].ID
	if _, err = d.SendTeamMessage(other.ID, otherH, bad, a, "cross-reply", 100); !errors.Is(err, ErrTeamMessageNotFound) {
		t.Fatal(err)
	}
	if _, err = d.TeamMessages(other.ID, h, 0, 10, false, a); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
	if _, err = d.ChangeTeamSession(team.ID, "resume", TeamSessionCommand{SessionID: h.SessionID, Generation: 1}, a, "resume"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.SendTeamMessage(team.ID, h, c, a, "stale", 100); !errors.Is(err, ErrTeamSessionStale) {
		t.Fatal(err)
	}
	// Receipt replay remains safe after generation advance, until enrollment is lost.
	if _, err = d.SendTeamMessage(team.ID, h, messageContent("note", TeamRecipient{Kind: "team"}), a, "same", 1); err != nil {
		t.Fatal(err)
	}
	team.Enrollment = nil
	if _, err = r.ConfigureTeam("alpha", team.ID, 1, team.TeamContent, admin, "revoke"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.SendTeamMessage(team.ID, h, messageContent("note", TeamRecipient{Kind: "team"}), a, "same", 1); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
}

func TestTeamMessageRollbackAndReopen(t *testing.T) {
	r, d, team, a, _ := sessionFixture(t)
	h := messageMember(t, d, team, a, "sender")
	c := messageContent("note", TeamRecipient{Kind: "team"})
	fail := func() {
		t.Helper()
		if _, err := d.sql.Exec(`CREATE TRIGGER reject_message_audit BEFORE INSERT ON team_events BEGIN SELECT RAISE(ABORT,'failure'); END`); err != nil {
			t.Fatal(err)
		}
	}
	allow := func() {
		t.Helper()
		if _, err := d.sql.Exec(`DROP TRIGGER reject_message_audit`); err != nil {
			t.Fatal(err)
		}
	}
	fail()
	if _, err := d.SendTeamMessage(team.ID, h, c, a, "rollback", 100); err == nil {
		t.Fatal("send survived failed audit")
	}
	allow()
	var count int
	d.sql.QueryRow(`SELECT count(*) FROM team_messages`).Scan(&count)
	if count != 0 {
		t.Fatal("orphan messages")
	}
	m, err := d.SendTeamMessage(team.ID, h, c, a, "rollback", 100)
	if err != nil {
		t.Fatal(err)
	}
	fail()
	if _, err = d.TeamMessages(team.ID, h, 0, 10, true, a); err == nil {
		t.Fatal("delivery survived failed audit")
	}
	allow()
	if _, err = d.AckTeamMessages(team.ID, h, []string{m.ID}, a, "unseen"); !errors.Is(err, ErrTeamMessageUndelivered) {
		t.Fatal(err)
	}
	if _, err = d.TeamMessages(team.ID, h, 0, 10, true, a); err != nil {
		t.Fatal(err)
	}
	// A mixed valid/invalid batch cannot consume the valid message.
	if _, err = d.AckTeamMessages(team.ID, h, []string{m.ID, uuidv7.New()}, a, "mixed"); !errors.Is(err, ErrTeamMessageUndelivered) {
		t.Fatal(err)
	}
	fail()
	if _, err = d.AckTeamMessages(team.ID, h, []string{m.ID}, a, "ack"); err == nil {
		t.Fatal("ack survived failed audit")
	}
	allow()
	root := r.root
	r.Close()
	r2, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	d2, err := r2.OpenExisting("alpha")
	if err != nil {
		t.Fatal(err)
	}
	page, err := d2.TeamMessages(team.ID, h, 0, 10, true, a)
	if err != nil || len(page.Messages) != 1 || page.Messages[0].ID != m.ID {
		t.Fatal(page, err)
	}
	if _, err = d2.AckTeamMessages(team.ID, h, []string{m.ID}, a, "ack"); err != nil {
		t.Fatal(err)
	}
	page, err = d2.TeamMessages(team.ID, h, 0, 10, true, a)
	if err != nil || len(page.Messages) != 0 {
		t.Fatal(page, err)
	}
}

func TestTeamMessageMigration(t *testing.T) {
	_, d, team, a, _ := sessionFixture(t)
	h := messageMember(t, d, team, a, "sender")
	rewindReservationSchema(t, d)
	for _, q := range []string{`DROP TABLE team_assignments`, `DROP TABLE team_managed_tasks`, `DROP TABLE team_deliveries`, `DROP TABLE team_messages`, `UPDATE meta SET value='15' WHERE key='schema_version'`} {
		if _, err := d.sql.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.SendTeamMessage(team.ID, h, messageContent("note", TeamRecipient{Kind: "team"}), a, "migrated", MaxTeamMessages); err != nil {
		t.Fatal(err)
	}
	if v, _ := d.GetMeta("schema_version"); v != fmt.Sprint(currentSchema) {
		t.Fatal(v)
	}
}

func TestTeamMessageRecipientLifecycle(t *testing.T) {
	r, d, team, a, admin := sessionFixture(t)
	h := messageMember(t, d, team, a, "sender")
	left := messageMember(t, d, team, a, "left")
	if _, err := d.ChangeTeamSession(team.ID, "leave", TeamSessionCommand{SessionID: left.SessionID, Generation: 1}, a, "leave"); err != nil {
		t.Fatal(err)
	}
	coordinator, err := d.JoinTeam(team.ID, "coordinator", testProfile(), a, "coordinator")
	if err != nil {
		t.Fatal(err)
	}
	team.Enrollment[0].Coordinator = false
	if _, err = r.ConfigureTeam("alpha", team.ID, 1, team.TeamContent, admin, "eligibility"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{left.SessionID, coordinator.ID} {
		if _, err = d.SendTeamMessage(team.ID, h, messageContent("note", TeamRecipient{Kind: "member", ID: id}), a, id, 100); !errors.Is(err, ErrTeamSessionDenied) {
			t.Fatal(err)
		}
	}
	m, err := d.SendTeamMessage(team.ID, h, messageContent("note", TeamRecipient{Kind: "team"}), a, "broadcast", 100)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err = d.sql.QueryRow(`SELECT count(*) FROM team_deliveries WHERE message_id=?`, m.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	wrong := a
	wrong.Actor.TokenID = uuidv7.New()
	if _, err = d.TeamMessages(team.ID, h, 0, 100, true, wrong); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
}
