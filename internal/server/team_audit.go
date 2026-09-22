package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"aimem/internal/store"
)

// teamExportSchema versions the JSONL export record shapes.
const teamExportSchema = 1

// auditFilter reads the filter query parameters shared by the events and
// export routes. The caller has already rejected unknown parameters.
func auditFilter(q url.Values) store.TeamAuditFilter {
	return store.TeamAuditFilter{SessionID: q.Get("session_id"), TaskID: q.Get("task_id"), AttemptID: q.Get("attempt_id"), Operation: q.Get("operation"), Since: q.Get("since"), Until: q.Get("until")}
}

var auditFilterParams = map[string]bool{"session_id": true, "task_id": true, "attempt_id": true, "operation": true, "since": true, "until": true}

// auditQuery rejects repeated or unknown parameters so a misspelled filter
// never silently widens a read.
func auditQuery(r *http.Request, extra ...string) (url.Values, error) {
	allowed := map[string]bool{}
	for k := range auditFilterParams {
		allowed[k] = true
	}
	for _, k := range extra {
		allowed[k] = true
	}
	q := r.URL.Query()
	for k, values := range q {
		if len(values) != 1 || !allowed[k] {
			return nil, errors.New("invalid audit query parameter " + k)
		}
	}
	return q, nil
}

func auditNumber(q url.Values, name string, fallback, min, max int64) (int64, error) {
	if _, exists := q[name]; !exists {
		return fallback, nil
	}
	n, err := strconv.ParseInt(q.Get(name), 10, 64)
	if err != nil || n < min || n > max {
		return 0, errors.New("invalid " + name)
	}
	return n, nil
}

// teamEvents is the admin audit page: sequence-ordered accepted events after
// a cursor, optionally narrowed by the audit filter.
func (s *Server) teamEvents(w http.ResponseWriter, r *http.Request) {
	_, db := s.adminTeamProject(w, r)
	if db == nil {
		return
	}
	q, err := auditQuery(r, "after", "limit")
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	limit, err := teamPageLimit(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	after, err := auditNumber(q, "after", 0, 0, 1<<63-1)
	if err != nil {
		s.fail(w, 400, errors.New("after must be a nonnegative sequence"))
		return
	}
	es, err := db.TeamTimeline(r.PathValue("team"), auditFilter(q), after, 0, limit+1)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	var next int64
	if len(es) > limit {
		es = es[:limit]
		next = es[len(es)-1].Sequence
	}
	s.ok(w, map[string]any{"protocol_version": 1, "events": es, "next_cursor": next})
}

type teamExportCursor struct {
	AfterEvents      int64 `json:"after_events"`
	AfterMessages    int64 `json:"after_messages"`
	SnapshotEvents   int64 `json:"snapshot_events"`
	SnapshotMessages int64 `json:"snapshot_messages"`
}

// teamExport writes one JSONL page of a team's audit export: a header, up to
// limit events, up to limit messages (metadata unless include_bodies), and an
// end record with the cursor for the next page. The first page takes the
// snapshot; later pages pass it back so records accepted meanwhile never
// appear, and every page runs the admin gate again. The page is read fully
// before anything is written, so a storage failure is a normal error response.
func (s *Server) teamExport(w http.ResponseWriter, r *http.Request) {
	p, db := s.adminTeamProject(w, r)
	if db == nil {
		return
	}
	q, err := auditQuery(r, "after_events", "after_messages", "snapshot_events", "snapshot_messages", "limit", "include_bodies")
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	limit, err := teamPageLimit(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	var cursor teamExportCursor
	for _, field := range []struct {
		name  string
		value *int64
	}{{"after_events", &cursor.AfterEvents}, {"after_messages", &cursor.AfterMessages}, {"snapshot_events", &cursor.SnapshotEvents}, {"snapshot_messages", &cursor.SnapshotMessages}} {
		if *field.value, err = auditNumber(q, field.name, 0, 0, 1<<63-1); err != nil {
			s.fail(w, 400, err)
			return
		}
	}
	includeBodies := false
	if q.Has("include_bodies") {
		if includeBodies, err = strconv.ParseBool(q.Get("include_bodies")); err != nil {
			s.fail(w, 400, errors.New("invalid include_bodies"))
			return
		}
	}
	filter, err := auditFilter(q).Normalize()
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	team := r.PathValue("team")
	snapshot := store.TeamAuditSnapshot{EventSequence: cursor.SnapshotEvents, MessageSequence: cursor.SnapshotMessages}
	first := !q.Has("snapshot_events") && !q.Has("snapshot_messages")
	if first {
		if snapshot, err = db.TeamAuditSnapshot(team); err != nil {
			s.teamError(w, r, err)
			return
		}
		cursor.SnapshotEvents, cursor.SnapshotMessages = snapshot.EventSequence, snapshot.MessageSequence
	} else if !q.Has("snapshot_events") || !q.Has("snapshot_messages") || cursor.AfterEvents > cursor.SnapshotEvents || cursor.AfterMessages > cursor.SnapshotMessages {
		s.fail(w, 400, errors.New("a continued export needs both snapshot sequences and cursors within them"))
		return
	}
	events, err := db.TeamTimeline(team, filter, cursor.AfterEvents, cursor.SnapshotEvents, limit+1)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	messages, err := db.TeamMessageAudit(team, filter, cursor.AfterMessages, cursor.SnapshotMessages, limit+1, includeBodies)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	moreEvents, moreMessages := len(events) > limit, len(messages) > limit
	if moreEvents {
		events = events[:limit]
	}
	if moreMessages {
		messages = messages[:limit]
	}
	next := cursor
	if len(events) > 0 {
		next.AfterEvents = events[len(events)-1].Sequence
	}
	if len(messages) > 0 {
		next.AfterMessages = messages[len(messages)-1].Sequence
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.Encode(map[string]any{"record": "header", "schema_version": teamExportSchema, "protocol_version": 1, "project": p, "team_id": team, "filters": filter, "include_bodies": includeBodies, "limit": limit,
		"snapshot": map[string]any{"event_sequence": cursor.SnapshotEvents, "message_sequence": cursor.SnapshotMessages, "taken_at": snapshot.TakenAt}, "first_page": first,
		"exported_at": time.Now().UTC().Format(time.RFC3339), "server_version": Version, "request_id": w.Header().Get("X-Request-ID")})
	for _, e := range events {
		enc.Encode(struct {
			Record string `json:"record"`
			store.TeamEvent
		}{"event", e})
	}
	for _, m := range messages {
		enc.Encode(struct {
			Record string `json:"record"`
			store.TeamMessageAudit
		}{"message", m})
	}
	enc.Encode(map[string]any{"record": "end", "events": len(events), "messages": len(messages), "complete": !moreEvents && !moreMessages, "next": next})
}
