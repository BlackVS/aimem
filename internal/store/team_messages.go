package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"aimem/internal/uuidv7"
)

const MaxTeamMessages = 10000

var (
	ErrTeamMessageNotFound    = errors.New("team message not found")
	ErrTeamMessageQuota       = errors.New("team message limit reached")
	ErrTeamMessageUndelivered = errors.New("ack requires a message delivered to this session")
)

type TeamSessionHandle struct {
	SessionID  string `json:"session_id"`
	Generation int64  `json:"generation"`
}

type TeamRecipient struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}
type TeamMessagePayload struct {
	Text     string    `json:"text"`
	Refs     []TaskRef `json:"refs,omitempty"`
	Deadline string    `json:"deadline,omitempty"`
}
type TeamMessageContent struct {
	Recipient TeamRecipient      `json:"recipient"`
	Kind      string             `json:"kind"`
	TaskID    string             `json:"task_id,omitempty"`
	AttemptID string             `json:"attempt_id,omitempty"`
	ReplyTo   string             `json:"reply_to,omitempty"`
	Payload   TeamMessagePayload `json:"payload"`
}

// TeamLifecycle is the hub-authored record inside a lifecycle message: which
// transition happened, to which attempt and task, and who caused it. It is
// informational; the assignment row remains the authority on ownership.
type TeamLifecycle struct {
	Operation             string             `json:"operation"`
	TaskID                string             `json:"task_id,omitempty"`
	AttemptID             string             `json:"attempt_id,omitempty"`
	State                 string             `json:"state,omitempty"`
	TaskState             string             `json:"task_state,omitempty"`
	ActorKind             string             `json:"actor_kind"`
	Session               *TeamSessionHandle `json:"session,omitempty"`   // the acting session when a user acted
	Successor             *TeamSessionHandle `json:"successor,omitempty"` // the new coordinator after a handoff
	CoordinatorGeneration int64              `json:"coordinator_generation,omitempty"`
}

// A hub-authored message (kind lifecycle) has no sender session or profile
// and carries Lifecycle; client sends never set these.
type TeamMessage struct {
	ID               string         `json:"id"`
	Sequence         int64          `json:"sequence"`
	TeamID           string         `json:"team_id"`
	SenderID         string         `json:"sender_id"`
	SenderGeneration int64          `json:"sender_generation"`
	ProfileRevision  int64          `json:"profile_revision"`
	Profile          TeamProfile    `json:"profile"`
	CreatedAt        string         `json:"created_at"`
	Lifecycle        *TeamLifecycle `json:"lifecycle,omitempty"`
	TeamMessageContent
}

func (c *TeamMessageContent) validate() error {
	switch c.Kind {
	case "question", "answer", "note", "progress", "blocker", "review_feedback":
	default:
		return invalid(errors.New("invalid message kind"))
	}
	switch c.Recipient.Kind {
	case "team":
		if c.Recipient.ID != "" {
			return invalid(errors.New("team recipient has no ID"))
		}
	case "member":
		if !taskIDRE.MatchString(c.Recipient.ID) {
			return invalid(errors.New("invalid recipient session"))
		}
	default:
		return invalid(errors.New("recipient must be team or member"))
	}
	if err := taskText(c.Payload.Text, 32<<10, true); err != nil {
		return invalid(err)
	}
	if err := validateRefs("payload.refs", c.Payload.Refs); err != nil {
		return invalid(err)
	}
	if c.Payload.Deadline != "" {
		if c.Kind != "question" {
			return invalid(errors.New("deadline applies only to questions"))
		}
		if _, err := time.Parse(time.RFC3339Nano, c.Payload.Deadline); err != nil {
			return invalid(errors.New("invalid deadline"))
		}
	}
	if c.AttemptID != "" {
		return invalid(errors.New("attempt references unavailable until assignment support"))
	}
	if c.TaskID != "" && !taskIDRE.MatchString(c.TaskID) {
		return invalid(errors.New("invalid task reference"))
	}
	if c.ReplyTo != "" && !taskIDRE.MatchString(c.ReplyTo) {
		return invalid(errors.New("invalid reply reference"))
	}
	if c.Kind == "answer" && c.ReplyTo == "" {
		return invalid(errors.New("answer requires a question reply_to"))
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if len(raw) > 32<<10 {
		return invalid(errors.New("message content exceeds 32 KiB"))
	}
	return nil
}

func currentMessageSession(tx *sql.Tx, teamID string, h TeamSessionHandle, a TaskActor) (Team, TeamSession, error) {
	t, s, err := boundTeamSession(tx, teamID, h.SessionID, a)
	if err != nil {
		return t, s, err
	}
	if s.State != "active" || h.Generation < 1 || s.Generation != h.Generation {
		return t, s, ErrTeamSessionStale
	}
	return t, s, nil
}

func readTeamMessage(q rowQuerier, teamID, id string) (TeamMessage, error) {
	var m TeamMessage
	var body string
	if err := q.QueryRow(`SELECT body FROM team_messages WHERE team_id=? AND id=?`, teamID, id).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return m, ErrTeamMessageNotFound
		}
		return m, err
	}
	err := json.Unmarshal([]byte(body), &m)
	return m, err
}

func messageAudit(tx *sql.Tx, t Team, s TeamSession, op string, ids []string, a TeamAuditContext) error {
	e := TeamEvent{ID: uuidv7.New(), ProtocolVersion: 1, Operation: op, At: nowUTC(), TeamAuditContext: a, Team: t, Session: &s, MessageIDs: ids}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO team_events(team_id,body) VALUES(?,?)`, t.ID, string(b))
	return err
}

// Message delivery authority is checked again when a recipient reads. Snapshot
// only active, currently enrolled sessions; missing heartbeat never means left.
func messageRecipients(tx *sql.Tx, t Team, r TeamRecipient) ([]string, error) {
	rows, err := tx.Query(`SELECT body FROM team_sessions WHERE team_id=? AND state='active' ORDER BY id`, t.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	enrolled := map[string]bool{}
	coordinators := map[string]bool{}
	for _, e := range t.Enrollment {
		enrolled[e.UserID] = true
		coordinators[e.UserID] = e.Coordinator
	}
	ids := []string{}
	for rows.Next() {
		var body string
		var s TeamSession
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			return nil, err
		}
		if !enrolled[s.UserID] || s.Role == "coordinator" && !coordinators[s.UserID] {
			continue
		}
		if r.Kind == "team" || s.ID == r.ID {
			ids = append(ids, s.ID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if r.Kind == "member" && len(ids) != 1 {
		return nil, ErrTeamSessionDenied
	}
	return ids, nil
}

// lifecycleRecipients keeps the given session IDs that are active and still
// enrolled (coordinators must still be designated), dropping duplicates,
// blanks and the excluded session. A departed counterpart simply receives no
// delivery; the message stays readable in team history.
func lifecycleRecipients(tx *sql.Tx, t Team, exclude string, ids ...string) ([]string, error) {
	enrolled := map[string]bool{}
	coordinators := map[string]bool{}
	for _, e := range t.Enrollment {
		enrolled[e.UserID] = true
		coordinators[e.UserID] = e.Coordinator
	}
	seen := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		if id == "" || id == exclude || seen[id] {
			continue
		}
		seen[id] = true
		s, err := readTeamSession(tx, t.ID, id)
		if errors.Is(err, ErrTeamSessionDenied) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if s.State != "active" || !enrolled[s.UserID] || s.Role == "coordinator" && !coordinators[s.UserID] {
			continue
		}
		out = append(out, id)
	}
	return out, nil
}

func activeCoordinatorID(tx *sql.Tx, teamID string) (string, error) {
	var id string
	err := tx.QueryRow(`SELECT id FROM team_sessions WHERE team_id=? AND role='coordinator' AND state='active'`, teamID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// lifecycleMessage writes a hub-authored message and its deliveries inside
// the caller's transaction. It bypasses the client send quota: a transition
// must never fail for lack of inbox capacity. The caller records the returned
// ID on its audit event.
func lifecycleMessage(tx *sql.Tx, teamID string, recipient TeamRecipient, recipients []string, l TeamLifecycle, text string, refs []TaskRef) (string, error) {
	if refs == nil {
		refs = []TaskRef{}
	}
	m := TeamMessage{ID: uuidv7.New(), TeamID: teamID, Profile: TeamProfile{Capabilities: []string{}}, CreatedAt: nowUTC(), Lifecycle: &l,
		TeamMessageContent: TeamMessageContent{Recipient: recipient, Kind: "lifecycle", TaskID: l.TaskID, AttemptID: l.AttemptID, Payload: TeamMessagePayload{Text: text, Refs: refs}}}
	if err := tx.QueryRow(`INSERT INTO team_messages(id,team_id,body) VALUES(?,?,'') RETURNING sequence`, m.ID, teamID).Scan(&m.Sequence); err != nil {
		return "", err
	}
	body, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(`UPDATE team_messages SET body=? WHERE id=?`, string(body), m.ID); err != nil {
		return "", err
	}
	for _, id := range recipients {
		if _, err = tx.Exec(`INSERT INTO team_deliveries(message_id,session_id) VALUES(?,?)`, m.ID, id); err != nil {
			return "", err
		}
	}
	return m.ID, nil
}

// SendTeamMessage persists content once; audit references its ID. Live token and
// project-grant authentication remains the service's responsibility on EVERY call.
func (d *DB) SendTeamMessage(teamID string, h TeamSessionHandle, c TeamMessageContent, a TeamAuditContext, key string, limit int) (TeamMessage, error) {
	if err := c.validate(); err != nil {
		return TeamMessage{}, err
	}
	if limit < 1 || limit > 1000000 {
		return TeamMessage{}, invalid(errors.New("invalid message quota"))
	}
	var t Team
	var s TeamSession
	check := func(tx *sql.Tx) (err error) { t, s, err = boundTeamSession(tx, teamID, h.SessionID, a.Actor); return }
	input := struct {
		Handle  TeamSessionHandle
		Content TeamMessageContent
	}{h, c}
	return checkedTaskMutation(d, a.Actor, "team.message.send", teamID, key, input, check, func(tx *sql.Tx) (TeamMessage, error) {
		if s.State != "active" || h.Generation < 1 || s.Generation != h.Generation {
			return TeamMessage{}, ErrTeamSessionStale
		}
		if c.TaskID != "" {
			if _, err := readTask(tx, c.TaskID); err != nil {
				return TeamMessage{}, err
			}
		}
		for _, ref := range c.Payload.Refs {
			if ref.Kind == "task" {
				if _, err := readTask(tx, ref.Ref); err != nil {
					return TeamMessage{}, err
				}
			}
		}
		if c.ReplyTo != "" {
			reply, err := readTeamMessage(tx, teamID, c.ReplyTo)
			if err != nil {
				return TeamMessage{}, err
			}
			if c.Kind == "answer" && reply.Kind != "question" {
				return TeamMessage{}, invalid(errors.New("answer must reply to a question"))
			}
		}
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM team_messages WHERE team_id=?`, teamID).Scan(&count); err != nil {
			return TeamMessage{}, err
		}
		if count >= limit {
			return TeamMessage{}, ErrTeamMessageQuota
		}
		recipients, err := messageRecipients(tx, t, c.Recipient)
		if err != nil {
			return TeamMessage{}, err
		}
		m := TeamMessage{ID: uuidv7.New(), TeamID: teamID, SenderID: s.ID, SenderGeneration: s.Generation, ProfileRevision: s.ProfileRevision, Profile: s.TeamProfile, CreatedAt: nowUTC(), TeamMessageContent: c}
		if err = tx.QueryRow(`INSERT INTO team_messages(id,team_id,body) VALUES(?,?,'') RETURNING sequence`, m.ID, teamID).Scan(&m.Sequence); err != nil {
			return TeamMessage{}, err
		}
		body, err := json.Marshal(m)
		if err != nil {
			return TeamMessage{}, err
		}
		if _, err = tx.Exec(`UPDATE team_messages SET body=? WHERE id=?`, string(body), m.ID); err != nil {
			return TeamMessage{}, err
		}
		for _, id := range recipients {
			if _, err = tx.Exec(`INSERT INTO team_deliveries(message_id,session_id) VALUES(?,?)`, m.ID, id); err != nil {
				return TeamMessage{}, err
			}
		}
		return m, messageAudit(tx, t, s, "team.message.send", []string{m.ID}, a)
	})
}

type TeamMessagePage struct {
	Messages   []TeamMessage `json:"messages"`
	Next       int64         `json:"next_cursor"`
	HasMore    bool          `json:"has_more"`
	ServerTime string        `json:"server_time"`
}

// TeamMessages reads team-visible history or the caller's unacknowledged inbox.
// Inbox reads durably record an attempted delivery before returning; this is
// NOT proof the client received or understood a response. A lost response is
// retried from the same cursor, or from zero for all still-unacknowledged items.
func (d *DB) TeamMessages(teamID string, h TeamSessionHandle, after int64, limit int, inbox bool, a TeamAuditContext) (TeamMessagePage, error) {
	out := TeamMessagePage{Messages: []TeamMessage{}, Next: after, ServerTime: nowUTC()}
	if err := d.taskScopeOK(); err != nil {
		return out, err
	}
	if after < 0 || limit < 1 || limit > 100 {
		return out, invalid(errors.New("invalid message page"))
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	t, s, err := currentMessageSession(tx, teamID, h, a.Actor)
	if err != nil {
		return out, err
	}
	query := `SELECT m.body FROM team_messages m WHERE m.team_id=? AND m.sequence>?`
	args := []any{teamID, after}
	if inbox {
		query += ` AND EXISTS(SELECT 1 FROM team_deliveries d WHERE d.message_id=m.id AND d.session_id=? AND d.acked_at='')`
		args = append(args, s.ID)
	}
	query += ` ORDER BY m.sequence LIMIT ?`
	args = append(args, limit+1)
	rows, err := tx.Query(query, args...)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var body string
		var m TeamMessage
		if err = rows.Scan(&body); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(body), &m); err != nil {
			break
		}
		out.Messages = append(out.Messages, m)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return out, err
	}
	if len(out.Messages) > limit {
		out.HasMore = true
		out.Messages = out.Messages[:limit]
	}
	ids := []string{}
	for _, m := range out.Messages {
		out.Next = m.Sequence
		if inbox {
			if _, err = tx.Exec(`UPDATE team_deliveries SET delivered_at=?,delivery_count=delivery_count+1 WHERE message_id=? AND session_id=?`, out.ServerTime, m.ID, s.ID); err != nil {
				return out, err
			}
			ids = append(ids, m.ID)
		}
	}
	if len(ids) > 0 {
		if err = messageAudit(tx, t, s, "team.message.delivery_attempt", ids, a); err != nil {
			return out, err
		}
	}
	return out, tx.Commit()
}

type TeamAck struct {
	MessageIDs []string `json:"message_ids"`
}

// AckTeamMessages cannot acknowledge unseen messages, another inbox or a whole
// cursor range. Duplicate commands may return receipts; new commands need the
// current generation. Already acknowledged IDs remain idempotent.
func (d *DB) AckTeamMessages(teamID string, h TeamSessionHandle, ids []string, a TeamAuditContext, key string) (TeamAck, error) {
	if len(ids) < 1 || len(ids) > 100 {
		return TeamAck{}, invalid(errors.New("ack needs 1-100 message IDs"))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !taskIDRE.MatchString(id) || seen[id] {
			return TeamAck{}, invalid(errors.New("ack needs unique message IDs"))
		}
		seen[id] = true
	}
	var t Team
	var s TeamSession
	check := func(tx *sql.Tx) (err error) { t, s, err = boundTeamSession(tx, teamID, h.SessionID, a.Actor); return }
	input := struct {
		Handle TeamSessionHandle
		IDs    []string
	}{h, ids}
	return checkedTaskMutation(d, a.Actor, "team.message.ack", teamID, key, input, check, func(tx *sql.Tx) (TeamAck, error) {
		if s.State != "active" || h.Generation < 1 || s.Generation != h.Generation {
			return TeamAck{}, ErrTeamSessionStale
		}
		changed := []string{}
		for _, id := range ids {
			var delivered, acked string
			err := tx.QueryRow(`SELECT d.delivered_at,d.acked_at FROM team_deliveries d JOIN team_messages m ON m.id=d.message_id WHERE m.team_id=? AND d.message_id=? AND d.session_id=?`, teamID, id, s.ID).Scan(&delivered, &acked)
			if errors.Is(err, sql.ErrNoRows) || err == nil && delivered == "" {
				return TeamAck{}, ErrTeamMessageUndelivered
			}
			if err != nil {
				return TeamAck{}, err
			}
			if acked != "" {
				continue
			}
			if _, err = tx.Exec(`UPDATE team_deliveries SET acked_at=? WHERE message_id=? AND session_id=?`, nowUTC(), id, s.ID); err != nil {
				return TeamAck{}, err
			}
			changed = append(changed, id)
		}
		if len(changed) > 0 {
			if err := messageAudit(tx, t, s, "team.message.ack", changed, a); err != nil {
				return TeamAck{}, err
			}
		}
		return TeamAck{MessageIDs: ids}, nil
	})
}
