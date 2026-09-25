package store

// Task storage (docs/AIMEM-KANBAN-PROPOSAL.md, docs/DESIGN-task-backend-
// implementation.md): dedicated per-project tables, independent of journal
// retention, collection revision pruning and sync. Every method here takes
// a TRUSTED actor: the service authenticates and authorizes each request —
// including retries — before entering this layer, and this layer never
// authenticates a Go struct.
//
// Concurrency: a project DB serializes writers already (one connection,
// immediate transactions), so a task mutation owns the database for its
// transaction; no extra lock is needed. Project lifecycle (drop/merge)
// coordinates with task writers at the registry lock — see store.go.

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"aimem/internal/redact"
	"aimem/internal/uuidv7"
)

const (
	MaxTaskBytes        = 32 * 1024 // canonical JSON of the editable content
	MaxTaskCommentBytes = 32 * 1024 // decoded Markdown body
	MaxTaskTitleBytes   = 256
	MaxTaskRefBytes     = 2048
	MaxTaskListEntries  = 32
	MaxTaskKeyBytes     = 128
	maxTaskActorName    = 128
)

var (
	ErrTaskInvalid       = errors.New("invalid task input")
	ErrTaskNotFound      = errors.New("task or comment not found")
	ErrTaskArchived      = errors.New("task is archived; unarchive it before adding discussion")
	ErrTaskRetryConflict = errors.New("idempotency key already used with different input")
	ErrTaskReservedScope = errors.New("tasks require an ordinary project (not the user store or a knowledge group)")
	ErrProjectHasTasks   = errors.New("project holds tasks (including archived ones); drop and source-merge are refused until a task-preserving export/removal exists")

	taskIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// TaskStates are the seven approved states; no forced transition graph.
var TaskStates = []string{"BACKLOG", "READY", "IN_PROGRESS", "REVIEW", "BLOCKED", "DONE", "CANCELLED"}

// TaskActor is the authenticated principal a mutation is stamped with. The
// service builds it from validated credentials; Kind "user" carries stable
// user and token IDs (display names change, keys must not), Kind "admin"
// carries only the trusted admin identity's name.
type TaskActor struct {
	Kind    string `json:"kind"`
	UserID  string `json:"user_id,omitempty"`
	TokenID string `json:"token_id,omitempty"`
	Name    string `json:"name"`
}

// key is the retry-receipt principal: distinct tokens are distinct retry
// scopes on purpose (a replacement token does not promise deduplication).
func (a TaskActor) key() (string, error) {
	if err := taskText(a.Name, maxTaskActorName, true); err != nil {
		return "", fmt.Errorf("invalid actor name: %w", err)
	}
	switch a.Kind {
	case "user":
		if taskIDRE.MatchString(a.UserID) && taskIDRE.MatchString(a.TokenID) {
			return "user/" + a.UserID + "/" + a.TokenID, nil
		}
	case "admin":
		if a.UserID == "" && a.TokenID == "" {
			return "admin/" + a.Name, nil
		}
	}
	return "", errors.New("invalid authenticated task actor")
}

// TaskAssignee names a user or access group. Assignment conveys no
// authority; storage validates the shape, the service the existence.
type TaskAssignee struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

func (a *TaskAssignee) column() string {
	if a == nil {
		return ""
	}
	return a.Kind + "/" + a.ID
}

// TaskContent is everything a writer may set. Updates REPLACE the whole
// content: required fields stay required, omitted optional ones clear.
type TaskContent struct {
	Title              string        `json:"title"`
	Objective          string        `json:"objective"`
	AcceptanceCriteria string        `json:"acceptance_criteria"`
	NonGoals           string        `json:"non_goals"`
	State              string        `json:"state"`
	Assignee           *TaskAssignee `json:"assignee,omitempty"`
	Blocker            string        `json:"blocker"`
	Dependencies       []string      `json:"dependencies"`
	CandidateRefs      []TaskRef     `json:"candidate_refs"`
	EvidenceRefs       []TaskRef     `json:"evidence_refs"`
	NextAction         string        `json:"next_action"`
	Archived           bool          `json:"archived"`
	// Epic is the optional grouping above the task: an OPEN epic of the
	// same project (checked inside the write's transaction). Omitted on
	// the wire when empty, so older readers and the retry digest see the
	// content they always saw.
	Epic string `json:"epic,omitempty"`
}

// Task is the current row. The owning project is NOT stored in the
// snapshot: it is the partition the row lives in, resolved at read time,
// so a project rename never rewrites task data.
type Task struct {
	ID           string            `json:"id"`
	Revision     int64             `json:"revision"`
	Coordination *TaskCoordination `json:"coordination,omitempty"`
	TaskContent
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// TaskChange is one history row: the complete resulting snapshot and who
// produced it.
type TaskChange struct {
	Task  Task      `json:"task"`
	Actor TaskActor `json:"actor"`
}

// TaskComment is immutable discussion. Sequence is the database append
// order (the pagination cursor); ID is the permanent reference.
type TaskComment struct {
	ID        string    `json:"id"`
	TaskID    string    `json:"task_id"`
	Sequence  int64     `json:"sequence"`
	Body      string    `json:"body"`
	Actor     TaskActor `json:"actor"`
	CreatedAt string    `json:"created_at"`
}

// TaskConflict is the typed CAS failure; Current lets the writer re-read
// and reapply intent. Unlike documents, an identical body with a stale
// revision is still a conflict here — retries are handled by receipts.
type TaskConflict struct{ Current Task }

func (e *TaskConflict) Error() string {
	return fmt.Sprintf("task revision conflict: current revision is %d", e.Current.Revision)
}

func validTaskState(s string) bool { return slices.Contains(TaskStates, s) }

// taskText validates authored text: UTF-8, bounded, no control characters
// beyond newline/tab, and no high-confidence secret shapes (the same tier
// that refuses a document — never silently redact authored content).
func taskText(s string, max int, required bool) error {
	if !utf8.ValidString(s) {
		return errors.New("text must be valid UTF-8")
	}
	if len(s) > max {
		return fmt.Errorf("text is %d bytes; the limit is %d", len(s), max)
	}
	if required && strings.TrimSpace(s) == "" {
		return errors.New("text must not be blank")
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return errors.New("text contains an unsupported control character")
		}
		if (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return errors.New("text contains a bidirectional override character")
		}
	}
	if _, refuse := redact.ScanAuthored(s); len(refuse) > 0 {
		return fmt.Errorf("secret-shaped content refused (%s); remove the secret first", strings.Join(refuse, ", "))
	}
	return nil
}

func (c *TaskContent) validate() error {
	if c.Dependencies == nil {
		c.Dependencies = []string{}
	}
	if c.CandidateRefs == nil {
		c.CandidateRefs = []TaskRef{}
	}
	if c.EvidenceRefs == nil {
		c.EvidenceRefs = []TaskRef{}
	}
	if err := taskText(c.Title, MaxTaskTitleBytes, true); err != nil {
		return fmt.Errorf("title: %w", err)
	}
	if !validTaskState(c.State) {
		return fmt.Errorf("invalid task state (want one of %s)", strings.Join(TaskStates, ", "))
	}
	if c.Archived && c.State != "DONE" && c.State != "CANCELLED" {
		return errors.New("only DONE or CANCELLED tasks can be archived")
	}
	if c.Assignee != nil && ((c.Assignee.Kind != "user" && c.Assignee.Kind != "group") || !taskIDRE.MatchString(c.Assignee.ID)) {
		return errors.New("assignee must be {kind: user|group, id: <access identity UUID>}")
	}
	if c.Epic != "" && !taskIDRE.MatchString(c.Epic) {
		return errors.New("epic must be an epic id")
	}
	if len(c.Dependencies) > MaxTaskListEntries {
		return fmt.Errorf("at most %d entries per dependency or reference list", MaxTaskListEntries)
	}
	if err := validateRefs("candidate_refs", c.CandidateRefs); err != nil {
		return err
	}
	if err := validateRefs("evidence_refs", c.EvidenceRefs); err != nil {
		return err
	}
	for _, id := range c.Dependencies {
		if !taskIDRE.MatchString(id) {
			return errors.New("each dependency must be a task ID")
		}
	}
	for _, f := range []struct{ name, text string }{
		{"objective", c.Objective}, {"acceptance_criteria", c.AcceptanceCriteria}, {"non_goals", c.NonGoals},
		{"blocker", c.Blocker}, {"next_action", c.NextAction},
	} {
		if err := taskText(f.text, MaxTaskBytes, false); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if len(b) > MaxTaskBytes {
		return fmt.Errorf("task content is %d bytes of JSON; the limit is %d — link long reports instead", len(b), MaxTaskBytes)
	}
	return nil
}

// IsReservedProject reports a store that never holds tasks: the user
// memory store and the knowledge-group stores.
func IsReservedProject(id string) bool {
	return id == UserScopeProject || strings.HasPrefix(id, "group-")
}

func (d *DB) taskScopeOK() error {
	if IsReservedProject(d.projectID) {
		return ErrTaskReservedScope
	}
	return nil
}

// receiptDigestFormat names the canonicalization a stored digest was made
// with; it is the digest's prefix. A receipt whose format this binary does
// not know is a conflict, never a silent match.
const receiptDigestFormat = "1"

// receiptDigest is the retry receipt's fingerprint of a mutation's input:
// the input's JSON with every zero-valued member removed, keys sorted,
// hashed. Dropping zero values is what makes a retry survive an upgrade —
// a field added to TaskContent later arrives empty from an older client
// and from a replay, so it must not change the digest — and it matches the
// replace-all contract, where an absent optional field and an empty one
// mean the same thing.
func receiptDigest(input any) (string, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	canon, err := json.Marshal(pruneZero(v))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return receiptDigestFormat + ":" + hex.EncodeToString(sum[:]), nil
}

// pruneZero removes zero-valued members ("" , false, 0, null, and empty
// arrays or objects after pruning) from every object in a decoded JSON
// value. Array elements are pruned in place but never removed: position
// carries meaning there.
func pruneZero(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, m := range x {
			m = pruneZero(m)
			if isZero(m) {
				delete(x, k)
			} else {
				x[k] = m
			}
		}
		return x
	case []any:
		for i := range x {
			x[i] = pruneZero(x[i])
		}
		return x
	}
	return v
}

func isZero(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case float64:
		return x == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// taskMutation runs fn inside one immediate transaction with a retry
// receipt keyed by (actor, operation, scope, key). A replay with the same
// canonical input returns the ORIGINAL result (a retried create yields the
// task as first created, even if it has advanced since); the same key with
// different input is a conflict. Receipt, task rows and history commit
// together, so a failure anywhere leaves no trace and does not consume the
// key.
func taskMutation[T any](d *DB, actor TaskActor, op, scope, key string, input any, fn func(*sql.Tx) (T, error)) (T, error) {
	return checkedTaskMutation(d, actor, op, scope, key, input, nil, fn)
}

// checkedTaskMutation checks current authorization inside the transaction even
// on receipt replay. Generation checks for NEW effects belong in fn: a retry of
// an accepted resume/leave must still return its original result.
func checkedTaskMutation[T any](d *DB, actor TaskActor, op, scope, key string, input any, check func(*sql.Tx) error, fn func(*sql.Tx) (T, error)) (T, error) {
	var zero T
	if err := d.taskScopeOK(); err != nil {
		return zero, err
	}
	actorKey, err := actor.key()
	if err != nil {
		return zero, err
	}
	if err := taskText(key, MaxTaskKeyBytes, true); err != nil {
		return zero, invalid(fmt.Errorf("idempotency key: %w", err))
	}
	digest, err := receiptDigest(input)
	if err != nil {
		return zero, err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	if check != nil {
		if err := check(tx); err != nil {
			return zero, err
		}
	}
	var savedDigest, saved string
	err = tx.QueryRow(`SELECT digest, result FROM task_requests WHERE actor=? AND operation=? AND scope=? AND key=?`,
		actorKey, op, scope, key).Scan(&savedDigest, &saved)
	switch {
	case err == nil:
		if savedDigest != digest {
			return zero, ErrTaskRetryConflict
		}
		var out T
		if err := json.Unmarshal([]byte(saved), &out); err != nil {
			return zero, err
		}
		return out, nil
	case !errors.Is(err, sql.ErrNoRows):
		return zero, err
	}
	out, err := fn(tx)
	if err != nil {
		return zero, err
	}
	result, err := json.Marshal(out)
	if err != nil {
		return zero, err
	}
	if _, err := tx.Exec(`INSERT INTO task_requests(actor, operation, scope, key, digest, result) VALUES(?,?,?,?,?,?)`,
		actorKey, op, scope, key, digest, string(result)); err != nil {
		return zero, err
	}
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return out, nil
}

// CreateTask stores a new task at revision 1 with its first history row.
// An empty state defaults to BACKLOG.
func (d *DB) CreateTask(content TaskContent, actor TaskActor, key string) (Task, error) {
	if content.State == "" {
		content.State = "BACKLOG"
	}
	if err := content.validate(); err != nil {
		return Task{}, invalid(err)
	}
	t, err := taskMutation(d, actor, "create", "", key, content, func(tx *sql.Tx) (Task, error) {
		if err := checkEpicAssignable(tx, content.Epic, ""); err != nil {
			return Task{}, err
		}
		now := nowUTC()
		t := Task{ID: uuidv7.New(), Revision: 1, TaskContent: content, CreatedAt: now, UpdatedAt: now}
		return t, saveTask(tx, t, actor, true)
	})
	if err == nil && d.hints != nil {
		d.hints.put(t.ID, d.projectID) // a lookup right after a create needs no scan
	}
	return t, err
}

// UpdateTask replaces every editable field under expected-revision CAS.
func (d *DB) UpdateTask(id string, content TaskContent, expected int64, actor TaskActor, key string) (Task, error) {
	if !taskIDRE.MatchString(id) {
		return Task{}, ErrTaskNotFound
	}
	if expected < 1 {
		return Task{}, invalid(errors.New("expected_revision must be a positive revision"))
	}
	if err := content.validate(); err != nil {
		return Task{}, invalid(err)
	}
	input := struct {
		Content  TaskContent `json:"content"`
		Expected int64       `json:"expected"`
	}{content, expected}
	check := func(tx *sql.Tx) error {
		var managed bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM team_managed_tasks WHERE task_id=? AND managed=1)`, id).Scan(&managed); err != nil {
			return err
		}
		if managed {
			return ErrManagedTask
		}
		return rejectActiveReservation(tx, id)
	}
	return checkedTaskMutation(d, actor, "update", id, key, input, check, func(tx *sql.Tx) (Task, error) {
		t, err := readTask(tx, id)
		if err != nil {
			return Task{}, err
		}
		if t.Revision != expected {
			return Task{}, &TaskConflict{Current: t}
		}
		if err := checkEpicAssignable(tx, content.Epic, t.Epic); err != nil {
			return Task{}, err
		}
		t.TaskContent = content
		t.Revision++
		t.UpdatedAt = nowUTC()
		return t, saveTask(tx, t, actor, false)
	})
}

// saveTask writes the current row (indexed filter columns + snapshot) and
// the matching history row in the caller's transaction; a test proves the
// columns and the snapshot agree.
func saveTask(tx *sql.Tx, t Task, actor TaskActor, create bool) error {
	if !create {
		if err := rejectActiveReservation(tx, t.ID); err != nil {
			return err
		}
	}
	return writeTask(tx, t, actor, create)
}

// saveReservedTask is the ledger's narrow write path after it has checked the
// current reservation ID, fence and task revision in this same transaction.
func saveReservedTask(tx *sql.Tx, t Task, actor TaskActor) error {
	return writeTask(tx, t, actor, false)
}

func writeTask(tx *sql.Tx, t Task, actor TaskActor, create bool) error {
	t.Coordination = nil // current projection is never persisted into task history
	body, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if create {
		_, err = tx.Exec(`INSERT INTO tasks(id, state, archived, assignee, epic, body) VALUES(?,?,?,?,?,?)`,
			t.ID, t.State, boolInt(t.Archived), t.Assignee.column(), t.Epic, string(body))
	} else {
		_, err = tx.Exec(`UPDATE tasks SET state=?, archived=?, assignee=?, epic=?, body=? WHERE id=?`,
			t.State, boolInt(t.Archived), t.Assignee.column(), t.Epic, string(body), t.ID)
	}
	if err != nil {
		return err
	}
	change, err := json.Marshal(TaskChange{Task: t, Actor: actor})
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO task_history(task_id, revision, body) VALUES(?,?,?)`, t.ID, t.Revision, string(change))
	return err
}

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

const taskReadQuery = `SELECT tasks.body,COALESCE(m.team_id,''),COALESCE(a.id,''),COALESCE(a.state,'') FROM tasks
LEFT JOIN team_managed_tasks m ON m.task_id=tasks.id AND m.managed=1
LEFT JOIN team_assignments a ON a.task_id=tasks.id AND a.reserved=1`

func scanCurrentTask(row interface{ Scan(...any) error }) (Task, error) {
	var body string
	var c TaskCoordination
	if err := row.Scan(&body, &c.TeamID, &c.AttemptID, &c.State); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, ErrTaskNotFound
		}
		return Task{}, err
	}
	var t Task
	if err := json.Unmarshal([]byte(body), &t); err != nil {
		return Task{}, err
	}
	if c.TeamID != "" {
		t.Coordination = &c
	}
	return t, nil
}

func readTask(q rowQuerier, id string) (Task, error) {
	return scanCurrentTask(q.QueryRow(taskReadQuery+` WHERE tasks.id=?`, id))
}

// GetTask returns the current task.
func (d *DB) GetTask(id string) (Task, error) {
	if !taskIDRE.MatchString(id) {
		return Task{}, ErrTaskNotFound
	}
	return readTask(d.sql, id)
}

// AddTaskComment appends immutable discussion. It does not touch the task
// revision; existence and archive state are checked in the same
// transaction as the insert, so an append cannot race an archival. A
// committed append replays through its receipt even after archival.
func (d *DB) AddTaskComment(taskID, body string, actor TaskActor, key string) (TaskComment, error) {
	if !taskIDRE.MatchString(taskID) {
		return TaskComment{}, ErrTaskNotFound
	}
	if err := taskText(body, MaxTaskCommentBytes, true); err != nil {
		return TaskComment{}, invalid(fmt.Errorf("comment: %w", err))
	}
	return taskMutation(d, actor, "comment", taskID, key, body, func(tx *sql.Tx) (TaskComment, error) {
		t, err := readTask(tx, taskID)
		if err != nil {
			return TaskComment{}, err
		}
		if t.Archived {
			return TaskComment{}, ErrTaskArchived
		}
		c := TaskComment{ID: uuidv7.New(), TaskID: taskID, Body: body, Actor: actor, CreatedAt: nowUTC()}
		a, err := json.Marshal(actor)
		if err != nil {
			return TaskComment{}, err
		}
		res, err := tx.Exec(`INSERT INTO task_comments(id, task_id, body, actor, created_at) VALUES(?,?,?,?,?)`,
			c.ID, taskID, body, string(a), c.CreatedAt)
		if err != nil {
			return TaskComment{}, err
		}
		c.Sequence, err = res.LastInsertId()
		return c, err
	})
}

type rowScanner interface{ Scan(dest ...any) error }

func scanTaskComment(row rowScanner) (TaskComment, error) {
	var c TaskComment
	var actor string
	err := row.Scan(&c.ID, &c.TaskID, &c.Sequence, &c.Body, &actor, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrTaskNotFound
	}
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal([]byte(actor), &c.Actor)
}

// GetTaskComment returns one comment; a wrong parent is "not found".
func (d *DB) GetTaskComment(taskID, id string) (TaskComment, error) {
	if !taskIDRE.MatchString(taskID) || !taskIDRE.MatchString(id) {
		return TaskComment{}, ErrTaskNotFound
	}
	return scanTaskComment(d.sql.QueryRow(
		`SELECT id, task_id, sequence, body, actor, created_at FROM task_comments WHERE task_id=? AND id=?`, taskID, id))
}

func taskPageLimit(limit int) (int, error) {
	if limit == 0 {
		return 20, nil
	}
	if limit < 1 || limit > 100 {
		return 0, invalid(errors.New("limit must be between 1 and 100"))
	}
	return limit, nil
}

// invalid marks a caller-input error so a transport can answer 400.
func invalid(err error) error { return fmt.Errorf("%w: %w", ErrTaskInvalid, err) }

// TaskFilter selects summaries; archived tasks are excluded unless asked
// for. After is the last task ID of the previous page.
type TaskFilter struct {
	State           string
	Assignee        *TaskAssignee
	Epic            string // only tasks under this epic
	IncludeArchived bool
	After           string
	Limit           int
}

// TaskSummary is the bounded list row: never the discussion or history.
type TaskSummary struct {
	Coordination *TaskCoordination `json:"coordination,omitempty"`
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	State        string            `json:"state"`
	Assignee     *TaskAssignee     `json:"assignee,omitempty"`
	Epic         string            `json:"epic,omitempty"`
	Archived     bool              `json:"archived"`
	Revision     int64             `json:"revision"`
	UpdatedAt    string            `json:"updated_at"`
}

type TaskPage struct {
	Tasks []TaskSummary `json:"tasks"`
	Next  string        `json:"next_cursor,omitempty"`
}

// ListTasks pages task summaries in ID (creation) order.
func (d *DB) ListTasks(f TaskFilter) (TaskPage, error) {
	out := TaskPage{Tasks: []TaskSummary{}}
	limit, err := taskPageLimit(f.Limit)
	if err != nil {
		return out, err
	}
	if f.State != "" && !validTaskState(f.State) {
		return out, invalid(errors.New("invalid task state filter"))
	}
	if f.After != "" && !taskIDRE.MatchString(f.After) {
		return out, invalid(errors.New("invalid task cursor"))
	}
	if f.Assignee != nil && ((f.Assignee.Kind != "user" && f.Assignee.Kind != "group") || !taskIDRE.MatchString(f.Assignee.ID)) {
		return out, invalid(errors.New("invalid assignee filter"))
	}
	if f.Epic != "" && !taskIDRE.MatchString(f.Epic) {
		return out, invalid(errors.New("invalid epic filter"))
	}
	query := taskReadQuery + ` WHERE tasks.id > ?`
	args := []any{f.After}
	if !f.IncludeArchived {
		query += ` AND archived = 0`
	}
	if f.State != "" {
		query += ` AND tasks.state = ?`
		args = append(args, f.State)
	}
	if f.Assignee != nil {
		query += ` AND assignee = ?`
		args = append(args, f.Assignee.column())
	}
	if f.Epic != "" {
		query += ` AND epic = ?`
		args = append(args, f.Epic)
	}
	query += ` ORDER BY tasks.id LIMIT ?`
	args = append(args, limit+1)
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(out.Tasks) == limit {
			out.Next = out.Tasks[limit-1].ID
			break
		}
		t, err := scanCurrentTask(rows)
		if err != nil {
			return out, err
		}
		out.Tasks = append(out.Tasks, TaskSummary{ID: t.ID, Title: t.Title, State: t.State, Assignee: t.Assignee, Epic: t.Epic,
			Archived: t.Archived, Revision: t.Revision, UpdatedAt: t.UpdatedAt, Coordination: t.Coordination})
	}
	return out, rows.Err()
}

type TaskHistoryPage struct {
	Changes []TaskChange `json:"changes"`
	Next    int64        `json:"next_cursor,omitempty"`
}

// TaskHistory pages accepted changes in revision order; after is the last
// revision of the previous page (0 from the start).
func (d *DB) TaskHistory(id string, after int64, limit int) (TaskHistoryPage, error) {
	out := TaskHistoryPage{Changes: []TaskChange{}}
	limit, err := taskPageLimit(limit)
	if err != nil {
		return out, err
	}
	if after < 0 {
		return out, invalid(errors.New("invalid history cursor"))
	}
	if _, err := d.GetTask(id); err != nil {
		return out, err
	}
	rows, err := d.sql.Query(`SELECT body FROM task_history WHERE task_id=? AND revision > ? ORDER BY revision LIMIT ?`, id, after, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return out, err
		}
		if len(out.Changes) == limit {
			out.Next = out.Changes[limit-1].Task.Revision
			break
		}
		var c TaskChange
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			return out, err
		}
		out.Changes = append(out.Changes, c)
	}
	return out, rows.Err()
}

type TaskCommentPage struct {
	Comments []TaskComment `json:"comments"`
	Next     int64         `json:"next_cursor,omitempty"`
}

// TaskComments pages discussion in append order; after is the last
// sequence of the previous page (0 from the start). The cursor is stable
// while later comments arrive.
func (d *DB) TaskComments(id string, after int64, limit int) (TaskCommentPage, error) {
	out := TaskCommentPage{Comments: []TaskComment{}}
	limit, err := taskPageLimit(limit)
	if err != nil {
		return out, err
	}
	if after < 0 {
		return out, invalid(errors.New("invalid comment cursor"))
	}
	if _, err := d.GetTask(id); err != nil {
		return out, err
	}
	rows, err := d.sql.Query(`SELECT id, task_id, sequence, body, actor, created_at FROM task_comments WHERE task_id=? AND sequence > ? ORDER BY sequence LIMIT ?`, id, after, limit+1)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanTaskComment(rows)
		if err != nil {
			return out, err
		}
		if len(out.Comments) == limit {
			out.Next = out.Comments[limit-1].Sequence
			break
		}
		out.Comments = append(out.Comments, c)
	}
	return out, rows.Err()
}

// HasTasks reports whether the project holds any task, archived included.
func (d *DB) HasTasks() (bool, error) {
	var exists bool
	err := d.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks)`).Scan(&exists)
	return exists, err
}

// fileHasTasks answers HasTasks for a project file whose cached handle the
// registry has evicted and closed. database/sql.Close does NOT end a
// transaction already running on the old handle, so this takes its own
// IMMEDIATE transaction: SQLite makes it wait (busy_timeout) for any
// in-flight writer to commit or roll back before we read, so a task that
// lands is seen. No new writer can start: the closed handle refuses one
// and a reopen needs the registry lock the caller holds. A file below
// schema 11 cannot hold tasks; anything else that cannot be read is
// reported, not treated as empty: refusing is the safe direction.
func fileHasTasks(path string) (bool, error) {
	return fileHasRows(path, 11, `SELECT EXISTS(SELECT 1 FROM tasks)`)
}

func fileHasRows(path string, minimumVersion int, query string) (bool, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return false, nil // no database (interrupted first open): nothing to keep
	}
	sdb, err := sql.Open("sqlite", "file:"+path+"?mode=rw&_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		return false, err
	}
	defer sdb.Close()
	tx, err := sdb.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var v int
	if err := tx.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v); err != nil {
		return false, err
	}
	if v < minimumVersion {
		return false, nil
	}
	var exists bool
	err = tx.QueryRow(query).Scan(&exists)
	return exists, err
}

// TasksMetaKey is the project metadata key that says whether tasks are
// enabled for a project: "on" or "off"; unset means off. It is set only by
// an admin — the console, the admin API, the host CLI — never by an
// ordinary token, a task write or sync, and it is read by every credential
// through the identity route.
const TasksMetaKey = "tasks"

// TasksEnabled reports whether tasks are enabled for this project.
func (d *DB) TasksEnabled() (bool, error) {
	v, err := d.GetMeta(TasksMetaKey)
	if err != nil {
		return false, err
	}
	return v == "on", nil
}

// EnableTasksWherePresent is the upgrade migration for per-project
// enablement: every ordinary project that already holds a task (archived
// included) and has no setting yet is switched on. It only ever writes
// where the key is unset, so running it at every start is the same as
// running it once: a later admin "off" is never undone, and a project
// created after the upgrade stays off until an admin switches it on. A
// project that cannot be opened is skipped and reported; the rest are
// still handled. Returns the ids it enabled.
func (r *Registry) EnableTasksWherePresent() ([]string, error) {
	projects, err := r.Projects()
	if err != nil {
		return nil, err
	}
	var enabled []string
	var skipped error
	for _, p := range projects {
		if IsReservedProject(p) {
			continue
		}
		db, err := r.OpenExisting(p)
		if err != nil {
			skipped = errors.Join(skipped, fmt.Errorf("project %q: %w", p, err))
			continue
		}
		v, err := db.GetMeta(TasksMetaKey)
		if err != nil {
			skipped = errors.Join(skipped, fmt.Errorf("project %q: %w", p, err))
			continue
		}
		if v != "" {
			continue
		}
		has, err := db.HasTasks()
		if err != nil {
			skipped = errors.Join(skipped, fmt.Errorf("project %q: %w", p, err))
			continue
		}
		if !has {
			continue
		}
		if err := db.SetMeta(TasksMetaKey, "on"); err != nil {
			skipped = errors.Join(skipped, fmt.Errorf("project %q: %w", p, err))
			continue
		}
		enabled = append(enabled, p)
	}
	return enabled, skipped
}
