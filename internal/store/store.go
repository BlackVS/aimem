// Package store owns the per-project SQLite journal databases. Storage is
// project-oriented and physical: one database file per project under
// <state-root>/projects/<project-id>/journal.db. Isolation, retention,
// deletion, export, and backup are all per-project file operations.
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"aimem/internal/redact"
	"aimem/internal/schema"
	"aimem/internal/uuidv7"
)

// Registry opens and caches per-project stores under one state root.
type Registry struct {
	root string
	mu   sync.Mutex
	dbs  map[string]*DB
}

// DB is one project's journal database.
type DB struct {
	sql       *sql.DB
	projectID string
	path      string
}

// StoredEvent is an event row as returned by queries.
type StoredEvent struct {
	ID string `json:"id"`
	schema.Event
	Truncated bool   `json:"truncated,omitempty"`
	CreatedAt string `json:"created_at"`
}

// NewRegistry prepares the state root with restrictive permissions.
func NewRegistry(root string) (*Registry, error) {
	if err := os.MkdirAll(filepath.Join(root, "projects"), 0o700); err != nil {
		return nil, err
	}
	// Never operate through a symlinked state root.
	if fi, err := os.Lstat(root); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("state root %s is a symlink; refusing", root)
	}
	return &Registry{root: root, dbs: map[string]*DB{}}, nil
}

// Root returns the state root path.
func (r *Registry) Root() string { return r.root }

// Projects lists project IDs present under the state root.
func (r *Registry) Projects() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(r.root, "projects"))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && schema.ValidProjectID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// Open returns the store for one project, creating it if needed.
func (r *Registry) Open(projectID string) (*DB, error) {
	if !schema.ValidProjectID(projectID) {
		return nil, fmt.Errorf("invalid project id %q", projectID)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if db, ok := r.dbs[projectID]; ok {
		return db, nil
	}
	dir := filepath.Join(r.root, "projects", projectID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "journal.db")
	// _txlock=immediate: every database/sql transaction here writes
	// (PutDoc's CAS, migration steps), and a deferred BEGIN that reads
	// before writing can die with a non-retryable SQLITE_BUSY_SNAPSHOT
	// under WAL when another PROCESS (CLI beside the daemon) wrote in
	// between. Taking the write lock up front lets busy_timeout do its
	// job instead.
	dsn := "file:" + path + "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)"
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(1) // serialize writers; modernc + single file
	db := &DB{sql: sdb, projectID: projectID, path: path}
	if err := db.migrate(); err != nil {
		sdb.Close()
		return nil, err
	}
	// SQLite creates the file with umask defaults; tighten it. Aux files
	// (-wal/-shm) inherit the database file's permissions.
	if err := os.Chmod(path, 0o600); err != nil {
		sdb.Close()
		return nil, err
	}
	r.dbs[projectID] = db
	return db, nil
}

// OpenExisting is Open for read paths: it never creates the project,
// returning an error when no such project exists. Reads that auto-create
// resurrect deleted projects as empty husks (e.g. a stale MCP facade
// polling a dropped id).
func (r *Registry) OpenExisting(projectID string) (*DB, error) {
	if !schema.ValidProjectID(projectID) {
		return nil, fmt.Errorf("invalid project id %q", projectID)
	}
	r.mu.Lock()
	cached := r.dbs[projectID]
	r.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	if _, err := os.Stat(filepath.Join(r.root, "projects", projectID)); err != nil {
		return nil, fmt.Errorf("no such project %q", projectID)
	}
	return r.Open(projectID)
}

// Drop closes a project's database and deletes it from disk — journal,
// memories, embeddings, everything. Meant for stale duplicates (e.g. a
// path-derived id after pinning); the caller is responsible for having
// migrated anything worth keeping.
func (r *Registry) Drop(projectID string) error {
	if !schema.ValidProjectID(projectID) {
		return fmt.Errorf("invalid project id %q", projectID)
	}
	if projectID == UserScopeProject {
		return fmt.Errorf("refusing to drop the user memory DB")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if db, ok := r.dbs[projectID]; ok {
		db.sql.Close()
		delete(r.dbs, projectID)
	}
	dir := filepath.Join(r.root, "projects", projectID)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("no such project %q", projectID)
	}
	return os.RemoveAll(dir)
}

// Rename moves a project to a new id: the journal, its memories, its
// embeddings, everything, under one directory rename. Group databases
// cite their contributors as "project:<id>" source rows, so those are
// rewritten too — otherwise the KB's origin facet keeps naming a project
// that no longer exists.
//
// This changes the key, not a label. A client that still DERIVES the old
// id (see ident.ProjectID) will re-create it on its next checkpoint; the
// durable fix on that machine is a {"project": "<new id>"} pin in
// .aimem.json. Callers should say so.
func (r *Registry) Rename(oldID, newID string) error {
	for _, id := range []string{oldID, newID} {
		if !schema.ValidProjectID(id) {
			return fmt.Errorf("invalid project id %q", id)
		}
		// The user DB and the group DBs are addressed by convention, not
		// by derivation: renaming one orphans every reference to it.
		if id == UserScopeProject || strings.HasPrefix(id, "group-") {
			return fmt.Errorf("refusing to rename reserved project %q", id)
		}
	}
	if oldID == newID {
		return nil
	}
	if err := r.moveProject(oldID, newID); err != nil {
		return err
	}
	// Citations are rewritten outside the registry lock, because reaching
	// a group DB may mean opening it. The move already succeeded; a
	// failure here leaves stale origin labels, not lost data.
	r.renameSources(oldID, newID)
	return nil
}

// moveProject is the locked half of Rename: validate, evict cached
// handles, move the directory.
func (r *Registry) moveProject(oldID, newID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	dir := filepath.Join(r.root, "projects", oldID)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("no such project %q", oldID)
	}
	dst := filepath.Join(r.root, "projects", newID)
	if _, err := os.Stat(dst); err == nil {
		// Merging two histories is a different operation with different
		// failure modes (duplicate facts, interleaved journals). Refuse
		// rather than silently do half of it.
		return fmt.Errorf("project %q already exists; rename would merge two histories", newID)
	}
	// Close both handles first: an open SQLite file must not be moved out
	// from under its connection, and a cached handle keeps the old path.
	for _, id := range []string{oldID, newID} {
		if db, ok := r.dbs[id]; ok {
			db.sql.Close()
			delete(r.dbs, id)
		}
	}
	return os.Rename(dir, dst)
}

// renameSources rewrites "project:<old>" citations in every group DB.
// OR IGNORE covers the case where a fact already cites the new id (a
// re-rename); the leftover old rows are then deleted outright.
func (r *Registry) renameSources(oldID, newID string) {
	entries, err := os.ReadDir(filepath.Join(r.root, "projects"))
	if err != nil {
		return
	}
	oldSrc, newSrc := "project:"+oldID, "project:"+newID
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "group-") {
			continue
		}
		db, err := r.Open(e.Name())
		if err != nil {
			continue
		}
		db.sql.Exec(`UPDATE OR IGNORE memory_sources SET event_id = ? WHERE event_id = ?`, newSrc, oldSrc)
		db.sql.Exec(`DELETE FROM memory_sources WHERE event_id = ?`, oldSrc)
	}
}

// Close closes all open project databases.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, db := range r.dbs {
		db.sql.Close()
	}
	r.dbs = map[string]*DB{}
}

const currentSchema = 8

// SetMeta / GetMeta store small key-value project metadata (e.g. the
// project's declared knowledge groups, stamped from event pushes so the
// hub curator can see membership). GetMeta returns "" for a missing key.
func (d *DB) SetMeta(key, value string) error {
	_, err := d.sql.Exec(`INSERT INTO meta(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (d *DB) GetMeta(key string) (string, error) {
	var v string
	err := d.sql.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// ProjectStats is a cheap read-only snapshot for dashboards.
type ProjectStats struct {
	Events      int    `json:"events"`
	Sessions    int    `json:"sessions"`
	LastEventTS string `json:"last_event_ts"`
	LastClient  string `json:"last_client"`
	Memories    int    `json:"memories"` // live only
	Pinned      int    `json:"pinned"`
	Embedded    int    `json:"embedded"` // live memories with any embedding
}

func (d *DB) Stats() (ProjectStats, error) {
	var s ProjectStats
	if err := d.sql.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT session_id),
			COALESCE(MAX(ts),'') FROM events`).Scan(&s.Events, &s.Sessions, &s.LastEventTS); err != nil {
		return s, err
	}
	d.sql.QueryRow(`SELECT client FROM events ORDER BY id DESC LIMIT 1`).Scan(&s.LastClient)
	if err := d.sql.QueryRow(`SELECT COUNT(*), COALESCE(SUM(pinned),0) FROM memories
			WHERE expired_at IS NULL`).Scan(&s.Memories, &s.Pinned); err != nil {
		return s, err
	}
	if err := d.sql.QueryRow(`SELECT COUNT(DISTINCT e.memory_id) FROM memory_embeddings e
			JOIN memories m ON m.id = e.memory_id WHERE m.expired_at IS NULL`).Scan(&s.Embedded); err != nil {
		return s, err
	}
	return s, nil
}

// step applies one migration version atomically: the DDL and the
// schema_version bump inside the SQL commit together, so a crash
// mid-step leaves either the old schema or the new — never a partial
// schema under the old version number, which a re-run would trip over.
func (d *DB) step(stmts string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(stmts); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) migrate() error {
	if _, err := d.sql.Exec(`CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return err
	}
	var v int
	err := d.sql.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		v = 0
	} else if err != nil {
		return err
	}
	if v > currentSchema {
		return fmt.Errorf("database schema v%d is newer than this binary (v%d)", v, currentSchema)
	}
	if v < 1 {
		if err := d.step(`
CREATE TABLE events(
  id TEXT PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  schema_version INTEGER NOT NULL,
  client TEXT NOT NULL,
  session_id TEXT NOT NULL,
  turn_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  outcome TEXT NOT NULL,
  ts TEXT NOT NULL,
  user_request TEXT NOT NULL DEFAULT '',
  assistant_response TEXT NOT NULL DEFAULT '',
  tool_summary TEXT NOT NULL DEFAULT '[]',
  touched_paths TEXT NOT NULL DEFAULT '[]',
  git_branch TEXT NOT NULL DEFAULT '',
  git_status TEXT NOT NULL DEFAULT '',
  handoff_path TEXT NOT NULL DEFAULT '',
  handoff_hash TEXT NOT NULL DEFAULT '',
  parent_event_id TEXT,
  truncated INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL
);
CREATE INDEX idx_events_session ON events(session_id, id);
CREATE VIRTUAL TABLE events_fts USING fts5(
  user_request, assistant_response, content='events', content_rowid='rowid'
);
CREATE TRIGGER events_ai AFTER INSERT ON events BEGIN
  INSERT INTO events_fts(rowid, user_request, assistant_response)
  VALUES (new.rowid, new.user_request, new.assistant_response);
END;
CREATE TRIGGER events_ad AFTER DELETE ON events BEGIN
  INSERT INTO events_fts(events_fts, rowid, user_request, assistant_response)
  VALUES ('delete', old.rowid, old.user_request, old.assistant_response);
END;
INSERT INTO meta(key,value) VALUES('schema_version','1')
  ON CONFLICT(key) DO UPDATE SET value='1';
`); err != nil {
			return err
		}
	}
	if v < 2 {
		if err := d.step(`
CREATE TABLE memories(
  id TEXT PRIMARY KEY,
  text TEXT NOT NULL,
  created_at TEXT NOT NULL,
  expired_at TEXT,
  valid_at TEXT,
  invalid_at TEXT,
  superseded_by TEXT,
  pinned INTEGER NOT NULL DEFAULT 0,
  actor TEXT NOT NULL DEFAULT ''
);
CREATE VIRTUAL TABLE memories_fts USING fts5(
  text, content='memories', content_rowid='rowid'
);
CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
  INSERT INTO memories_fts(rowid, text) VALUES (new.rowid, new.text);
END;
CREATE TRIGGER memories_ad AFTER DELETE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, text)
  VALUES ('delete', old.rowid, old.text);
END;
CREATE TABLE memory_sources(
  memory_id TEXT NOT NULL,
  event_id TEXT NOT NULL,
  quote TEXT NOT NULL DEFAULT '',
  PRIMARY KEY(memory_id, event_id)
);
CREATE TABLE memory_audit(
  id TEXT PRIMARY KEY,
  memory_id TEXT NOT NULL,
  op TEXT NOT NULL,
  old_text TEXT,
  new_text TEXT,
  actor TEXT,
  ts TEXT NOT NULL
);
UPDATE meta SET value='2' WHERE key='schema_version';
`); err != nil {
			return err
		}
	}
	if v < 3 {
		// Knowledge-db structure (reference-app parity where it earns it):
		// typed facts, entity tags, inter-fact links, confidence with a
		// reinforcement rule. No graph server, no embeddings — plain tables.
		if err := d.step(`
ALTER TABLE memories ADD COLUMN kind TEXT NOT NULL DEFAULT 'fact';
ALTER TABLE memories ADD COLUMN confidence REAL NOT NULL DEFAULT 0.6;
CREATE TABLE memory_tags(
  memory_id TEXT NOT NULL,
  tag TEXT NOT NULL,
  PRIMARY KEY(memory_id, tag)
);
CREATE INDEX idx_memory_tags_tag ON memory_tags(tag);
CREATE TABLE memory_links(
  from_id TEXT NOT NULL,
  to_id TEXT NOT NULL,
  rel TEXT NOT NULL DEFAULT 'related',
  PRIMARY KEY(from_id, to_id, rel)
);
UPDATE meta SET value='3' WHERE key='schema_version';
`); err != nil {
			return err
		}
	}
	if v < 4 {
		// Semantic recall: one embedding per (memory, model), float32 blob.
		// Machine-local derived data — rebuildable, so not part of sync.
		if err := d.step(`
CREATE TABLE memory_embeddings(
  memory_id TEXT NOT NULL,
  model TEXT NOT NULL,
  dim INTEGER NOT NULL,
  vec BLOB NOT NULL,
  PRIMARY KEY(memory_id, model)
);
UPDATE meta SET value='4' WHERE key='schema_version';
`); err != nil {
			return err
		}
	}
	if v < 5 {
		// Curation run history: the token/cost meter over time. Synced
		// between machines (id-idempotent) so dashboards see hub runs.
		if err := d.step(`
CREATE TABLE curate_runs(
  id TEXT PRIMARY KEY,
  ts TEXT NOT NULL,
  host TEXT NOT NULL DEFAULT '',
  events_read INTEGER NOT NULL DEFAULT 0,
  written INTEGER NOT NULL DEFAULT 0,
  reasserted INTEGER NOT NULL DEFAULT 0,
  skipped INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd REAL NOT NULL DEFAULT 0
);
CREATE INDEX curate_runs_ts ON curate_runs(ts);
UPDATE meta SET value='5' WHERE key='schema_version';
`); err != nil {
			return err
		}
	}
	if v < 6 {
		// Which model produced the tokens — per-model cost breakdown.
		if err := d.step(`
ALTER TABLE curate_runs ADD COLUMN model TEXT NOT NULL DEFAULT '';
UPDATE meta SET value='6' WHERE key='schema_version';
`); err != nil {
			return err
		}
	}
	if v < 7 {
		// The consumed journal window per run: a zero-yield run (events
		// consumed, nothing written — possibly a guardrail's clean [])
		// stays auditable and its exact window re-curable.
		if err := d.step(`
ALTER TABLE curate_runs ADD COLUMN first_event TEXT NOT NULL DEFAULT '';
ALTER TABLE curate_runs ADD COLUMN last_event TEXT NOT NULL DEFAULT '';
UPDATE meta SET value='7' WHERE key='schema_version';
`); err != nil {
			return err
		}
	}
	// v8: shared documents (docs/DESIGN-shared-docs.md) — CAS-versioned
	// whole files, deliberately distinct from memories: a document is not
	// a fact, and supersession/dedup/ranking are the wrong operations on
	// a runbook.
	if v < 8 {
		if err := d.step(`
CREATE TABLE docs(
  name TEXT PRIMARY KEY,
  body TEXT NOT NULL,
  rev INTEGER NOT NULL,
  updated_at TEXT NOT NULL,
  updated_by TEXT NOT NULL DEFAULT '',
  deleted INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE doc_revisions(
  name TEXT NOT NULL,
  rev INTEGER NOT NULL,
  body TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  updated_by TEXT NOT NULL DEFAULT '',
  deleted INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(name, rev)
);
UPDATE meta SET value='8' WHERE key='schema_version';`); err != nil {
			return err
		}
	}
	return nil
}

// CurateRun is one recorded curation run (the maintenance cost meter).
type CurateRun struct {
	ID           string  `json:"id"`
	TS           string  `json:"ts"`
	Host         string  `json:"host,omitempty"`
	Model        string  `json:"model,omitempty"`
	EventsRead   int     `json:"events_read"`
	Written      int     `json:"written"`
	Reasserted   int     `json:"reasserted"`
	Skipped      int     `json:"skipped"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	FirstEvent   string  `json:"first_event,omitempty"` // consumed window,
	LastEvent    string  `json:"last_event,omitempty"`  // for re-curation audits
}

// AddCurateRun records a run; idempotent on ID (sync-safe).
func (d *DB) AddCurateRun(r *CurateRun) error {
	if r.ID == "" {
		r.ID = uuidv7.New()
	}
	_, err := d.sql.Exec(`INSERT OR IGNORE INTO curate_runs
		(id, ts, host, model, events_read, written, reasserted, skipped, input_tokens, output_tokens, cost_usd, first_event, last_event)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.TS, r.Host, r.Model, r.EventsRead, r.Written, r.Reasserted, r.Skipped,
		r.InputTokens, r.OutputTokens, r.CostUSD, r.FirstEvent, r.LastEvent)
	return err
}

// LastCurateRun returns the newest run, or nil.
func (d *DB) LastCurateRun() (*CurateRun, error) {
	r := &CurateRun{}
	err := d.sql.QueryRow(`SELECT id, ts, host, model, events_read, written, reasserted,
		skipped, input_tokens, output_tokens, cost_usd, first_event, last_event FROM curate_runs
		ORDER BY ts DESC LIMIT 1`).Scan(&r.ID, &r.TS, &r.Host, &r.Model, &r.EventsRead,
		&r.Written, &r.Reasserted, &r.Skipped, &r.InputTokens, &r.OutputTokens, &r.CostUSD,
		&r.FirstEvent, &r.LastEvent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

// CurateUsage sums token usage for runs at or after sinceTS ("" = all).
func (d *DB) CurateUsage(sinceTS string) (in, out int64, cost float64, runs int, err error) {
	err = d.sql.QueryRow(`SELECT COALESCE(SUM(input_tokens),0),
		COALESCE(SUM(output_tokens),0), COALESCE(SUM(cost_usd),0), COUNT(*)
		FROM curate_runs WHERE ts >= ?`, sinceTS).Scan(&in, &out, &cost, &runs)
	return
}

// CurateRuns lists runs newest-first for export/sync.
func (d *DB) CurateRuns() ([]CurateRun, error) {
	rows, err := d.sql.Query(`SELECT id, ts, host, model, events_read, written, reasserted,
		skipped, input_tokens, output_tokens, cost_usd, first_event, last_event
		FROM curate_runs ORDER BY ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CurateRun
	for rows.Next() {
		var r CurateRun
		if err := rows.Scan(&r.ID, &r.TS, &r.Host, &r.Model, &r.EventsRead, &r.Written,
			&r.Reasserted, &r.Skipped, &r.InputTokens, &r.OutputTokens, &r.CostUSD,
			&r.FirstEvent, &r.LastEvent); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Append sanitizes and inserts one event. The insert is idempotent on
// idempotency_key: a duplicate resolves to the existing row and inserted is
// false. This is the entire synchronous ingestion path — no enrichment here.
func (d *DB) Append(ev *schema.Event) (id string, inserted bool, err error) {
	if err := ev.Validate(); err != nil {
		return "", false, err
	}
	var anyTrunc bool
	san := func(s string) string {
		out, t := redact.String(s, redact.DefaultMaxFieldBytes)
		anyTrunc = anyTrunc || t
		return out
	}
	userReq := san(ev.UserRequest)
	reply := san(ev.AssistantReply)
	tools, t1 := redact.Strings(ev.ToolSummary, 4096)
	paths, t2 := redact.Strings(ev.TouchedPaths, 1024)
	anyTrunc = anyTrunc || t1 || t2
	gitStatus := san(ev.GitStatus)
	toolsJSON, _ := json.Marshal(tools)
	pathsJSON, _ := json.Marshal(paths)

	id = uuidv7.New()
	res, err := d.sql.Exec(`
INSERT INTO events(id, idempotency_key, schema_version, client, session_id,
  turn_id, kind, outcome, ts, user_request, assistant_response, tool_summary,
  touched_paths, git_branch, git_status, handoff_path, handoff_hash,
  parent_event_id, truncated, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(idempotency_key) DO NOTHING`,
		id, ev.IdempotencyKey, ev.SchemaVersion, ev.Client, ev.SessionID,
		ev.TurnID, ev.Kind, ev.Outcome, ev.TS, userReq, reply, string(toolsJSON),
		string(pathsJSON), ev.GitBranch, gitStatus, ev.HandoffPath,
		ev.HandoffHash, nullable(ev.ParentEventID), boolInt(anyTrunc),
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return "", false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Idempotent retry: return the existing row's id.
		var existing string
		if err := d.sql.QueryRow(`SELECT id FROM events WHERE idempotency_key=?`,
			ev.IdempotencyKey).Scan(&existing); err != nil {
			return "", false, err
		}
		return existing, false, nil
	}
	return id, true, nil
}

// Sessions lists distinct session IDs with event counts and last activity.
func (d *DB) Sessions() ([]map[string]any, error) {
	rows, err := d.sql.Query(`
SELECT session_id, client, COUNT(*), MAX(ts) FROM events
GROUP BY session_id, client ORDER BY MAX(id) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var sid, client, last string
		var n int
		if err := rows.Scan(&sid, &client, &n, &last); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"session_id": sid, "client": client, "events": n, "last_ts": last,
		})
	}
	return out, rows.Err()
}

// Timeline returns a session's events in insertion order, newest last.
// RecentEvents returns the newest events across all sessions, newest
// first (dashboard tail).
func (d *DB) RecentEvents(limit int) ([]StoredEvent, error) {
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	rows, err := d.sql.Query(`SELECT `+cols+` FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	return scan(rows)
}

func (d *DB) Timeline(sessionID string, limit int) ([]StoredEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := d.sql.Query(`SELECT `+cols+` FROM events
WHERE session_id=? ORDER BY id DESC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	evs, err := scan(rows)
	if err != nil {
		return nil, err
	}
	// Reverse to chronological order.
	for i, j := 0, len(evs)-1; i < j; i, j = i+1, j-1 {
		evs[i], evs[j] = evs[j], evs[i]
	}
	return evs, nil
}

// Latest returns a session's most recent event, or nil.
func (d *DB) Latest(sessionID string) (*StoredEvent, error) {
	rows, err := d.sql.Query(`SELECT `+cols+` FROM events
WHERE session_id=? ORDER BY id DESC LIMIT 1`, sessionID)
	if err != nil {
		return nil, err
	}
	evs, err := scan(rows)
	if err != nil || len(evs) == 0 {
		return nil, err
	}
	return &evs[0], nil
}

// ftsQuote turns a raw user query into a literal FTS5 phrase-per-token
// query: every token is double-quoted so hyphens, dots, and FTS operators
// in ordinary text cannot become syntax errors. Tokens AND together.
func ftsQuote(q string) string {
	fields := strings.Fields(q)
	for i, f := range fields {
		fields[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	return strings.Join(fields, " ")
}

// Search runs deterministic FTS5 full-text search within this project.
func (d *DB) Search(query string, limit int) ([]StoredEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := d.sql.Query(`SELECT `+cols+` FROM events
WHERE rowid IN (SELECT rowid FROM events_fts WHERE events_fts MATCH ? ORDER BY rank LIMIT ?)
ORDER BY id DESC`, ftsQuote(query), limit)
	if err != nil {
		return nil, err
	}
	return scan(rows)
}

// Retention deletes events older than maxAge (if >0) and then, if maxBytes>0,
// deletes oldest events until the database fits the byte budget. Scoped
// strictly to this project's database file; never follows links.
func (d *DB) Retention(maxAge time.Duration, maxBytes int64) (deleted int64, err error) {
	if maxAge > 0 {
		cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339)
		res, err := d.sql.Exec(`DELETE FROM events WHERE ts < ?`, cutoff)
		if err != nil {
			return deleted, err
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	if maxBytes > 0 {
		for {
			size, err := d.dbSize()
			if err != nil || size <= maxBytes {
				return deleted, err
			}
			res, err := d.sql.Exec(`DELETE FROM events WHERE id IN
(SELECT id FROM events ORDER BY id ASC LIMIT 100)`)
			if err != nil {
				return deleted, err
			}
			n, _ := res.RowsAffected()
			deleted += n
			if n == 0 {
				return deleted, nil
			}
			if _, err := d.sql.Exec(`VACUUM`); err != nil {
				return deleted, err
			}
		}
	}
	return deleted, nil
}

// MaxEventID returns the lexicographically largest event id (UUIDv7 order
// == time order), or "" for an empty journal. Used as the sync cursor.
func (d *DB) MaxEventID() (string, error) {
	var id string
	err := d.sql.QueryRow(`SELECT COALESCE(MAX(id),'') FROM events`).Scan(&id)
	return id, err
}

// DumpSince writes events with id > since (all events when since is empty).
func (d *DB) DumpSince(w io.Writer, since string) error {
	rows, err := d.sql.Query(`SELECT `+cols+` FROM events WHERE id > ? ORDER BY id ASC`, since)
	if err != nil {
		return err
	}
	evs, err := scan(rows)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	for _, e := range evs {
		if err := enc.Encode(map[string]any{
			"project_id": d.projectID,
			"event":      e.Event,
		}); err != nil {
			return err
		}
	}
	return nil
}

// EventsSince returns up to limit events with id > since, oldest first.
func (d *DB) EventsSince(since string, limit int) ([]StoredEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.sql.Query(`SELECT `+cols+` FROM events WHERE id > ? ORDER BY id ASC LIMIT ?`,
		since, limit)
	if err != nil {
		return nil, err
	}
	return scan(rows)
}

// Dump writes every event as one JSON line in the adapter payload shape
// ({"project_id","event",...}), suitable for `aimem import-events` on
// another machine. Because idempotency keys are globally stable and inserts
// are ON CONFLICT DO NOTHING, importing a dump is a pure union — duplicates
// drop out, and cross-machine sync is conflict-free by construction.
func (d *DB) Dump(w io.Writer) error { return d.DumpSince(w, "") }

func (d *DB) dbSize() (int64, error) {
	var pageCount, pageSize int64
	if err := d.sql.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, err
	}
	if err := d.sql.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, err
	}
	return pageCount * pageSize, nil
}

const cols = `id, idempotency_key, schema_version, client, session_id, turn_id,
kind, outcome, ts, user_request, assistant_response, tool_summary,
touched_paths, git_branch, git_status, handoff_path, handoff_hash,
COALESCE(parent_event_id,''), truncated, created_at`

func scan(rows *sql.Rows) ([]StoredEvent, error) {
	defer rows.Close()
	var out []StoredEvent
	for rows.Next() {
		var e StoredEvent
		var tools, paths string
		var trunc int
		if err := rows.Scan(&e.ID, &e.IdempotencyKey, &e.SchemaVersion,
			&e.Client, &e.SessionID, &e.TurnID, &e.Kind, &e.Outcome, &e.TS,
			&e.UserRequest, &e.AssistantReply, &tools, &paths, &e.GitBranch,
			&e.GitStatus, &e.HandoffPath, &e.HandoffHash, &e.ParentEventID,
			&trunc, &e.CreatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(tools), &e.ToolSummary)
		json.Unmarshal([]byte(paths), &e.TouchedPaths)
		e.Truncated = trunc != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
