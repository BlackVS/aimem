package store

// The fact-staleness review loop (FEATURE-PROPOSALS #5). The knowledge
// base's write side already resolves conflicts (newest-wins) — but a
// fact nobody has contradicted can still quietly rot. The review queue
// surfaces active, unpinned facts that are old, thinly corroborated,
// and untouched since; a reviewer then confirms (audited touch +
// confidence), supersedes, or expires each one.
//
// The queue is DERIVED, never stored: staleness is a query over the
// audit trail, and every review verdict is an ordinary audited write
// that naturally drops the fact out of the queue. Nothing to maintain,
// nothing to let rot — the same reasoning that keeps label-relation
// graphs unstored (DESIGN.md).

import (
	"database/sql"
	"errors"
	"fmt"
)

// Review defaults: a fact untouched for a month with at most this many
// corroborating sources deserves a look.
const (
	DefaultReviewAgeDays          = 30
	DefaultReviewMaxCorroboration = 2
)

// ReviewItem is one stale candidate. LastSeen is the newest moment the
// fact was asserted or validated (remember / reassert / confirm audit
// rows; creation time for facts that arrived by sync with no local
// audit history).
type ReviewItem struct {
	ID            string  `json:"id"`
	Text          string  `json:"text"`
	Kind          string  `json:"kind"`
	Confidence    float64 `json:"confidence"`
	CreatedAt     string  `json:"created_at"`
	Actor         string  `json:"actor,omitempty"`
	Corroboration int     `json:"corroboration"`
	LastSeen      string  `json:"last_seen"`
}

// ReviewQueue lists active, unpinned facts whose last assertion or
// validation predates cutoff (RFC3339) and whose corroboration is at
// most maxCorroboration — oldest and least confident first, so the
// riskiest knowledge is reviewed before the merely aging.
func (d *DB) ReviewQueue(cutoff string, maxCorroboration, limit int) ([]ReviewItem, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if maxCorroboration < 0 {
		maxCorroboration = DefaultReviewMaxCorroboration
	}
	rows, err := d.sql.Query(`SELECT * FROM (
		SELECT m.id, m.text, m.kind, m.confidence, m.created_at, m.actor,
		  (SELECT COUNT(*) FROM memory_sources s WHERE s.memory_id = m.id) AS corroboration,
		  COALESCE((SELECT MAX(a.ts) FROM memory_audit a WHERE a.memory_id = m.id
		            AND a.op IN ('remember','reassert','confirm')), m.created_at) AS last_seen
		FROM memories m
		WHERE m.expired_at IS NULL AND m.superseded_by IS NULL AND m.pinned = 0
	) WHERE corroboration <= ? AND last_seen < ?
	ORDER BY last_seen ASC, confidence ASC LIMIT ?`,
		maxCorroboration, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReviewItem
	for rows.Next() {
		var it ReviewItem
		if err := rows.Scan(&it.ID, &it.Text, &it.Kind, &it.Confidence,
			&it.CreatedAt, &it.Actor, &it.Corroboration, &it.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Confirm records that a reviewer checked a fact and found it still
// true: an audited touch (which removes it from the review queue until
// the age window passes again) and a modest confidence reinforcement —
// smaller than a reassertion's, because "still true" is weaker evidence
// than "independently asserted again".
func (d *DB) Confirm(id, actor string) error {
	var text string
	err := d.sql.QueryRow(`SELECT text FROM memories
		WHERE id=? AND expired_at IS NULL AND superseded_by IS NULL`, id).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no active memory %s", id)
	}
	if err != nil {
		return err
	}
	if _, err := d.sql.Exec(`UPDATE memories SET confidence=MIN(1.0, confidence+0.1) WHERE id=?`, id); err != nil {
		return err
	}
	return d.audit(id, "confirm", "", text, actor)
}
