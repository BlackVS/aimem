package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"aimem/internal/uuidv7"
)

func timeline(t *testing.T, d *DB, team Team, f TeamAuditFilter) []TeamEvent {
	t.Helper()
	out, err := d.TeamTimeline(team.ID, f, 0, TeamAuditLatest, 101)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// window is the true half-open semantics on the requested instants, used to
// check that the whole-second normalization never changes the answer.
func window(t *testing.T, at []string, since, until time.Time) int {
	t.Helper()
	n := 0
	for _, s := range at {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		if !parsed.Before(since) && parsed.Before(until) {
			n++
		}
	}
	return n
}

func mentionsSession(e TeamEvent, id string) bool {
	return e.Session != nil && e.Session.ID == id || e.Assignment != nil && e.Assignment.Worker.SessionID == id ||
		e.Handoff != nil && (e.Handoff.From.SessionID == id || e.Handoff.To.SessionID == id)
}

func TestTeamAuditTimelineFilters(t *testing.T) {
	_, d, team, a, _, c, run := runningResultFixture(t)
	all, err := d.TeamEvents(team.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := timeline(t, d, team, TeamAuditFilter{}); !reflect.DeepEqual(got, all) {
		t.Fatal("unfiltered timeline differs from the event page")
	}
	worker := timeline(t, d, team, TeamAuditFilter{SessionID: c.Worker.SessionID})
	joined, offered := false, false
	for _, e := range worker {
		if !mentionsSession(e, c.Worker.SessionID) {
			t.Fatalf("event %s does not mention the worker", e.Operation)
		}
		joined = joined || e.Operation == "team.join"
		offered = offered || e.Operation == "team.assignment.offer"
	}
	if !joined || !offered {
		t.Fatal("worker timeline", len(worker))
	}
	coordinator := timeline(t, d, team, TeamAuditFilter{SessionID: c.TeamSessionHandle.SessionID})
	for _, e := range coordinator {
		if e.Assignment == nil && e.Session != nil && e.Session.ID != c.TeamSessionHandle.SessionID {
			t.Fatal("foreign session event", e.Operation)
		}
	}
	if len(coordinator) >= len(all) || len(worker) >= len(all) {
		t.Fatal("session filters did not narrow", len(coordinator), len(worker), len(all))
	}
	assignments := 0
	for _, e := range all {
		if e.Assignment != nil {
			assignments++
		}
	}
	for _, f := range []TeamAuditFilter{{TaskID: run.TaskID}, {AttemptID: run.ID}, {Operation: "team.assignment."}} {
		got := timeline(t, d, team, f)
		if len(got) != assignments || assignments == 0 {
			t.Fatalf("%+v: %d events, want %d", f, len(got), assignments)
		}
		for _, e := range got {
			if e.Assignment == nil || e.Assignment.ID != run.ID || !strings.HasPrefix(e.Operation, "team.assignment.") {
				t.Fatal(e.Operation)
			}
		}
	}
	if got := timeline(t, d, team, TeamAuditFilter{Operation: "team.assignment.offer"}); len(got) != 1 || got[0].Operation != "team.assignment.offer" {
		t.Fatal("exact operation", got)
	}
	if got := timeline(t, d, team, TeamAuditFilter{Operation: "team.assignment.offe"}); len(got) != 0 {
		t.Fatal("a prefix needs a trailing dot", got)
	}
	// No event mentions a session that never joined or a foreign task.
	for _, f := range []TeamAuditFilter{{SessionID: uuidv7.New()}, {TaskID: uuidv7.New()}, {AttemptID: uuidv7.New()}, {Operation: "team.nothing."}} {
		if got := timeline(t, d, team, f); len(got) != 0 {
			t.Fatalf("%+v matched %d", f, len(got))
		}
	}
	// Time range: half-open on the server timestamp; offsets normalize to UTC.
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if got := timeline(t, d, team, TeamAuditFilter{Since: future}); len(got) != 0 {
		t.Fatal("future since", len(got))
	}
	if got := timeline(t, d, team, TeamAuditFilter{Until: all[0].At}); len(got) != 0 {
		t.Fatal("until is exclusive", len(got))
	}
	if got := timeline(t, d, team, TeamAuditFilter{Since: "2000-01-01T02:00:00.5+02:00", Until: future}); len(got) != len(all) {
		t.Fatal("window", len(got), len(all))
	}
	f, err := TeamAuditFilter{Since: "2000-01-01T02:00:00.5+02:00", Until: "2000-01-01T00:00:00.9Z"}.Normalize()
	if err != nil || f.Since != "2000-01-01T00:00:01Z" || f.Until != "2000-01-01T00:00:01Z" {
		t.Fatal("fractional bounds round up; validity uses the instants", f, err)
	}
	// Fractional bounds around a stored whole second keep the half-open meaning.
	at := []string{}
	for _, e := range all {
		at = append(at, e.At)
	}
	first, err := time.Parse(time.RFC3339, all[0].At)
	if err != nil {
		t.Fatal(err)
	}
	far := first.Add(time.Hour)
	for _, w := range [][2]time.Time{{first.Add(500 * time.Millisecond), far}, {first.Add(-time.Hour), first.Add(500 * time.Millisecond)}, {first.Add(-500 * time.Millisecond), first.Add(500 * time.Millisecond)},
		{first.Add(100 * time.Millisecond), first.Add(900 * time.Millisecond)}, {first, first.Add(time.Second)}, {first.Add(time.Second), far}, {first.Add(-time.Hour), first.Add(time.Second)}} {
		got := timeline(t, d, team, TeamAuditFilter{Since: w[0].Format(time.RFC3339Nano), Until: w[1].Format(time.RFC3339Nano)})
		if len(got) != window(t, at, w[0], w[1]) {
			t.Fatalf("window [%s, %s): %d events, want %d", w[0].Format(time.RFC3339Nano), w[1].Format(time.RFC3339Nano), len(got), window(t, at, w[0], w[1]))
		}
	}
	// Combined filters intersect.
	if got := timeline(t, d, team, TeamAuditFilter{SessionID: c.Worker.SessionID, Operation: "team.join"}); len(got) != 1 || got[0].Operation != "team.join" {
		t.Fatal("intersection", got)
	}
	// An upper bound freezes the read: later events stay out.
	snapshot, err := d.TeamAuditSnapshot(team.ID)
	if err != nil || snapshot.EventSequence != all[len(all)-1].Sequence || snapshot.TakenAt == "" {
		t.Fatal(snapshot, err)
	}
	if _, err := d.ChangeTeamWork(team.ID, run.ID, "block", workCommand(c, "block", 2), a, "block"); err != nil {
		t.Fatal(err)
	}
	bounded, err := d.TeamTimeline(team.ID, TeamAuditFilter{}, 0, snapshot.EventSequence, 101)
	if err != nil || len(bounded) != len(all) {
		t.Fatal(len(bounded), err)
	}
	// A zero bound is an empty snapshot, never an unbounded read.
	if none, err := d.TeamTimeline(team.ID, TeamAuditFilter{}, 0, 0, 101); err != nil || len(none) != 0 {
		t.Fatal("zero bound", none, err)
	}
	if none, err := d.TeamMessageAudit(team.ID, TeamAuditFilter{}, 0, 0, 101, false); err != nil || len(none) != 0 {
		t.Fatal("zero message bound", none, err)
	}
	if got := timeline(t, d, team, TeamAuditFilter{}); len(got) <= len(all) {
		t.Fatal("unbounded read misses the new event")
	}
	// Paging by cursor within the bound.
	page, err := d.TeamTimeline(team.ID, TeamAuditFilter{}, all[1].Sequence, snapshot.EventSequence, 2)
	if err != nil || len(page) != 2 || page[0].Sequence != all[2].Sequence {
		t.Fatal(page, err)
	}
}

func TestTeamAuditRefusals(t *testing.T) {
	_, d, team, _, _, _, _ := runningResultFixture(t)
	for _, f := range []TeamAuditFilter{{SessionID: "x"}, {TaskID: "../x"}, {AttemptID: " "}, {Operation: "Team.X"}, {Operation: "a..b"}, {Operation: "team assignment"}, {Operation: strings.Repeat("a", 129)},
		{Since: "yesterday"}, {Until: "2026-09-22"}, {Since: "2026-09-22T10:00:00Z", Until: "2026-09-22T10:00:00Z"}, {Since: "2026-09-22T10:00:00.5Z", Until: "2026-09-22T10:00:00.5Z"}} {
		if _, err := d.TeamTimeline(team.ID, f, 0, TeamAuditLatest, 10); !errors.Is(err, ErrTaskInvalid) {
			t.Fatalf("%+v: %v", f, err)
		}
		if _, err := d.TeamMessageAudit(team.ID, f, 0, TeamAuditLatest, 10, false); !errors.Is(err, ErrTaskInvalid) {
			t.Fatalf("messages %+v: %v", f, err)
		}
	}
	for _, page := range [][3]int64{{-1, 0, 10}, {0, -1, 10}, {0, 0, 0}, {0, 0, 102}} {
		if _, err := d.TeamTimeline(team.ID, TeamAuditFilter{}, page[0], page[1], int(page[2])); !errors.Is(err, ErrTaskInvalid) {
			t.Fatal(page, err)
		}
		if _, err := d.TeamMessageAudit(team.ID, TeamAuditFilter{}, page[0], page[1], int(page[2]), false); !errors.Is(err, ErrTaskInvalid) {
			t.Fatal(page, err)
		}
	}
	unknown := uuidv7.New()
	if _, err := d.TeamTimeline(unknown, TeamAuditFilter{}, 0, TeamAuditLatest, 10); !errors.Is(err, ErrTeamNotFound) {
		t.Fatal(err)
	}
	if _, err := d.TeamMessageAudit(unknown, TeamAuditFilter{}, 0, TeamAuditLatest, 10, false); !errors.Is(err, ErrTeamNotFound) {
		t.Fatal(err)
	}
	if _, err := d.TeamAuditSnapshot(unknown); !errors.Is(err, ErrTeamNotFound) {
		t.Fatal(err)
	}
	if err := d.SetMeta(TasksMetaKey, "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.TeamTimeline(team.ID, TeamAuditFilter{}, 0, TeamAuditLatest, 10); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
	if _, err := d.TeamMessageAudit(team.ID, TeamAuditFilter{}, 0, TeamAuditLatest, 10, false); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
	if _, err := d.TeamAuditSnapshot(team.ID); !errors.Is(err, ErrTeamSessionDenied) {
		t.Fatal(err)
	}
}

func messageAuditOf(t *testing.T, d *DB, team Team, f TeamAuditFilter, bodies bool) []TeamMessageAudit {
	t.Helper()
	out, err := d.TeamMessageAudit(team.ID, f, 0, TeamAuditLatest, 101, bodies)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTeamAuditMessagesMetadataAndDeliveries(t *testing.T) {
	_, d, team, a, _, c, run := runningResultFixture(t)
	coordinator, worker := c.TeamSessionHandle, c.Worker
	question := messageContent("question", TeamRecipient{Kind: "member", ID: coordinator.SessionID})
	question.TaskID = run.TaskID
	asked, err := d.SendTeamMessage(team.ID, worker, question, a, "ask", MaxTeamMessages)
	if err != nil {
		t.Fatal(err)
	}
	note, err := d.SendTeamMessage(team.ID, coordinator, messageContent("note", TeamRecipient{Kind: "team"}), a, "note", MaxTeamMessages)
	if err != nil {
		t.Fatal(err)
	}
	// The coordinator reads and acknowledges the question; the worker never reads.
	inbox := inboxOf(t, d, team, a, coordinator)
	if _, err := d.AckTeamMessages(team.ID, coordinator, []string{asked.ID}, a, "ack"); err != nil {
		t.Fatal(err)
	}
	all := messageAuditOf(t, d, team, TeamAuditFilter{}, false)
	byID := map[string]TeamMessageAudit{}
	lifecycle := 0
	for i, m := range all {
		byID[m.ID] = m
		if m.Payload != nil {
			t.Fatal("body exported by default", m.ID)
		}
		if i > 0 && m.Sequence <= all[i-1].Sequence {
			t.Fatal("order")
		}
		if m.Kind == "lifecycle" {
			lifecycle++
			if m.Lifecycle == nil || m.SenderID != "" || m.Profile != nil {
				t.Fatalf("lifecycle metadata: %+v", m)
			}
		}
	}
	if lifecycle == 0 || len(all) != lifecycle+2 {
		t.Fatal("message count", len(all), lifecycle)
	}
	q := byID[asked.ID]
	if q.SenderID != worker.SessionID || q.SenderGeneration != worker.Generation || q.Profile == nil || q.Profile.Platform != "test-client" || q.TaskID != run.TaskID || q.Kind != "question" || q.Recipient.ID != coordinator.SessionID {
		t.Fatalf("question metadata: %+v", q)
	}
	if len(q.Deliveries) != 1 || q.Deliveries[0].SessionID != coordinator.SessionID || q.Deliveries[0].DeliveredAt == "" || q.Deliveries[0].DeliveryCount != 1 || q.Deliveries[0].AckedAt == "" {
		t.Fatalf("question deliveries: %+v", q.Deliveries)
	}
	// Broadcast: both sessions are recipients; the coordinator's inbox read
	// delivered it without acknowledgement, the worker never read.
	n := byID[note.ID]
	if len(n.Deliveries) != 2 {
		t.Fatalf("note deliveries: %+v", n.Deliveries)
	}
	for _, dl := range n.Deliveries {
		read := dl.SessionID == coordinator.SessionID
		if (dl.DeliveredAt != "") != read || (dl.DeliveryCount == 1) != read || dl.AckedAt != "" {
			t.Fatalf("note delivery state: %+v", dl)
		}
	}
	// Lifecycle messages the coordinator read are delivered but not acknowledged.
	for _, m := range inbox {
		if m.Kind != "lifecycle" {
			continue
		}
		got := byID[m.ID]
		found := false
		for _, dl := range got.Deliveries {
			if dl.SessionID == coordinator.SessionID {
				found = dl.DeliveredAt != "" && dl.AckedAt == ""
			}
		}
		if !found {
			t.Fatalf("delivered lifecycle: %+v", got.Deliveries)
		}
	}
	// Bodies are opt-in and copied as stored.
	with := messageAuditOf(t, d, team, TeamAuditFilter{}, true)
	for _, m := range with {
		if m.Payload == nil {
			t.Fatal("body requested but absent", m.ID)
		}
		if m.ID == asked.ID && m.Payload.Text != "Which interface?" {
			t.Fatal(m.Payload)
		}
	}
	// Session filter: sender or recipient.
	for _, m := range messageAuditOf(t, d, team, TeamAuditFilter{SessionID: worker.SessionID}, false) {
		recipient := false
		for _, dl := range m.Deliveries {
			recipient = recipient || dl.SessionID == worker.SessionID
		}
		if m.SenderID != worker.SessionID && !recipient {
			t.Fatalf("foreign message for the worker: %+v", m)
		}
	}
	coordinatorMessages := messageAuditOf(t, d, team, TeamAuditFilter{SessionID: coordinator.SessionID}, false)
	if _, ok := lookup(coordinatorMessages, asked.ID); !ok {
		t.Fatal("recipient filter lost the question")
	}
	if got := messageAuditOf(t, d, team, TeamAuditFilter{SessionID: uuidv7.New()}, false); len(got) != 0 {
		t.Fatal("stranger", got)
	}
	// Task, attempt and operation filters.
	tasked := messageAuditOf(t, d, team, TeamAuditFilter{TaskID: run.TaskID}, false)
	if _, ok := lookup(tasked, note.ID); ok || len(tasked) != lifecycle+1 {
		t.Fatal("task filter", len(tasked))
	}
	if got := messageAuditOf(t, d, team, TeamAuditFilter{AttemptID: run.ID}, false); len(got) != lifecycle {
		t.Fatal("attempt filter", len(got))
	}
	if got := messageAuditOf(t, d, team, TeamAuditFilter{Operation: "team.assignment."}, false); len(got) != lifecycle {
		t.Fatal("operation filter", len(got))
	}
	if got := messageAuditOf(t, d, team, TeamAuditFilter{Operation: "team.assignment.offer"}, false); len(got) != 1 || got[0].Lifecycle.Operation != "team.assignment.offer" {
		t.Fatal("exact operation", got)
	}
	// Time range and snapshot bound; fractional bounds keep the half-open meaning.
	if got := messageAuditOf(t, d, team, TeamAuditFilter{Until: all[0].CreatedAt}, false); len(got) != 0 {
		t.Fatal("until", len(got))
	}
	created := []string{}
	for _, m := range all {
		created = append(created, m.CreatedAt)
	}
	first, err := time.Parse(time.RFC3339, all[0].CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range [][2]time.Time{{first.Add(500 * time.Millisecond), first.Add(time.Hour)}, {first.Add(-time.Hour), first.Add(500 * time.Millisecond)}, {first.Add(100 * time.Millisecond), first.Add(900 * time.Millisecond)}} {
		if got := messageAuditOf(t, d, team, TeamAuditFilter{Since: w[0].Format(time.RFC3339Nano), Until: w[1].Format(time.RFC3339Nano)}, false); len(got) != window(t, created, w[0], w[1]) {
			t.Fatalf("message window [%s, %s): %d, want %d", w[0], w[1], len(got), window(t, created, w[0], w[1]))
		}
	}
	snapshot, err := d.TeamAuditSnapshot(team.ID)
	if err != nil || snapshot.MessageSequence != all[len(all)-1].Sequence {
		t.Fatal(snapshot, err)
	}
	if _, err := d.SendTeamMessage(team.ID, coordinator, messageContent("note", TeamRecipient{Kind: "team"}), a, "later", MaxTeamMessages); err != nil {
		t.Fatal(err)
	}
	bounded, err := d.TeamMessageAudit(team.ID, TeamAuditFilter{}, 0, snapshot.MessageSequence, 101, false)
	if err != nil || len(bounded) != len(all) {
		t.Fatal(len(bounded), err)
	}
	page, err := d.TeamMessageAudit(team.ID, TeamAuditFilter{}, all[0].Sequence, snapshot.MessageSequence, 1, false)
	if err != nil || len(page) != 1 || page[0].ID != all[1].ID {
		t.Fatal(page, err)
	}
}

func lookup(msgs []TeamMessageAudit, id string) (TeamMessageAudit, bool) {
	for _, m := range msgs {
		if m.ID == id {
			return m, true
		}
	}
	return TeamMessageAudit{}, false
}
