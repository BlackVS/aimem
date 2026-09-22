package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"aimem/internal/store"
)

type exportPage struct {
	header   map[string]any
	events   []store.TeamEvent
	messages []store.TeamMessageAudit
	end      map[string]any
}

func parseExport(t *testing.T, w *httptest.ResponseRecorder) exportPage {
	t.Helper()
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/x-ndjson" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Request-ID") == "" {
		t.Fatal(w.Code, w.Header(), w.Body)
	}
	var page exportPage
	lines := strings.Split(strings.TrimSpace(w.Body.String()), "\n")
	for i, line := range lines {
		var record struct {
			Record string `json:"record"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(line, err)
		}
		switch record.Record {
		case "header":
			if i != 0 {
				t.Fatal("header not first")
			}
			json.Unmarshal([]byte(line), &page.header)
		case "event":
			var e store.TeamEvent
			json.Unmarshal([]byte(line), &e)
			page.events = append(page.events, e)
		case "message":
			var m store.TeamMessageAudit
			json.Unmarshal([]byte(line), &m)
			page.messages = append(page.messages, m)
		case "end":
			if i != len(lines)-1 {
				t.Fatal("end not last")
			}
			json.Unmarshal([]byte(line), &page.end)
		default:
			t.Fatal("unknown record", line)
		}
	}
	if page.header == nil || page.end == nil {
		t.Fatal("missing header or end", w.Body)
	}
	return page
}

func eventsPage(t *testing.T, w *httptest.ResponseRecorder) []store.TeamEvent {
	t.Helper()
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	var page struct {
		Events []store.TeamEvent `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page.Events
}

func TestAuditHTTPFilteredEventsAndExport(t *testing.T) {
	f := newAssignmentFixture(t)
	run := f.running(t)
	events, export := f.base+"/events", f.base+"/export"
	// Admin only, on both routes, whatever the query.
	for _, path := range []string{events + "?session_id=" + f.recipient.SessionID, export} {
		for _, token := range []string{f.alice, f.peer, f.writer} {
			if w := taskReq(t, f.h, "GET", path, token, "", ""); w.Code != 403 {
				t.Fatal(path, w.Code, w.Body)
			}
		}
	}
	// Unknown, repeated or malformed parameters are refused, never ignored.
	for _, path := range []string{events + "?foo=1", events + "?session_id=a&session_id=b", events + "?session_id=nope", events + "?operation=Team.X", events + "?since=yesterday",
		export + "?after=1", export + "?snapshot_events=5", export + "?snapshot_events=5&snapshot_messages=5&after_events=6", export + "?include_bodies=maybe", export + "?limit=0", export + "?until=2026-09-22"} {
		if w := taskReq(t, f.h, "GET", path, f.admin, "", ""); w.Code != 400 {
			t.Fatal(path, w.Code, w.Body)
		}
	}
	all := eventsPage(t, taskReq(t, f.h, "GET", events, f.admin, "", ""))
	worker := eventsPage(t, taskReq(t, f.h, "GET", events+"?session_id="+f.recipient.SessionID, f.admin, "", ""))
	if len(worker) == 0 || len(worker) >= len(all) {
		t.Fatal(len(worker), len(all))
	}
	for _, e := range worker {
		if (e.Session == nil || e.Session.ID != f.recipient.SessionID) && (e.Assignment == nil || e.Assignment.Worker.SessionID != f.recipient.SessionID) {
			t.Fatal("foreign event", e.Operation)
		}
	}
	assignments := 0
	for _, e := range all {
		if e.Assignment != nil {
			assignments++
		}
	}
	for _, query := range []string{"?operation=team.assignment.", "?task_id=" + run.TaskID, "?attempt_id=" + run.ID, "?attempt_id=" + run.ID + "&since=2000-01-01T00:00:00Z&until=2100-01-01T00:00:00Z"} {
		if got := eventsPage(t, taskReq(t, f.h, "GET", events+query, f.admin, "", "")); len(got) != assignments {
			t.Fatal(query, len(got), assignments)
		}
	}
	if got := eventsPage(t, taskReq(t, f.h, "GET", events+"?until=2000-01-01T00:00:00Z", f.admin, "", "")); len(got) != 0 {
		t.Fatal("until", len(got))
	}
	// First export page takes the snapshot; a later write must not leak in.
	first := parseExport(t, taskReq(t, f.h, "GET", export+"?limit=1", f.admin, "", ""))
	snapshot := first.header["snapshot"].(map[string]any)
	snapEvents, snapMessages := int64(snapshot["event_sequence"].(float64)), int64(snapshot["message_sequence"].(float64))
	if first.header["schema_version"].(float64) != 1 || first.header["protocol_version"].(float64) != 1 || first.header["project"] != "alpha" || first.header["team_id"] != strings.TrimPrefix(f.base, "/v1/projects/alpha/teams/") ||
		first.header["include_bodies"] != false || first.header["first_page"] != true || snapshot["taken_at"] == "" || snapEvents != all[len(all)-1].Sequence || snapMessages == 0 {
		t.Fatalf("header %+v", first.header)
	}
	if len(first.events) != 1 || len(first.messages) != 1 || first.end["complete"] != false {
		t.Fatalf("first page %d %d %+v", len(first.events), len(first.messages), first.end)
	}
	if first.messages[0].Payload != nil || first.messages[0].Kind != "lifecycle" || len(first.messages[0].Deliveries) == 0 {
		t.Fatalf("message metadata %+v", first.messages[0])
	}
	if got := assignmentResult(t, f.execute(t, run.ID, "block", f.peer, "block", workBody(f.recipient, 0, 2, "waiting")), 200); got.State != "BLOCKED" {
		t.Fatal(got)
	}
	exported := append([]store.TeamEvent{}, first.events...)
	messages := len(first.messages)
	next := first.end["next"].(map[string]any)
	for pages := 0; first.end["complete"] != true; pages++ {
		if pages > 50 {
			t.Fatal("export never completes")
		}
		path := fmt.Sprintf("%s?limit=1&after_events=%d&after_messages=%d&snapshot_events=%d&snapshot_messages=%d", export, int64(next["after_events"].(float64)), int64(next["after_messages"].(float64)), int64(next["snapshot_events"].(float64)), int64(next["snapshot_messages"].(float64)))
		page := parseExport(t, taskReq(t, f.h, "GET", path, f.admin, "", ""))
		if page.header["first_page"] != false || int64(page.header["snapshot"].(map[string]any)["event_sequence"].(float64)) != snapEvents {
			t.Fatalf("continued header %+v", page.header)
		}
		exported = append(exported, page.events...)
		messages += len(page.messages)
		first.end, next = page.end, page.end["next"].(map[string]any)
	}
	if len(exported) != len(all) {
		t.Fatal("exported events", len(exported), len(all))
	}
	for i, e := range exported {
		if e.Sequence != all[i].Sequence || e.Sequence > snapEvents {
			t.Fatal("export order or snapshot leak", i, e.Sequence)
		}
	}
	if messages == 0 || int64(next["after_messages"].(float64)) != snapMessages {
		t.Fatal("messages", messages, next)
	}
	// A fresh export sees the block; bodies are opt-in; filters apply to both streams.
	fresh := parseExport(t, taskReq(t, f.h, "GET", export+"?include_bodies=true", f.admin, "", ""))
	if len(fresh.events) <= len(all) || fresh.end["complete"] != true || fresh.header["include_bodies"] != true {
		t.Fatalf("fresh export %d %+v", len(fresh.events), fresh.end)
	}
	for _, m := range fresh.messages {
		if m.Payload == nil {
			t.Fatal("body missing", m.ID)
		}
	}
	filtered := parseExport(t, taskReq(t, f.h, "GET", export+"?operation=team.assignment.block", f.admin, "", ""))
	if len(filtered.events) != 1 || filtered.events[0].Operation != "team.assignment.block" || len(filtered.messages) != 1 || filtered.messages[0].Lifecycle.Operation != "team.assignment.block" || filtered.header["filters"].(map[string]any)["operation"] != "team.assignment.block" {
		t.Fatalf("filtered export %+v %+v", filtered.events, filtered.messages)
	}
	// Disabled project: refused on every page.
	d, _ := f.reg.OpenExisting("alpha")
	d.SetMeta(store.TasksMetaKey, "off")
	if w := taskReq(t, f.h, "GET", export, f.admin, "", ""); w.Code != 403 {
		t.Fatal(w.Code, w.Body)
	}
}
