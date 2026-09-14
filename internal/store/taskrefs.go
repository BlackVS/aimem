package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"aimem/internal/schema"
)

// Typed references (docs/DESIGN-kanban-docs.md, "Typed references"): a
// task's candidate and evidence references name what they point at, so
// the board can link and the agents can check, instead of parsing prose.
// Rendering is the page's: it links only an http(s) URL of the external
// kinds and a task id (internal/server/tasks.html, refHTML); validation
// here guarantees those are the only values those kinds can hold.
// The string form shipped in v0.4.0 is gone (a pre-1.0 break): schema 13
// rewrites every stored string to a typed reference and recomputes the
// retry receipts, and a write that still sends strings is refused with a
// message naming the new shape.

// TaskRef is one typed reference.
type TaskRef struct {
	Kind  string `json:"kind"`
	Ref   string `json:"ref"`
	Note  string `json:"note,omitempty"`
	Scope string `json:"scope,omitempty"` // doc, record: the project or group the target lives in; "" is this project
}

// TaskRefKinds are the reference kinds and what ref must hold for each.
var TaskRefKinds = []string{"task", "doc", "record", "commit", "pr", "ci", "url", "text"}

// MaxTaskRefNoteBytes bounds a reference's note.
const MaxTaskRefNoteBytes = 512

// ErrLegacyTaskRef is the decode error for the pre-schema-13 string form.
var ErrLegacyTaskRef = errors.New("references are objects {kind, ref, note?, scope?} with kind one of task, doc, record, commit, pr, ci, url, text; a bare string is no longer accepted")

// UnmarshalJSON refuses the legacy string form with the reason, so a
// client still sending strings learns the shape instead of a type error.
// Unknown keys are errors here as they are one level up: under a
// replace-all update a misspelled "note" must not silently clear one.
func (r *TaskRef) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return ErrLegacyTaskRef
	}
	type plain TaskRef
	var p plain
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return err
	}
	*r = TaskRef(p)
	return nil
}

func httpURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	return true
}

func (r *TaskRef) validate(field string, i int) error {
	where := fmt.Sprintf("%s[%d]", field, i)
	kindOK := false
	for _, k := range TaskRefKinds {
		if r.Kind == k {
			kindOK = true
		}
	}
	if !kindOK {
		return fmt.Errorf("%s: kind must be one of %s", where, strings.Join(TaskRefKinds, ", "))
	}
	if err := taskText(r.Ref, MaxTaskRefBytes, true); err != nil {
		return fmt.Errorf("%s: ref: %w", where, err)
	}
	if err := taskText(r.Note, MaxTaskRefNoteBytes, false); err != nil {
		return fmt.Errorf("%s: note: %w", where, err)
	}
	if r.Scope != "" {
		if r.Kind != "doc" && r.Kind != "record" {
			return fmt.Errorf("%s: scope applies to doc and record references only", where)
		}
		if !schema.ValidProjectID(r.Scope) {
			return fmt.Errorf("%s: scope must be a project or group id", where)
		}
	}
	switch r.Kind {
	case "task":
		if !taskIDRE.MatchString(r.Ref) {
			return fmt.Errorf("%s: a task reference is a task id", where)
		}
	case "doc":
		if !docNameRe.MatchString(r.Ref) {
			return fmt.Errorf("%s: a doc reference is a document name (letters/digits/._-, at most 64)", where)
		}
	case "record":
		col, id, ok := strings.Cut(r.Ref, "/")
		if !ok || !recordSegRe.MatchString(col) || validRecordID(id) != nil {
			return fmt.Errorf("%s: a record reference is <collection>/<record id>", where)
		}
	case "commit", "pr", "ci", "url":
		if !httpURL(r.Ref) {
			return fmt.Errorf("%s: a %s reference is an http(s) URL naming the service and the target (a bare number or hash is not enough)", where, r.Kind)
		}
	}
	return nil
}

func validateRefs(field string, refs []TaskRef) error {
	if len(refs) > MaxTaskListEntries {
		return fmt.Errorf("at most %d entries per dependency or reference list", MaxTaskListEntries)
	}
	for i := range refs {
		if err := refs[i].validate(field, i); err != nil {
			return err
		}
	}
	return nil
}

// LegacyRef is the migration's rule for a pre-schema-13 string: a valid
// HTTP(S) URL becomes a url reference, anything else becomes text with
// its exact content — never a guessed identity.
func LegacyRef(s string) TaskRef {
	if httpURL(s) {
		return TaskRef{Kind: "url", Ref: s}
	}
	return TaskRef{Kind: "text", Ref: s}
}

// migrateTypedRefs is the schema-13 step: every stored string reference —
// in the current snapshots, the history rows and the saved retry
// results — is rewritten with LegacyRef, and every task receipt's digest
// is recomputed from its saved result, so a request committed before the
// upgrade replays as its typed retry (docs/DESIGN-kanban-docs.md,
// "Compatibility policy"). One transaction with the version bump.
func (d *DB) migrateTypedRefs() error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := rewriteRefsIn(tx, `SELECT id, body FROM tasks`, `UPDATE tasks SET body=? WHERE id=?`, func(m map[string]any) { convertRefs(m) }); err != nil {
		return fmt.Errorf("tasks: %w", err)
	}
	if err := rewriteRefsIn(tx, `SELECT rowid, body FROM task_history`, `UPDATE task_history SET body=? WHERE rowid=?`, func(m map[string]any) {
		if t, ok := m["task"].(map[string]any); ok {
			convertRefs(t)
		}
	}); err != nil {
		return fmt.Errorf("task_history: %w", err)
	}
	// Receipts: rewrite the saved result, then recompute the digest from
	// it — the input of a create is the created task's content, of an
	// update the updated content with expected = revision - 1.
	rows, err := tx.Query(`SELECT rowid, operation, result FROM task_requests WHERE operation IN ('create','update')`)
	if err != nil {
		return err
	}
	type receipt struct {
		rowid  int64
		op     string
		result string
	}
	var receipts []receipt
	for rows.Next() {
		var r receipt
		if err := rows.Scan(&r.rowid, &r.op, &r.result); err != nil {
			rows.Close()
			return err
		}
		receipts = append(receipts, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range receipts {
		var m map[string]any
		if err := json.Unmarshal([]byte(r.result), &m); err != nil {
			return fmt.Errorf("task_requests row %d: %w", r.rowid, err)
		}
		convertRefs(m)
		raw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		var t Task
		if err := json.Unmarshal(raw, &t); err != nil {
			return fmt.Errorf("task_requests row %d: %w", r.rowid, err)
		}
		var input any = t.TaskContent
		if r.op == "update" {
			input = struct {
				Content  TaskContent `json:"content"`
				Expected int64       `json:"expected"`
			}{t.TaskContent, t.Revision - 1}
		}
		digest, err := receiptDigest(input)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE task_requests SET result=?, digest=? WHERE rowid=?`, string(raw), digest, r.rowid); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE meta SET value='13' WHERE key='schema_version'`); err != nil {
		return err
	}
	return tx.Commit()
}

// rewriteRefsIn applies convert to every JSON body a query yields and
// writes the result back with the update statement (body, key).
func rewriteRefsIn(tx *sql.Tx, query, update string, convert func(map[string]any)) error {
	rows, err := tx.Query(query)
	if err != nil {
		return err
	}
	type row struct {
		key  any
		body string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.key, &r.body); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range all {
		var m map[string]any
		if err := json.Unmarshal([]byte(r.body), &m); err != nil {
			return fmt.Errorf("row %v: %w", r.key, err)
		}
		convert(m)
		raw, err := json.Marshal(m)
		if err != nil {
			return err
		}
		res, err := tx.Exec(update, string(raw), r.key)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("row %v: rewrite matched %d rows", r.key, n)
		}
	}
	return nil
}

// convertRefs turns string entries of the two reference lists into typed
// references in place; entries that are already objects are kept.
func convertRefs(m map[string]any) {
	for _, field := range []string{"candidate_refs", "evidence_refs"} {
		list, ok := m[field].([]any)
		if !ok {
			continue
		}
		out := make([]any, 0, len(list))
		for _, e := range list {
			if s, ok := e.(string); ok {
				r := LegacyRef(s)
				out = append(out, map[string]any{"kind": r.Kind, "ref": r.Ref})
			} else {
				out = append(out, e)
			}
		}
		m[field] = out
	}
}
