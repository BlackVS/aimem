package store

import (
	"database/sql"
	"errors"
)

// TaskStateChange is one entry of the board feed: a task's state as one
// revision set it, and the state before. From is empty for a task's
// creation. Sequence orders the project's entries and is the cursor.
type TaskStateChange struct {
	Sequence int64  `json:"-"`
	TaskID   string `json:"task_id"`
	Revision int64  `json:"revision"`
	From     string `json:"from"`
	To       string `json:"to"`
	At       string `json:"at"`
	// RequiredCapability is the task's at that revision, read from its
	// history snapshot.
	RequiredCapability string `json:"required_capability"`
}

// recordStateChange appends the feed entry of a write that created the
// task or changed its state, in the write's transaction.
func recordStateChange(tx *sql.Tx, t Task, from string, create bool) error {
	if !create && from == t.State {
		return nil
	}
	_, err := tx.Exec(`INSERT INTO task_state_changes(task_id, revision, from_state, to_state, at) VALUES(?,?,?,?,?)`,
		t.ID, t.Revision, from, t.State, t.UpdatedAt)
	return err
}

// TaskStateChanges returns up to limit entries after the given sequence, in
// sequence order, and the project's last sequence. One statement reads
// both, so they come from one snapshot and a cursor past the last
// sequence is recognised.
func (d *DB) TaskStateChanges(after int64, limit int) ([]TaskStateChange, int64, error) {
	if after < 0 || limit < 1 {
		return nil, 0, invalid(errors.New("invalid state change cursor or limit"))
	}
	rows, err := d.sql.Query(`SELECT l.last, c.sequence, c.task_id, c.revision, c.from_state, c.to_state, c.at,
  (SELECT json_extract(h.body, '$.task.required_capability') FROM task_history h WHERE h.task_id = c.task_id AND h.revision = c.revision)
FROM (SELECT COALESCE(MAX(sequence), 0) AS last FROM task_state_changes) l
LEFT JOIN (SELECT * FROM task_state_changes WHERE sequence > ? ORDER BY sequence LIMIT ?) c
ORDER BY c.sequence`, after, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []TaskStateChange{}
	var last int64
	for rows.Next() {
		var seq, rev sql.NullInt64
		var task, from, to, at, capability sql.NullString
		if err := rows.Scan(&last, &seq, &task, &rev, &from, &to, &at, &capability); err != nil {
			return nil, 0, err
		}
		if seq.Valid {
			out = append(out, TaskStateChange{seq.Int64, task.String, rev.Int64, from.String, to.String, at.String, capability.String})
		}
	}
	return out, last, rows.Err()
}
