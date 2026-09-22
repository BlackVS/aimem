package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// TeamAuditFilter narrows a timeline or export read. IDs are exact; the
// operation is an exact name or a prefix ending in '.'; since/until are
// RFC3339 timestamps compared against the server timestamp of each record as
// a half-open range [since, until). Normalized to UTC seconds on validation
// because stored timestamps are RFC3339 UTC and compare lexicographically.
type TeamAuditFilter struct {
	SessionID string `json:"session_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
	Operation string `json:"operation,omitempty"`
	Since     string `json:"since,omitempty"`
	Until     string `json:"until,omitempty"`
}

var auditOperationRE = regexp.MustCompile(`^[a-z_]+(\.[a-z_]+)*\.?$`)

// Normalize validates the filter and returns it with UTC timestamps.
func (f TeamAuditFilter) Normalize() (TeamAuditFilter, error) {
	for _, id := range []struct{ name, value string }{{"session_id", f.SessionID}, {"task_id", f.TaskID}, {"attempt_id", f.AttemptID}} {
		if id.value != "" && !taskIDRE.MatchString(id.value) {
			return f, invalid(errors.New("invalid " + id.name))
		}
	}
	if f.Operation != "" && (len(f.Operation) > 128 || !auditOperationRE.MatchString(f.Operation)) {
		return f, invalid(errors.New("operation must be a dotted name or a prefix ending in '.'"))
	}
	for _, ts := range []struct {
		name  string
		value *string
	}{{"since", &f.Since}, {"until", &f.Until}} {
		if *ts.value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339Nano, *ts.value)
		if err != nil {
			return f, invalid(errors.New(ts.name + " must be an RFC3339 timestamp"))
		}
		*ts.value = parsed.UTC().Format(time.RFC3339)
	}
	if f.Since != "" && f.Until != "" && f.Until <= f.Since {
		return f, invalid(errors.New("until must be after since"))
	}
	return f, nil
}

// operationClause matches the JSON path exactly or by prefix.
func (f TeamAuditFilter) operationClause(path string) (string, []any) {
	if strings.HasSuffix(f.Operation, ".") {
		return ` AND substr(json_extract(body,'` + path + `'),1,?)=?`, []any{len(f.Operation), f.Operation}
	}
	return ` AND json_extract(body,'` + path + `')=?`, []any{f.Operation}
}

// TeamAuditSnapshot is the upper bound of one export: pages read up to these
// sequences so records accepted after the first page never appear in it.
type TeamAuditSnapshot struct {
	EventSequence   int64  `json:"event_sequence"`
	MessageSequence int64  `json:"message_sequence"`
	TakenAt         string `json:"taken_at"`
}

// TeamDeliveryRecord is one recipient's delivery state for a message. A
// delivered_at means an inbox response was served, not that a model consumed
// it; acked_at is the client's explicit report, not proof of understanding.
type TeamDeliveryRecord struct {
	SessionID     string `json:"session_id"`
	DeliveredAt   string `json:"delivered_at,omitempty"`
	DeliveryCount int64  `json:"delivery_count"`
	AckedAt       string `json:"acked_at,omitempty"`
}

// TeamMessageAudit is message metadata for export. The payload is present
// only when bodies were requested; the reported profile is copied as stored,
// never completed or inferred.
type TeamMessageAudit struct {
	ID               string               `json:"id"`
	Sequence         int64                `json:"sequence"`
	TeamID           string               `json:"team_id"`
	SenderID         string               `json:"sender_id,omitempty"`
	SenderGeneration int64                `json:"sender_generation,omitempty"`
	ProfileRevision  int64                `json:"profile_revision,omitempty"`
	Profile          *TeamProfile         `json:"profile,omitempty"`
	CreatedAt        string               `json:"created_at"`
	Recipient        TeamRecipient        `json:"recipient"`
	Kind             string               `json:"kind"`
	TaskID           string               `json:"task_id,omitempty"`
	AttemptID        string               `json:"attempt_id,omitempty"`
	ReplyTo          string               `json:"reply_to,omitempty"`
	Lifecycle        *TeamLifecycle       `json:"lifecycle,omitempty"`
	Payload          *TeamMessagePayload  `json:"payload,omitempty"`
	Deliveries       []TeamDeliveryRecord `json:"deliveries"`
}

func (d *DB) auditTeam(teamID string) error {
	if err := d.taskScopeOK(); err != nil {
		return err
	}
	var enabled string
	if err := d.sql.QueryRow(`SELECT COALESCE((SELECT value FROM meta WHERE key=?),'')`, TasksMetaKey).Scan(&enabled); err != nil {
		return err
	}
	if enabled != "on" {
		return ErrTeamSessionDenied
	}
	_, err := readTeam(d.sql, teamID)
	return err
}

// TeamAuditSnapshot reads the current upper sequences of a team's events and
// messages with the server time.
func (d *DB) TeamAuditSnapshot(teamID string) (TeamAuditSnapshot, error) {
	if err := d.auditTeam(teamID); err != nil {
		return TeamAuditSnapshot{}, err
	}
	out := TeamAuditSnapshot{TakenAt: nowUTC()}
	err := d.sql.QueryRow(`SELECT COALESCE((SELECT MAX(sequence) FROM team_events WHERE team_id=?),0), COALESCE((SELECT MAX(sequence) FROM team_messages WHERE team_id=?),0)`, teamID, teamID).Scan(&out.EventSequence, &out.MessageSequence)
	return out, err
}

func auditPage(after, upTo int64, limit int) error {
	if after < 0 || upTo < 0 || limit < 1 || limit > 101 {
		return invalid(errors.New("invalid audit page"))
	}
	return nil
}

// TeamTimeline reads accepted audit events in sequence order with sequence
// in (after, upTo] (upTo zero: unbounded) that match the filter. A session
// matches the event session, the assignment worker or either handoff side;
// a task matches the assignment or managed task; an attempt matches the
// assignment or managed attempt. A sparse filter scans the team's events in
// order until limit matches, so keep limit small for large teams.
func (d *DB) TeamTimeline(teamID string, f TeamAuditFilter, after, upTo int64, limit int) ([]TeamEvent, error) {
	if err := d.auditTeam(teamID); err != nil {
		return nil, err
	}
	if err := auditPage(after, upTo, limit); err != nil {
		return nil, err
	}
	f, err := f.Normalize()
	if err != nil {
		return nil, err
	}
	query := `SELECT sequence,body FROM team_events WHERE team_id=? AND sequence>?`
	args := []any{teamID, after}
	if upTo > 0 {
		query += ` AND sequence<=?`
		args = append(args, upTo)
	}
	if f.SessionID != "" {
		query += ` AND (json_extract(body,'$.session.id')=? OR json_extract(body,'$.assignment.worker.session_id')=? OR json_extract(body,'$.handoff.from.session_id')=? OR json_extract(body,'$.handoff.to.session_id')=?)`
		args = append(args, f.SessionID, f.SessionID, f.SessionID, f.SessionID)
	}
	if f.TaskID != "" {
		query += ` AND (json_extract(body,'$.assignment.task_id')=? OR json_extract(body,'$.managed.task_id')=?)`
		args = append(args, f.TaskID, f.TaskID)
	}
	if f.AttemptID != "" {
		query += ` AND (json_extract(body,'$.assignment.id')=? OR json_extract(body,'$.managed.attempt_id')=?)`
		args = append(args, f.AttemptID, f.AttemptID)
	}
	if f.Operation != "" {
		clause, more := f.operationClause("$.operation")
		query += clause
		args = append(args, more...)
	}
	if f.Since != "" {
		query += ` AND json_extract(body,'$.at')>=?`
		args = append(args, f.Since)
	}
	if f.Until != "" {
		query += ` AND json_extract(body,'$.at')<?`
		args = append(args, f.Until)
	}
	query += ` ORDER BY sequence LIMIT ?`
	args = append(args, limit)
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamEvent{}
	for rows.Next() {
		var seq int64
		var body string
		if err := rows.Scan(&seq, &body); err != nil {
			return nil, err
		}
		var e TeamEvent
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, err
		}
		e.Sequence = seq
		out = append(out, e)
	}
	return out, rows.Err()
}

// TeamMessageAudit reads message metadata in sequence order with sequence in
// (after, upTo] (upTo zero: unbounded) that match the filter, with one
// delivery record per recipient session. A session matches the sender or a
// recipient; the operation applies to the lifecycle record, so an operation
// filter yields lifecycle messages only. Payloads are included only on
// request; metadata is the default.
func (d *DB) TeamMessageAudit(teamID string, f TeamAuditFilter, after, upTo int64, limit int, includeBodies bool) ([]TeamMessageAudit, error) {
	if err := d.auditTeam(teamID); err != nil {
		return nil, err
	}
	if err := auditPage(after, upTo, limit); err != nil {
		return nil, err
	}
	f, err := f.Normalize()
	if err != nil {
		return nil, err
	}
	query := `SELECT body FROM team_messages WHERE team_id=? AND sequence>?`
	args := []any{teamID, after}
	if upTo > 0 {
		query += ` AND sequence<=?`
		args = append(args, upTo)
	}
	if f.SessionID != "" {
		query += ` AND (json_extract(body,'$.sender_id')=? OR EXISTS(SELECT 1 FROM team_deliveries d WHERE d.message_id=team_messages.id AND d.session_id=?))`
		args = append(args, f.SessionID, f.SessionID)
	}
	if f.TaskID != "" {
		query += ` AND json_extract(body,'$.task_id')=?`
		args = append(args, f.TaskID)
	}
	if f.AttemptID != "" {
		query += ` AND json_extract(body,'$.attempt_id')=?`
		args = append(args, f.AttemptID)
	}
	if f.Operation != "" {
		clause, more := f.operationClause("$.lifecycle.operation")
		query += clause
		args = append(args, more...)
	}
	if f.Since != "" {
		query += ` AND json_extract(body,'$.created_at')>=?`
		args = append(args, f.Since)
	}
	if f.Until != "" {
		query += ` AND json_extract(body,'$.created_at')<?`
		args = append(args, f.Until)
	}
	query += ` ORDER BY sequence LIMIT ?`
	args = append(args, limit)
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(query, args...)
	if err != nil {
		return nil, err
	}
	out := []TeamMessageAudit{}
	for rows.Next() {
		var body string
		var m TeamMessage
		if err = rows.Scan(&body); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(body), &m); err != nil {
			break
		}
		out = append(out, messageAuditRecord(m, includeBodies))
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Deliveries, err = readDeliveries(tx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func messageAuditRecord(m TeamMessage, includeBodies bool) TeamMessageAudit {
	out := TeamMessageAudit{ID: m.ID, Sequence: m.Sequence, TeamID: m.TeamID, SenderID: m.SenderID, SenderGeneration: m.SenderGeneration, ProfileRevision: m.ProfileRevision,
		CreatedAt: m.CreatedAt, Recipient: m.Recipient, Kind: m.Kind, TaskID: m.TaskID, AttemptID: m.AttemptID, ReplyTo: m.ReplyTo, Lifecycle: m.Lifecycle, Deliveries: []TeamDeliveryRecord{}}
	if m.SenderID != "" {
		profile := m.Profile
		out.Profile = &profile
	}
	if includeBodies {
		payload := m.Payload
		out.Payload = &payload
	}
	return out
}

func readDeliveries(tx *sql.Tx, messageID string) ([]TeamDeliveryRecord, error) {
	rows, err := tx.Query(`SELECT session_id,delivered_at,delivery_count,acked_at FROM team_deliveries WHERE message_id=? ORDER BY session_id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamDeliveryRecord{}
	for rows.Next() {
		var r TeamDeliveryRecord
		if err := rows.Scan(&r.SessionID, &r.DeliveredAt, &r.DeliveryCount, &r.AckedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
