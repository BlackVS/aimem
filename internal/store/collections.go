package store

// Structured collections (docs/DESIGN-structured-docs.md): named sets of
// small JSON records with slash-path ids forming a tree — the authored
// structured state of a product (an API surface, a config matrix), kept
// live on the hub by many concurrent writers. The unit of compare-and-swap
// is the RECORD, not a file: writers touching different records never
// conflict, which is the entire point of the design. Deliberately NOT
// documents (no whole to merge) and NOT memories (authored, not distilled;
// no curator ever touches a collection). Markdown is a downstream build
// artifact rendered by the client, never stored here.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"aimem/internal/redact"
)

const (
	// MaxRecordBytes keeps records entry-sized: anything bigger belongs in
	// a shared doc or the repo (DESIGN-structured-docs open question 1).
	MaxRecordBytes = 32 * 1024
	// colHistoryKeep bounds col_revisions per record, like docHistoryKeep:
	// a convenience for "what did it say", not an archive — release cuts
	// in git are the archive.
	colHistoryKeep = 20
)

// Record ids are slash-separated path segments, each shaped like a doc
// name — the tree structure a reference wiki renders from.
var recordSegRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func validRecordID(id string) error {
	if id == "" || len(id) > 512 {
		return fmt.Errorf("invalid record id %q (1-512 chars)", id)
	}
	segs := strings.Split(id, "/")
	if len(segs) > 8 {
		return fmt.Errorf("invalid record id %q (max 8 path segments)", id)
	}
	for _, s := range segs {
		if !recordSegRe.MatchString(s) {
			return fmt.Errorf("invalid record id %q: segment %q (want letters/digits/._- per segment, max 64)", id, s)
		}
	}
	return nil
}

// Record is one entry of a collection (or one revision of it).
type Record struct {
	Collection string          `json:"collection,omitempty"`
	ID         string          `json:"id"`
	Body       json.RawMessage `json:"body,omitempty"`
	Rev        int64           `json:"rev"`
	UpdatedAt  string          `json:"updated_at"`
	UpdatedBy  string          `json:"updated_by"`
	Deleted    bool            `json:"deleted,omitempty"`
	Size       int             `json:"size"`
}

// ColSummary is one collection in a listing: existence is derived from
// records — there is no separate registry to fall out of sync with.
type ColSummary struct {
	Name      string `json:"name"`
	Records   int    `json:"records"`
	UpdatedAt string `json:"updated_at"`
}

// RecordConflict is the CAS refusal, carrying the current record so the
// caller can re-apply its intent — a record is small enough that "re-read
// one record and redo" replaces merge machinery entirely.
type RecordConflict struct {
	Current Record
}

func (e *RecordConflict) Error() string {
	if e.Current.Deleted {
		return fmt.Sprintf("record was deleted at rev %d by %s", e.Current.Rev, e.Current.UpdatedBy)
	}
	return fmt.Sprintf("stale base_rev: record is at rev %d (updated %s by %s)",
		e.Current.Rev, e.Current.UpdatedAt, e.Current.UpdatedBy)
}

// PutRecord writes one record revision with the same CAS contract as
// PutDoc: baseRev must match (0 creates), an identical body is an
// idempotent no-op at the current rev, a stale write returns
// *RecordConflict with the current record, deleted=true tombstones.
// Bodies must be JSON objects — records are structured entries, and a
// blob that isn't one belongs in a shared document.
func (d *DB) PutRecord(collection, id string, body []byte, updatedBy string, baseRev int64, deleted bool) (Record, error) {
	if !docNameRe.MatchString(collection) {
		return Record{}, fmt.Errorf("invalid collection name %q (want letters/digits/._-, max 64)", collection)
	}
	if err := validRecordID(id); err != nil {
		return Record{}, err
	}
	if deleted {
		body = []byte("{}")
	}
	if len(body) > MaxRecordBytes {
		return Record{}, fmt.Errorf("record %s/%s is %d bytes; the limit is %d — records are entries, not documents",
			collection, id, len(body), MaxRecordBytes)
	}
	trimmed := strings.TrimSpace(string(body))
	if !json.Valid(body) || !strings.HasPrefix(trimmed, "{") {
		return Record{}, fmt.Errorf("record body must be a JSON object")
	}
	// Same egress guard as documents: a pasted secret must not fan out.
	if _, refuse := redact.ScanAuthored(string(body)); len(refuse) > 0 {
		return Record{}, fmt.Errorf("record %s/%s contains secret-shaped content (%s); refusing to store it",
			collection, id, strings.Join(refuse, ", "))
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return Record{}, err
	}
	defer tx.Rollback()

	cur, err := scanRecord(tx.QueryRow(
		`SELECT collection, id, body, rev, updated_at, updated_by, deleted
			FROM col_records WHERE collection=? AND id=?`, collection, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if deleted {
			return Record{}, fmt.Errorf("no such record %s/%s", collection, id)
		}
		if baseRev != 0 {
			return Record{}, fmt.Errorf("no such record %s/%s (base_rev %d; use 0 to create)", collection, id, baseRev)
		}
	case err != nil:
		return Record{}, err
	default:
		if string(cur.Body) == string(body) && deleted == cur.Deleted {
			return cur, nil // idempotent retry
		}
		if baseRev != cur.Rev {
			return Record{}, &RecordConflict{Current: cur}
		}
	}

	next := Record{
		Collection: collection, ID: id, Body: body, Rev: baseRev + 1,
		UpdatedAt: nowUTC(), UpdatedBy: updatedBy, Deleted: deleted,
		Size: len(body),
	}
	if _, err := tx.Exec(`INSERT INTO col_records(collection, id, body, rev, updated_at, updated_by, deleted)
			VALUES(?,?,?,?,?,?,?)
			ON CONFLICT(collection, id) DO UPDATE SET body=excluded.body, rev=excluded.rev,
			  updated_at=excluded.updated_at, updated_by=excluded.updated_by, deleted=excluded.deleted`,
		next.Collection, next.ID, string(next.Body), next.Rev, next.UpdatedAt, next.UpdatedBy, boolInt(next.Deleted)); err != nil {
		return Record{}, err
	}
	if _, err := tx.Exec(`INSERT INTO col_revisions(collection, id, rev, body, updated_at, updated_by, deleted)
			VALUES(?,?,?,?,?,?,?)`,
		next.Collection, next.ID, next.Rev, string(next.Body), next.UpdatedAt, next.UpdatedBy, boolInt(next.Deleted)); err != nil {
		return Record{}, err
	}
	if _, err := tx.Exec(`DELETE FROM col_revisions WHERE collection=? AND id=? AND rev <= ?`,
		next.Collection, next.ID, next.Rev-colHistoryKeep); err != nil {
		return Record{}, err
	}
	return next, tx.Commit()
}

// GetRecord returns the current record, or the named historical revision
// when rev > 0. Tombstones return with the flag set.
func (d *DB) GetRecord(collection, id string, rev int64) (Record, error) {
	var row *sql.Row
	if rev > 0 {
		row = d.sql.QueryRow(`SELECT collection, id, body, rev, updated_at, updated_by, deleted
			FROM col_revisions WHERE collection=? AND id=? AND rev=?`, collection, id, rev)
	} else {
		row = d.sql.QueryRow(`SELECT collection, id, body, rev, updated_at, updated_by, deleted
			FROM col_records WHERE collection=? AND id=?`, collection, id)
	}
	rec, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, fmt.Errorf("no such record %s/%s", collection, id)
	}
	return rec, err
}

// ListRecords enumerates a collection in id order (which IS tree order,
// since ids are slash paths). withBodies=false substitutes Size — a
// listing; withBodies=true is the render/export fetch.
func (d *DB) ListRecords(collection string, withBodies bool) ([]Record, error) {
	col := `'{}'`
	if withBodies {
		col = "body"
	}
	rows, err := d.sql.Query(`SELECT collection, id, `+col+`, rev, updated_at, updated_by, deleted, LENGTH(body)
		FROM col_records WHERE collection=? ORDER BY id`, collection)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var rec Record
		var body string
		var del int
		if err := rows.Scan(&rec.Collection, &rec.ID, &body, &rec.Rev, &rec.UpdatedAt, &rec.UpdatedBy, &del, &rec.Size); err != nil {
			return nil, err
		}
		rec.Body, rec.Deleted = json.RawMessage(body), del != 0
		if !withBodies {
			rec.Body = nil
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ListCollections summarizes every collection (live records only counted;
// a fully tombstoned collection still lists, so its history stays
// reachable until the operator drops the project).
func (d *DB) ListCollections() ([]ColSummary, error) {
	rows, err := d.sql.Query(`SELECT collection, SUM(deleted=0), MAX(updated_at)
		FROM col_records GROUP BY collection ORDER BY collection`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColSummary
	for rows.Next() {
		var c ColSummary
		if err := rows.Scan(&c.Name, &c.Records, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecordLog returns the retained revisions of one record, newest first,
// without bodies (GetRecord with a rev fetches one) — the same shape
// DocLog gives documents, because history you retain but cannot
// enumerate is history you do not have.
func (d *DB) RecordLog(collection, id string) ([]Record, error) {
	rows, err := d.sql.Query(`SELECT collection, id, '{}', rev, updated_at, updated_by, deleted, LENGTH(body)
		FROM col_revisions WHERE collection=? AND id=? ORDER BY rev DESC`, collection, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var rec Record
		var body string
		var del int
		if err := rows.Scan(&rec.Collection, &rec.ID, &body, &rec.Rev, &rec.UpdatedAt, &rec.UpdatedBy, &del, &rec.Size); err != nil {
			return nil, err
		}
		rec.Deleted = del != 0
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ColMatch is one search hit inside a collection record: enough to know
// WHICH record to fetch — like DocMatch, retrieval stays fetch-by-id.
type ColMatch struct {
	Collection string `json:"collection"`
	ID         string `json:"id"`
	Rev        int64  `json:"rev"`
	UpdatedAt  string `json:"updated_at"`
	Snippet    string `json:"snippet"`
}

// SearchRecords finds live records whose id or body contains EVERY term,
// case-insensitively. The same deliberate exact scan as SearchDocs and
// for the same reason: records are 32KB-capped entries at a scale where
// scanning beats an FTS table over an upsert-and-tombstone base; the
// recorded upgrade path is shared with docs.
func (d *DB) SearchRecords(q string, limit int) ([]ColMatch, error) {
	terms := strings.Fields(strings.ToLower(q))
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := d.sql.Query(`SELECT collection, id, body, rev, updated_at FROM col_records
		WHERE deleted = 0 ORDER BY collection, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColMatch
	for rows.Next() {
		var col, id, body, updatedAt string
		var rev int64
		if err := rows.Scan(&col, &id, &body, &rev, &updatedAt); err != nil {
			return nil, err
		}
		lowBody, lowID := strings.ToLower(body), strings.ToLower(col+"/"+id)
		all := true
		first := -1
		for _, t := range terms {
			i := strings.Index(lowBody, t)
			if i < 0 && !strings.Contains(lowID, t) {
				all = false
				break
			}
			if i >= 0 && (first < 0 || i < first) {
				first = i
			}
		}
		if !all {
			continue
		}
		out = append(out, ColMatch{Collection: col, ID: id, Rev: rev, UpdatedAt: updatedAt,
			Snippet: docSnippet(body, first)})
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

func scanRecord(row *sql.Row) (Record, error) {
	var rec Record
	var body string
	var del int
	err := row.Scan(&rec.Collection, &rec.ID, &body, &rec.Rev, &rec.UpdatedAt, &rec.UpdatedBy, &del)
	rec.Body, rec.Deleted = json.RawMessage(body), del != 0
	rec.Size = len(body)
	return rec, err
}
