package store

// Shared documents (docs/DESIGN-shared-docs.md): whole, authored files —
// the handoff, a runbook — that every machine on a project should see the
// newest version of. Deliberately NOT memories: a fact is a disposable
// assertion that dedup, supersession and ranking act on correctly; a
// document is a whole that those operations would destroy. Writes are
// compare-and-swap on a per-doc revision — never newest-wins, which is
// safe for the generated design_doc and destructive for authored text.

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"aimem/internal/redact"
)

// MaxDocBytes refuses runaway documents: a bound file rides the checkpoint
// path, so an unbounded one would tax every turn. WarnDocBytes is the
// caller-side nagging threshold.
const (
	MaxDocBytes  = 256 * 1024
	WarnDocBytes = 64 * 1024
	// docHistoryKeep bounds doc_revisions per doc. This is a convenience
	// for "what did it say before", not an archive — git remains the
	// archive for anything committed.
	docHistoryKeep = 20
)

var docNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Doc is one shared document (or one revision of it, from DocLog).
type Doc struct {
	Name      string `json:"name"`
	Body      string `json:"body,omitempty"`
	Rev       int64  `json:"rev"`
	UpdatedAt string `json:"updated_at"`
	UpdatedBy string `json:"updated_by"`
	Deleted   bool   `json:"deleted,omitempty"`
	Size      int    `json:"size"`
}

// DocConflict is the CAS refusal: the caller's base_rev is stale. It
// carries the current document so the caller can merge — refusing without
// handing back the other side would leave nothing to merge WITH.
type DocConflict struct {
	Current Doc
}

func (e *DocConflict) Error() string {
	if e.Current.Deleted {
		return fmt.Sprintf("document was deleted at rev %d by %s", e.Current.Rev, e.Current.UpdatedBy)
	}
	return fmt.Sprintf("stale base_rev: document is at rev %d (updated %s by %s)",
		e.Current.Rev, e.Current.UpdatedAt, e.Current.UpdatedBy)
}

// PutDoc writes a document revision with compare-and-swap semantics:
// baseRev must equal the current revision (0 for a new document). Two
// deliberate softenings keep retries safe: an IDENTICAL body succeeds with
// the current rev regardless of baseRev (a re-fired hook or a crash
// between write and ack is then a no-op, not a spurious conflict), and a
// stale write returns *DocConflict carrying the current doc. deleted=true
// writes a tombstone: the row stays, the body empties, and a later push
// from a machine that still has the file conflicts by name instead of
// silently resurrecting the document.
func (d *DB) PutDoc(name, body, updatedBy string, baseRev int64, deleted bool) (Doc, error) {
	if !docNameRe.MatchString(name) {
		return Doc{}, fmt.Errorf("invalid document name %q (want letters/digits/._-, max 64)", name)
	}
	if len(body) > MaxDocBytes {
		return Doc{}, fmt.Errorf("document %q is %d bytes; the limit is %d — this store is for handoffs and runbooks, not archives", name, len(body), MaxDocBytes)
	}
	// A pasted secret must not fan out to every machine on the project.
	// Authored text is otherwise accepted as written (silently redacting
	// a document would corrupt intended content), so only the unambiguous
	// shapes refuse — private keys and recognised vendor token formats;
	// softer matches are the caller's warning (adapter.PublishDocs).
	if _, refuse := redact.ScanAuthored(body); len(refuse) > 0 {
		return Doc{}, fmt.Errorf("document %q contains secret-shaped content (%s); refusing to publish it — remove the secret first",
			name, strings.Join(refuse, ", "))
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return Doc{}, err
	}
	defer tx.Rollback()

	cur, err := scanDoc(tx.QueryRow(
		`SELECT name, body, rev, updated_at, updated_by, deleted FROM docs WHERE name=?`, name))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if deleted {
			return Doc{}, fmt.Errorf("no such document %q", name)
		}
		if baseRev != 0 {
			return Doc{}, fmt.Errorf("no such document %q (base_rev %d; use 0 to create)", name, baseRev)
		}
	case err != nil:
		return Doc{}, err
	default:
		if body == cur.Body && deleted == cur.Deleted {
			return cur, nil // idempotent: retries of the same write are no-ops
		}
		if baseRev != cur.Rev {
			return Doc{}, &DocConflict{Current: cur}
		}
	}

	next := Doc{
		Name: name, Body: body, Rev: baseRev + 1,
		UpdatedAt: nowUTC(), UpdatedBy: updatedBy, Deleted: deleted,
		Size: len(body),
	}
	if deleted {
		next.Body, next.Size = "", 0
	}
	if _, err := tx.Exec(`INSERT INTO docs(name, body, rev, updated_at, updated_by, deleted)
			VALUES(?,?,?,?,?,?)
			ON CONFLICT(name) DO UPDATE SET body=excluded.body, rev=excluded.rev,
			  updated_at=excluded.updated_at, updated_by=excluded.updated_by, deleted=excluded.deleted`,
		next.Name, next.Body, next.Rev, next.UpdatedAt, next.UpdatedBy, boolInt(next.Deleted)); err != nil {
		return Doc{}, err
	}
	if _, err := tx.Exec(`INSERT INTO doc_revisions(name, rev, body, updated_at, updated_by, deleted)
			VALUES(?,?,?,?,?,?)`,
		next.Name, next.Rev, next.Body, next.UpdatedAt, next.UpdatedBy, boolInt(next.Deleted)); err != nil {
		return Doc{}, err
	}
	if _, err := tx.Exec(`DELETE FROM doc_revisions WHERE name=? AND rev <= ?`,
		next.Name, next.Rev-docHistoryKeep); err != nil {
		return Doc{}, err
	}
	return next, tx.Commit()
}

// DocMatch is one search hit inside a shared document: enough to know
// WHICH document to fetch — retrieval stays fetch-by-name and whole,
// never ranked alongside facts (DESIGN-shared-docs).
type DocMatch struct {
	Name      string `json:"name"`
	Rev       int64  `json:"rev"`
	UpdatedAt string `json:"updated_at"`
	Snippet   string `json:"snippet"`
}

// SearchDocs finds live documents whose name or body contains EVERY
// term, case-insensitively. Deliberately an exact scan, not FTS: a
// project holds a handful of documents capped at 256KB, and at that
// scale scanning beats a virtual table with triggers on an
// upsert-and-tombstone base table in both simplicity and correctness.
// If doc counts ever grow past that shape, an FTS index is the
// recorded upgrade path (FEATURE-PROPOSALS #4).
func (d *DB) SearchDocs(q string, limit int) ([]DocMatch, error) {
	terms := strings.Fields(strings.ToLower(q))
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	rows, err := d.sql.Query(`SELECT name, body, rev, updated_at FROM docs
		WHERE deleted = 0 ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocMatch
	for rows.Next() {
		var name, body, updatedAt string
		var rev int64
		if err := rows.Scan(&name, &body, &rev, &updatedAt); err != nil {
			return nil, err
		}
		lowBody, lowName := strings.ToLower(body), strings.ToLower(name)
		all := true
		first := -1
		for _, t := range terms {
			i := strings.Index(lowBody, t)
			if i < 0 && !strings.Contains(lowName, t) {
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
		out = append(out, DocMatch{Name: name, Rev: rev, UpdatedAt: updatedAt,
			Snippet: docSnippet(body, first)})
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// docSnippet excerpts ~180 chars around the first hit (start of body
// for name-only matches), newlines flattened so a snippet is one line.
func docSnippet(body string, pos int) string {
	pos = max(pos, 0)
	start := max(pos-60, 0)
	end := min(pos+120, len(body))
	// Never split UTF-8 runes at either cut.
	for start > 0 && body[start] >= 0x80 && body[start] < 0xC0 {
		start--
	}
	for end < len(body) && body[end] >= 0x80 && body[end] < 0xC0 {
		end++
	}
	s := strings.Join(strings.Fields(body[start:end]), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(body) {
		s += "…"
	}
	return s
}

// GetDoc returns the current document, or the named historical revision
// when rev > 0. Deleted documents return with the flag set — the caller
// decides whether a tombstone reads as absence.
func (d *DB) GetDoc(name string, rev int64) (Doc, error) {
	var row *sql.Row
	if rev > 0 {
		row = d.sql.QueryRow(`SELECT name, body, rev, updated_at, updated_by, deleted
			FROM doc_revisions WHERE name=? AND rev=?`, name, rev)
	} else {
		row = d.sql.QueryRow(`SELECT name, body, rev, updated_at, updated_by, deleted
			FROM docs WHERE name=?`, name)
	}
	doc, err := scanDoc(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Doc{}, fmt.Errorf("no such document %q", name)
	}
	return doc, err
}

// ListDocs enumerates documents without their bodies (Size stands in).
func (d *DB) ListDocs() ([]Doc, error) {
	rows, err := d.sql.Query(`SELECT name, '', rev, updated_at, updated_by, deleted, LENGTH(body)
		FROM docs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Doc
	for rows.Next() {
		var doc Doc
		var del int
		if err := rows.Scan(&doc.Name, &doc.Body, &doc.Rev, &doc.UpdatedAt, &doc.UpdatedBy, &del, &doc.Size); err != nil {
			return nil, err
		}
		doc.Deleted = del != 0
		out = append(out, doc)
	}
	return out, rows.Err()
}

// DocLog returns the retained revisions of one document, newest first,
// without bodies (GetDoc with a rev fetches one).
func (d *DB) DocLog(name string) ([]Doc, error) {
	rows, err := d.sql.Query(`SELECT name, '', rev, updated_at, updated_by, deleted, LENGTH(body)
		FROM doc_revisions WHERE name=? ORDER BY rev DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Doc
	for rows.Next() {
		var doc Doc
		var del int
		if err := rows.Scan(&doc.Name, &doc.Body, &doc.Rev, &doc.UpdatedAt, &doc.UpdatedBy, &del, &doc.Size); err != nil {
			return nil, err
		}
		doc.Deleted = del != 0
		out = append(out, doc)
	}
	return out, rows.Err()
}

func scanDoc(row *sql.Row) (Doc, error) {
	var doc Doc
	var del int
	err := row.Scan(&doc.Name, &doc.Body, &doc.Rev, &doc.UpdatedAt, &doc.UpdatedBy, &del)
	doc.Deleted = del != 0
	doc.Size = len(doc.Body)
	return doc, err
}
