package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aimem/internal/uuidv7"
)

var (
	adminActor = TaskActor{Kind: "admin", Name: "host-admin"}
	aliceActor = TaskActor{Kind: "user", UserID: uuidv7.New(), TokenID: uuidv7.New(), Name: "alice"}
)

func taskDB(t *testing.T) (*Registry, *DB) {
	t.Helper()
	r := newTestRegistry(t)
	db, err := r.Open("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	return r, db
}

// content is a full editable payload: updates REPLACE every field, so the
// state is explicit here; only CreateTask defaults an empty state.
func content(title string) TaskContent {
	return TaskContent{Title: title, State: "BACKLOG", Objective: "ship it", NextAction: "start"}
}

func mustCreate(t *testing.T, db *DB, title, key string) Task {
	t.Helper()
	task, err := db.CreateTask(content(title), aliceActor, key)
	if err != nil {
		t.Fatalf("create %q: %v", title, err)
	}
	return task
}

func countRows(t *testing.T, db *DB, table, where string, args ...any) int {
	t.Helper()
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Schema 11 migrates a real v10 file additively and idempotently, keeps
// journal/docs/collections, and a newer schema is refused.
func TestMigrationV10ToV11(t *testing.T) {
	root := t.TempDir()
	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := r.Open("proj-a")
	db.Append(testEvent("k1", "t1"))
	if _, err := db.PutDoc("RUNBOOK", "body\n", "t", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PutRecord("api", "a/b", []byte(`{}`), "t", 0, false); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`DROP TABLE task_requests`, `DROP TABLE task_comments`, `DROP TABLE task_history`, `DROP TABLE tasks`,
		`UPDATE meta SET value='10' WHERE key='schema_version'`,
	} {
		if _, err := db.sql.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	for pass := 1; pass <= 2; pass++ {
		r2, err := NewRegistry(root)
		if err != nil {
			t.Fatal(err)
		}
		db2, err := r2.OpenExisting("proj-a")
		if err != nil {
			t.Fatalf("pass %d: migration failed: %v", pass, err)
		}
		var v string
		db2.sql.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
		if v != "11" {
			t.Fatalf("pass %d: schema_version=%q", pass, v)
		}
		for _, tbl := range []string{"tasks", "task_history", "task_comments", "task_requests"} {
			var name string
			if db2.sql.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name); name == "" {
				t.Fatalf("pass %d: table %s missing", pass, tbl)
			}
		}
		if evs, _ := db2.RecentEvents(10); len(evs) != 1 {
			t.Fatalf("pass %d: journal lost: %d events", pass, len(evs))
		}
		if docs, _ := db2.ListDocs(); len(docs) != 1 {
			t.Fatalf("pass %d: docs lost", pass)
		}
		if cols, _ := db2.ListCollections(); len(cols) != 1 {
			t.Fatalf("pass %d: collections lost", pass)
		}
		if pass == 2 {
			db2.sql.Exec(`UPDATE meta SET value='12' WHERE key='schema_version'`)
		}
		r2.Close()
	}
	r3, _ := NewRegistry(root)
	defer r3.Close()
	if _, err := r3.OpenExisting("proj-a"); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer schema must be refused, got %v", err)
	}
}

func TestTaskCreateDefaultsAndReads(t *testing.T) {
	_, db := taskDB(t)
	task, err := db.CreateTask(TaskContent{Title: "first"}, aliceActor, "k-create-1") // no state: defaults
	if err != nil {
		t.Fatal(err)
	}
	if task.State != "BACKLOG" || task.Revision != 1 || task.CreatedAt == "" || task.CreatedAt != task.UpdatedAt {
		t.Fatalf("defaults: %+v", task)
	}
	if !taskIDRE.MatchString(task.ID) {
		t.Fatalf("id shape: %s", task.ID)
	}
	if task.Dependencies == nil || task.CandidateRefs == nil || task.EvidenceRefs == nil {
		t.Fatal("list fields must be empty lists, not null")
	}
	got, err := db.GetTask(task.ID)
	if err != nil || got.ID != task.ID || got.Title != "first" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := db.GetTask(uuidv7.New()); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing task: %v", err)
	}
	if _, err := db.GetTask("not-an-id"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("malformed id: %v", err)
	}
	h, err := db.TaskHistory(task.ID, 0, 0)
	if err != nil || len(h.Changes) != 1 || h.Changes[0].Task.Revision != 1 || h.Changes[0].Actor != aliceActor {
		t.Fatalf("first history row: %+v %v", h, err)
	}
}

func TestTaskValidationBounds(t *testing.T) {
	_, db := taskDB(t)
	bad := func(name string, c TaskContent, want string) {
		t.Helper()
		if _, err := db.CreateTask(c, aliceActor, "k-"+name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: accepted or wrong error: %v", name, err)
		}
	}
	bad("blank title", content("   "), "title")
	bad("long title", content(strings.Repeat("x", MaxTaskTitleBytes+1)), "title")
	bad("bad state", TaskContent{Title: "t", State: "DOING"}, "invalid task state")
	bad("archive non-terminal", TaskContent{Title: "t", State: "READY", Archived: true}, "DONE or CANCELLED")
	bad("bad assignee", TaskContent{Title: "t", Assignee: &TaskAssignee{Kind: "robot", ID: uuidv7.New()}}, "assignee")
	bad("bad assignee id", TaskContent{Title: "t", Assignee: &TaskAssignee{Kind: "user", ID: "alice"}}, "assignee")
	bad("bad dependency", TaskContent{Title: "t", Dependencies: []string{"nope"}}, "dependency")
	many := make([]string, MaxTaskListEntries+1)
	for i := range many {
		many[i] = "ref"
	}
	bad("too many refs", TaskContent{Title: "t", EvidenceRefs: many}, "at most")
	bad("long ref", TaskContent{Title: "t", CandidateRefs: []string{strings.Repeat("r", MaxTaskRefBytes+1)}}, "candidate_refs")
	bad("control char", TaskContent{Title: "t\x00"}, "control")
	bad("control char in ref", TaskContent{Title: "t", EvidenceRefs: []string{"r\x00"}}, "evidence_refs")
	bad("secret in ref", TaskContent{Title: "t", CandidateRefs: []string{"-----BEGIN RSA PRIVATE KEY-----\nMIIE"}}, "secret")
	bad("blank ref", TaskContent{Title: "t", CandidateRefs: []string{" "}}, "candidate_refs")
	bad("invalid utf8", TaskContent{Title: "t\xff"}, "UTF-8")
	bad("secret", TaskContent{Title: "t", Objective: "-----BEGIN RSA PRIVATE KEY-----\nMIIE"}, "secret")
	bad("oversized content", TaskContent{Title: "t", Objective: strings.Repeat("a", MaxTaskBytes)}, "limit")
	// Exact boundaries are accepted.
	if _, err := db.CreateTask(content(strings.Repeat("x", MaxTaskTitleBytes)), aliceActor, "k-title-max"); err != nil {
		t.Fatalf("title at limit: %v", err)
	}
	if _, err := db.CreateTask(TaskContent{Title: "t", CandidateRefs: []string{strings.Repeat("r", MaxTaskRefBytes)}}, aliceActor, strings.Repeat("k", MaxTaskKeyBytes)); err != nil {
		t.Fatalf("ref and key at limit: %v", err)
	}
	full := make([]string, MaxTaskListEntries)
	for i := range full {
		full[i] = "ref"
	}
	if _, err := db.CreateTask(TaskContent{Title: "t", EvidenceRefs: full}, aliceActor, "k-refs-max"); err != nil {
		t.Fatalf("%d refs: %v", MaxTaskListEntries, err)
	}
	exact := TaskContent{Title: "t", State: "BACKLOG", Dependencies: []string{}, CandidateRefs: []string{}, EvidenceRefs: []string{}}
	b, _ := json.Marshal(exact)
	exact.Objective = strings.Repeat("a", MaxTaskBytes-len(b))
	if b, _ := json.Marshal(exact); len(b) != MaxTaskBytes {
		t.Fatalf("test setup: %d bytes", len(b))
	}
	if _, err := db.CreateTask(exact, aliceActor, "k-json-max"); err != nil {
		t.Fatalf("content at exactly %d bytes: %v", MaxTaskBytes, err)
	}
	exact.Objective += "a"
	bad("one byte over", exact, "limit")
	bad("bidi override", TaskContent{Title: "fix\u202Eauth"}, "bidirectional")
	// Caller values never come back in the error: a secret-shaped state or
	// dependency is neither stored nor echoed.
	for name, c := range map[string]TaskContent{
		"secret state":      {Title: "t", State: "-----BEGIN RSA PRIVATE KEY-----"},
		"secret dependency": {Title: "t", Dependencies: []string{"sk-" + strings.Repeat("a", 40)}},
		"huge state":        {Title: "t", State: strings.Repeat("S", 1<<20)},
	} {
		_, err := db.CreateTask(c, aliceActor, "k-"+name)
		if err == nil || strings.Contains(err.Error(), "BEGIN") || strings.Contains(err.Error(), "sk-") || len(err.Error()) > 200 {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := db.ListTasks(TaskFilter{State: "-----BEGIN RSA PRIVATE KEY-----"}); err == nil || strings.Contains(err.Error(), "BEGIN") {
		t.Fatalf("filter echo: %v", err)
	}
	// Idempotency key bounds and secrets.
	if _, err := db.CreateTask(content("t"), aliceActor, ""); err == nil || !strings.Contains(err.Error(), "idempotency key") {
		t.Fatalf("blank key: %v", err)
	}
	if _, err := db.CreateTask(content("t"), aliceActor, strings.Repeat("k", MaxTaskKeyBytes+1)); err == nil {
		t.Fatal("oversized key accepted")
	}
	// Actor shapes.
	if _, err := db.CreateTask(content("t"), TaskActor{Kind: "user", Name: "x"}, "k-actor"); err == nil {
		t.Fatal("user actor without stable ids accepted")
	}
	if _, err := db.CreateTask(content("t"), TaskActor{Kind: "admin", UserID: uuidv7.New(), Name: "x"}, "k-actor2"); err == nil {
		t.Fatal("admin actor carrying user id accepted")
	}
	if _, err := db.CreateTask(content("t"), TaskActor{Kind: "admin", Name: ""}, "k-actor3"); err == nil {
		t.Fatal("blank actor name accepted")
	}
	if _, err := db.CreateTask(content("t"), TaskActor{Kind: "writer", Name: "legacy"}, "k-actor4"); err == nil {
		t.Fatal("unknown actor kind accepted")
	}
}

// The indexed filter columns and the JSON snapshot are written together;
// they must never disagree.
func TestTaskFilterColumnsMatchSnapshot(t *testing.T) {
	_, db := taskDB(t)
	who := &TaskAssignee{Kind: "group", ID: uuidv7.New()}
	c := content("cols")
	c.State, c.Assignee = "READY", who
	task, err := db.CreateTask(c, aliceActor, "k-cols")
	if err != nil {
		t.Fatal(err)
	}
	check := func(want Task) {
		t.Helper()
		var state, assignee, body string
		var archived int
		if err := db.sql.QueryRow(`SELECT state, archived, assignee, body FROM tasks WHERE id=?`, want.ID).Scan(&state, &archived, &assignee, &body); err != nil {
			t.Fatal(err)
		}
		var stored Task // the persisted snapshot, not the value the API returned
		if err := json.Unmarshal([]byte(body), &stored); err != nil {
			t.Fatal(err)
		}
		if state != stored.State || (archived == 1) != stored.Archived || assignee != stored.Assignee.column() {
			t.Fatalf("columns (%s,%d,%s) disagree with the stored snapshot %+v", state, archived, assignee, stored.TaskContent)
		}
		if stored.Revision != want.Revision || stored.State != want.State || stored.Archived != want.Archived || stored.Assignee.column() != want.Assignee.column() {
			t.Fatalf("stored snapshot %+v disagrees with the returned task %+v", stored, want)
		}
		if got, err := db.GetTask(want.ID); err != nil || got.Revision != want.Revision || got.State != want.State {
			t.Fatalf("GetTask %+v %v", got, err)
		}
	}
	check(task)
	c.State, c.Assignee, c.Archived = "DONE", nil, true
	task, err = db.UpdateTask(task.ID, c, 1, adminActor, "k-cols-2")
	if err != nil {
		t.Fatal(err)
	}
	check(task)
}

// A stale expected revision is a typed conflict carrying the CURRENT task,
// and nothing is written for it.
func TestTaskCASConflictCarriesCurrent(t *testing.T) {
	_, db := taskDB(t)
	task := mustCreate(t, db, "v1", "k-v1")
	if _, err := db.UpdateTask(task.ID, content("v2"), 1, adminActor, "k-v2"); err != nil {
		t.Fatal(err)
	}
	var tc *TaskConflict
	_, err := db.UpdateTask(task.ID, content("stale"), 1, aliceActor, "k-stale")
	if !errors.As(err, &tc) {
		t.Fatalf("stale revision: %v", err)
	}
	if tc.Current.Revision != 2 || tc.Current.Title != "v2" || tc.Current.ID != task.ID {
		t.Fatalf("conflict must carry the current task: %+v", tc.Current)
	}
	if n := countRows(t, db, "task_history", "task_id=?", task.ID); n != 2 {
		t.Fatalf("history rows after a refused update: %d", n)
	}
	if n := countRows(t, db, "task_requests", "key=?", "k-stale"); n != 0 {
		t.Fatal("a refused update must not consume its key")
	}
}

// Two competing CAS updates: exactly one wins, the loser gets the current
// task, and each accepted revision has exactly one actor-stamped history row.
func TestTaskCASRace(t *testing.T) {
	_, db := taskDB(t)
	task := mustCreate(t, db, "race", "k-race")
	actors := []TaskActor{aliceActor, adminActor}
	results := make([]error, 2)
	var wg sync.WaitGroup
	for i := range actors {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := content("race by " + actors[i].Name)
			_, results[i] = db.UpdateTask(task.ID, c, 1, actors[i], "k-race-"+actors[i].Name)
		}(i)
	}
	wg.Wait()
	wins, conflicts := 0, 0
	for _, err := range results {
		var tc *TaskConflict
		switch {
		case err == nil:
			wins++
		case errors.As(err, &tc):
			conflicts++
			if tc.Current.Revision != 2 {
				t.Fatalf("loser must see the winner's revision, got %d", tc.Current.Revision)
			}
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	if n := countRows(t, db, "task_history", "task_id=?", task.ID); n != 2 {
		t.Fatalf("history rows: %d (want exactly one per accepted revision)", n)
	}
	h, _ := db.TaskHistory(task.ID, 0, 0)
	if h.Changes[1].Task.Revision != 2 || h.Changes[1].Actor.Name == "" || h.Changes[1].Task.Title != "race by "+h.Changes[1].Actor.Name {
		t.Fatalf("history row 2 must carry the winner's snapshot and actor: %+v", h.Changes[1])
	}
	// The identical retry of the winner returns its original result; the
	// loser retrying its own key still conflicts (input unchanged, task
	// advanced) — receipts never store failures.
	for i, err := range results {
		c := content("race by " + actors[i].Name)
		again, err2 := db.UpdateTask(task.ID, c, 1, actors[i], "k-race-"+actors[i].Name)
		if err == nil && (err2 != nil || again.Revision != 2) {
			t.Fatalf("winner replay: %+v %v", again, err2)
		}
		if err != nil && err2 == nil {
			t.Fatal("loser's failed attempt must not have consumed its key into a success")
		}
	}
}

// A failure anywhere in a mutation rolls back the task row, its history
// and the receipt together, and the key stays usable.
func TestTaskMutationRollsBackTogether(t *testing.T) {
	_, db := taskDB(t)
	boom := errors.New("injected after the rows were written")
	_, err := taskMutation(db, aliceActor, "create", "", "k-rollback", "input", func(tx *sql.Tx) (Task, error) {
		task := Task{ID: uuidv7.New(), Revision: 1, TaskContent: content("ghost"), CreatedAt: nowUTC(), UpdatedAt: nowUTC()}
		if err := saveTask(tx, task, aliceActor, true); err != nil {
			return Task{}, err
		}
		return task, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("expected the injected error, got %v", err)
	}
	if countRows(t, db, "tasks", "1=1") != 0 || countRows(t, db, "task_history", "1=1") != 0 || countRows(t, db, "task_requests", "1=1") != 0 {
		t.Fatal("rolled-back mutation left rows behind")
	}
	if _, err := db.CreateTask(content("real"), aliceActor, "k-rollback"); err != nil {
		t.Fatalf("key must stay usable after a failed attempt: %v", err)
	}
}

func TestTaskRetryReceipts(t *testing.T) {
	root := t.TempDir()
	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := r.Open("proj-a")
	first := mustCreate(t, db, "once", "k-once")
	again, err := db.CreateTask(content("once"), aliceActor, "k-once")
	if err != nil || again.ID != first.ID {
		t.Fatalf("replay must return the original task: %+v %v", again, err)
	}
	if countRows(t, db, "tasks", "1=1") != 1 {
		t.Fatal("replay created a second task")
	}
	if _, err := db.CreateTask(content("changed"), aliceActor, "k-once"); !errors.Is(err, ErrTaskRetryConflict) {
		t.Fatalf("same key, different input: %v", err)
	}
	// Scopes: another actor, another operation, another task reuse the
	// key text independently.
	if other, err := db.CreateTask(content("once"), adminActor, "k-once"); err != nil || other.ID == first.ID {
		t.Fatalf("other actor's key must be independent: %+v %v", other, err)
	}
	upd, err := db.UpdateTask(first.ID, content("v2"), 1, aliceActor, "k-once")
	if err != nil || upd.Revision != 2 {
		t.Fatalf("update with the create key text is a different scope: %+v %v", upd, err)
	}
	// An update replay returns the ORIGINAL revision even after the task
	// advanced further.
	if _, err := db.UpdateTask(first.ID, content("v3"), 2, aliceActor, "k-v3"); err != nil {
		t.Fatal(err)
	}
	replay, err := db.UpdateTask(first.ID, content("v2"), 1, aliceActor, "k-once")
	if err != nil || replay.Revision != 2 || replay.Title != "v2" {
		t.Fatalf("update replay must return the original result: %+v %v", replay, err)
	}
	// Receipts survive a close/reopen (they live with the task data).
	r.Close()
	r2, _ := NewRegistry(root)
	defer r2.Close()
	db2, _ := r2.OpenExisting("proj-a")
	again2, err := db2.CreateTask(content("once"), aliceActor, "k-once")
	if err != nil || again2.ID != first.ID {
		t.Fatalf("replay after reopen: %+v %v", again2, err)
	}
	if countRows(t, db2, "tasks", "1=1") != 2 {
		t.Fatal("reopen changed the task count")
	}
}

// The receipt scope is the task: the same actor, operation and key on two
// tasks are two mutations, not a replay.
func TestTaskReceiptScopeIsPerTask(t *testing.T) {
	_, db := taskDB(t)
	a := mustCreate(t, db, "a", "k-a")
	b := mustCreate(t, db, "b", "k-b")
	ca, err := db.AddTaskComment(a.ID, "same key", aliceActor, "c-1")
	if err != nil {
		t.Fatal(err)
	}
	cb, err := db.AddTaskComment(b.ID, "same key", aliceActor, "c-1")
	if err != nil {
		t.Fatalf("same key on another task must be a new comment: %v", err)
	}
	if cb.ID == ca.ID || cb.TaskID != b.ID {
		t.Fatalf("scope leaked across tasks: %+v vs %+v", ca, cb)
	}
	if again, err := db.AddTaskComment(a.ID, "same key", aliceActor, "c-1"); err != nil || again.ID != ca.ID {
		t.Fatalf("replay on the first task: %+v %v", again, err)
	}
	if n := countRows(t, db, "task_comments", "1=1"); n != 2 {
		t.Fatalf("comments: %d", n)
	}
	// Operation is part of the key: update and comment may share one.
	if _, err := db.UpdateTask(a.ID, content("a2"), 1, aliceActor, "shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTaskComment(a.ID, "after update", aliceActor, "shared"); err != nil {
		t.Fatalf("same key across operations must not collide: %v", err)
	}
	// The token is part of the principal: a fresh token replaying a create
	// key (same user, same content) is a new mutation, not a replay.
	other := TaskActor{Kind: "user", UserID: aliceActor.UserID, TokenID: uuidv7.New(), Name: aliceActor.Name}
	again, err := db.CreateTask(content("a"), other, "k-a")
	if err != nil || again.ID == a.ID {
		t.Fatalf("new token must get its own task: %+v %v", again, err)
	}
}

func TestTaskComments(t *testing.T) {
	_, db := taskDB(t)
	task := mustCreate(t, db, "discuss", "k-discuss")
	c1, err := db.AddTaskComment(task.ID, "first note", aliceActor, "c-1")
	if err != nil || c1.Sequence != 1 || c1.TaskID != task.ID || c1.Actor != aliceActor || c1.CreatedAt == "" {
		t.Fatalf("append: %+v %v", c1, err)
	}
	if got, _ := db.GetTask(task.ID); got.Revision != 1 {
		t.Fatal("a comment must not bump the task revision")
	}
	if countRows(t, db, "task_history", "task_id=?", task.ID) != 1 {
		t.Fatal("a comment must not add a history row")
	}
	// Replay returns the same comment; changed body conflicts.
	if again, err := db.AddTaskComment(task.ID, "first note", aliceActor, "c-1"); err != nil || again.ID != c1.ID {
		t.Fatalf("comment replay: %+v %v", again, err)
	}
	if _, err := db.AddTaskComment(task.ID, "edited", aliceActor, "c-1"); !errors.Is(err, ErrTaskRetryConflict) {
		t.Fatalf("changed body under the same key: %v", err)
	}
	// Concurrent independent appends both succeed with distinct sequences.
	var wg sync.WaitGroup
	got := make([]TaskComment, 2)
	errs := make([]error, 2)
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = db.AddTaskComment(task.ID, "parallel "+string(rune('a'+i)), adminActor, "c-par-"+string(rune('a'+i)))
		}(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || got[0].Sequence == got[1].Sequence {
		t.Fatalf("concurrent appends: %+v %+v %v %v", got[0], got[1], errs[0], errs[1])
	}
	// Pagination is stable while later comments arrive.
	page, err := db.TaskComments(task.ID, 0, 2)
	if err != nil || len(page.Comments) != 2 || page.Next != page.Comments[1].Sequence {
		t.Fatalf("page 1: %+v %v", page, err)
	}
	if _, err := db.AddTaskComment(task.ID, "late arrival", aliceActor, "c-late"); err != nil {
		t.Fatal(err)
	}
	page2, err := db.TaskComments(task.ID, page.Next, 2)
	if err != nil || len(page2.Comments) != 2 || page2.Comments[0].Sequence <= page.Next {
		t.Fatalf("page 2 must continue after the cursor: %+v %v", page2, err)
	}
	// Wrong parent is not found; direct fetch works.
	if _, err := db.GetTaskComment(uuidv7.New(), c1.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("wrong parent: %v", err)
	}
	if one, err := db.GetTaskComment(task.ID, c1.ID); err != nil || one.Body != "first note" {
		t.Fatalf("direct fetch: %+v %v", one, err)
	}
	// Archive → new appends refused, replay of a committed append still
	// returns the original, unarchive → appends resume.
	done := content("discuss")
	done.State, done.Archived = "DONE", true
	if _, err := db.UpdateTask(task.ID, done, 1, adminActor, "k-archive"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTaskComment(task.ID, "too late", aliceActor, "c-archived"); !errors.Is(err, ErrTaskArchived) {
		t.Fatalf("append to archived task: %v", err)
	}
	if again, err := db.AddTaskComment(task.ID, "first note", aliceActor, "c-1"); err != nil || again.ID != c1.ID {
		t.Fatalf("committed append must replay after archival: %+v %v", again, err)
	}
	if all, _ := db.TaskComments(task.ID, 0, 100); len(all.Comments) != 4 {
		t.Fatalf("comments remain readable after archival: %d", len(all.Comments))
	}
	done.Archived = false
	if _, err := db.UpdateTask(task.ID, done, 2, adminActor, "k-unarchive"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTaskComment(task.ID, "reopened discussion", aliceActor, "c-again"); err != nil {
		t.Fatalf("append after unarchive: %v", err)
	}
	// Bounds.
	if _, err := db.AddTaskComment(task.ID, strings.Repeat("b", MaxTaskCommentBytes+1), aliceActor, "c-big"); err == nil {
		t.Fatal("oversized comment accepted")
	}
	if _, err := db.AddTaskComment(task.ID, "   ", aliceActor, "c-blank"); err == nil {
		t.Fatal("blank comment accepted")
	}
	if _, err := db.AddTaskComment(task.ID, "nul\x00", aliceActor, "c-nul"); err == nil || !strings.Contains(err.Error(), "control") {
		t.Fatalf("control character in comment: %v", err)
	}
	if _, err := db.AddTaskComment(task.ID, "bad \xff utf8", aliceActor, "c-utf8"); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 in comment: %v", err)
	}
	if _, err := db.AddTaskComment(task.ID, "-----BEGIN RSA PRIVATE KEY-----\nMIIE", aliceActor, "c-secret"); err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("secret in comment: %v", err)
	}
	if _, err := db.AddTaskComment(task.ID, strings.Repeat("b", MaxTaskCommentBytes), aliceActor, "c-max"); err != nil {
		t.Fatalf("comment at limit: %v", err)
	}
	if _, err := db.AddTaskComment(uuidv7.New(), "orphan", aliceActor, "c-orphan"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("comment on missing task: %v", err)
	}
}

func TestTaskListFiltersAndPaging(t *testing.T) {
	_, db := taskDB(t)
	who := &TaskAssignee{Kind: "user", ID: uuidv7.New()}
	for i := range 25 {
		c := content("t" + string(rune('a'+i%26)))
		if i%5 == 0 {
			c.State = "READY"
		}
		if i%7 == 0 {
			c.Assignee = who
		}
		if _, err := db.CreateTask(c, aliceActor, "k-list-"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	page, err := db.ListTasks(TaskFilter{})
	if err != nil || len(page.Tasks) != 20 || page.Next == "" {
		t.Fatalf("default page: %d %q %v", len(page.Tasks), page.Next, err)
	}
	rest, err := db.ListTasks(TaskFilter{After: page.Next})
	if err != nil || len(rest.Tasks) != 5 || rest.Next != "" {
		t.Fatalf("second page: %d %q %v", len(rest.Tasks), rest.Next, err)
	}
	if ready, _ := db.ListTasks(TaskFilter{State: "READY"}); len(ready.Tasks) != 5 {
		t.Fatalf("state filter: %d", len(ready.Tasks))
	}
	if mine, _ := db.ListTasks(TaskFilter{Assignee: who}); len(mine.Tasks) != 4 {
		t.Fatalf("assignee filter: %d", len(mine.Tasks))
	}
	// Archive one DONE task: excluded by default, included on request.
	first := page.Tasks[0]
	done := content(first.Title)
	done.State, done.Archived = "DONE", true
	if _, err := db.UpdateTask(first.ID, done, 1, adminActor, "k-arch"); err != nil {
		t.Fatal(err)
	}
	if all, _ := db.ListTasks(TaskFilter{Limit: 100}); len(all.Tasks) != 24 {
		t.Fatalf("archived must be excluded by default: %d", len(all.Tasks))
	}
	if all, _ := db.ListTasks(TaskFilter{Limit: 100, IncludeArchived: true}); len(all.Tasks) != 25 {
		t.Fatalf("include archived: %d", len(all.Tasks))
	}
	for _, f := range []TaskFilter{{Limit: 101}, {Limit: -1}, {State: "nope"}, {After: "bad"}, {Assignee: &TaskAssignee{Kind: "user", ID: "x"}}} {
		if _, err := db.ListTasks(f); err == nil {
			t.Fatalf("filter %+v accepted", f)
		}
	}
	if _, err := db.TaskHistory(first.ID, -1, 0); err == nil {
		t.Fatal("negative history cursor accepted")
	}
	if _, err := db.TaskComments(first.ID, 0, 101); err == nil {
		t.Fatal("comment limit above 100 accepted")
	}
}

// History is retained beyond any collection-style revision cap and pages
// in revision order.
func TestTaskHistoryRetainedBeyond20(t *testing.T) {
	_, db := taskDB(t)
	task := mustCreate(t, db, "long-lived", "k-hist")
	for rev := int64(1); rev <= 24; rev++ {
		if _, err := db.UpdateTask(task.ID, content("edit "+string(rune('a'+rev))), rev, aliceActor, "k-hist-"+string(rune('a'+rev))); err != nil {
			t.Fatal(err)
		}
	}
	p1, err := db.TaskHistory(task.ID, 0, 0)
	if err != nil || len(p1.Changes) != 20 || p1.Next != 20 {
		t.Fatalf("page 1: %d next=%d %v", len(p1.Changes), p1.Next, err)
	}
	p2, err := db.TaskHistory(task.ID, p1.Next, 0)
	if err != nil || len(p2.Changes) != 5 || p2.Next != 0 || p2.Changes[4].Task.Revision != 25 {
		t.Fatalf("page 2: %d next=%d %v", len(p2.Changes), p2.Next, err)
	}
	if n := countRows(t, db, "task_history", "task_id=?", task.ID); n != 25 {
		t.Fatalf("history pruned: %d rows", n)
	}
}

func TestTaskReservedScopesRefused(t *testing.T) {
	r := newTestRegistry(t)
	for _, id := range []string{UserScopeProject, "group-kb"} {
		db, err := r.Open(id)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CreateTask(content("nope"), adminActor, "k-reserved"); !errors.Is(err, ErrTaskReservedScope) {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

// Rename keeps tasks, comments, history and receipts reachable under the
// new project id; the snapshot never carried the old name.
func TestTaskRenamePreserves(t *testing.T) {
	r, db := taskDB(t)
	task := mustCreate(t, db, "moves", "k-move")
	c, err := db.AddTaskComment(task.ID, "before rename", aliceActor, "c-move")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Rename("proj-a", "proj-b"); err != nil {
		t.Fatal(err)
	}
	db2, err := r.OpenExisting("proj-b")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := db2.GetTask(task.ID); err != nil || got.Title != "moves" {
		t.Fatalf("task after rename: %+v %v", got, err)
	}
	if got, err := db2.GetTaskComment(task.ID, c.ID); err != nil || got.Body != "before rename" {
		t.Fatalf("comment after rename: %+v %v", got, err)
	}
	if again, err := db2.CreateTask(content("moves"), aliceActor, "k-move"); err != nil || again.ID != task.ID {
		t.Fatalf("receipt after rename: %+v %v", again, err)
	}
	if _, err := r.OpenExisting("proj-a"); err == nil {
		t.Fatal("old id must be gone")
	}
}

// Drop and source-merge refuse a project that holds any task — even a
// single archived one — and leave it intact.
func TestDropAndMergeRefuseTaskBearingProjects(t *testing.T) {
	r, db := taskDB(t)
	task := mustCreate(t, db, "keep me", "k-keep")
	done := content("keep me")
	done.State, done.Archived = "DONE", true
	if _, err := db.UpdateTask(task.ID, done, 1, adminActor, "k-keep-arch"); err != nil {
		t.Fatal(err)
	}
	if err := r.Drop("proj-a"); !errors.Is(err, ErrProjectHasTasks) {
		t.Fatalf("drop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(r.Root(), "projects", "proj-a", "journal.db")); err != nil {
		t.Fatal("refused drop must leave the project on disk")
	}
	if _, err := r.Open("proj-b"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := r.MergeProject("proj-a", "proj-b"); !errors.Is(err, ErrProjectHasTasks) {
		t.Fatalf("merge: %v", err)
	}
	// The project is usable again after a refused drop (handle was evicted).
	db2, err := r.OpenExisting("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := db2.GetTask(task.ID); err != nil || !got.Archived {
		t.Fatalf("task after refused drop: %+v %v", got, err)
	}
	// A task-free project still drops, and a target holding tasks still
	// accepts a merge of legacy data.
	if err := r.Drop("proj-b"); err != nil {
		t.Fatalf("drop of task-free project: %v", err)
	}
	src, _ := r.Open("proj-src")
	src.Append(testEvent("k9", "t9"))
	if events, _, _, _, err := r.MergeProject("proj-src", "proj-a"); err != nil || events != 1 {
		t.Fatalf("merge INTO a task-bearing target: events=%d %v", events, err)
	}
	if got, err := db2.GetTask(task.ID); err != nil || got.Title != "keep me" {
		t.Fatalf("target's tasks after merge: %+v %v", got, err)
	}
}

// A project directory without a database file (a first open interrupted
// before SQLite created it) holds no tasks and must still be droppable.
func TestDropProjectWithoutDatabaseFile(t *testing.T) {
	r := newTestRegistry(t)
	dir := filepath.Join(r.root, "projects", "proj-empty")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.Drop("proj-empty"); err != nil {
		t.Fatalf("drop of a database-less project dir: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("directory still present after drop: %v", err)
	}
}

// A create that already holds the write transaction when the drop starts:
// closing the cached handle does not end it (database/sql only closes idle
// connections), so the drop's check must wait for it and then see the
// committed task.
func TestDropWaitsForInFlightTaskWrite(t *testing.T) {
	r := newTestRegistry(t)
	db, err := r.Open("proj-inflight")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.sql.Begin() // immediate: holds the write lock until Commit
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: uuidv7.New(), Revision: 1, TaskContent: content("in flight"), CreatedAt: nowUTC(), UpdatedAt: nowUTC()}
	if err := saveTask(tx, task, aliceActor, true); err != nil {
		t.Fatal(err)
	}
	dropErr := make(chan error, 1)
	go func() { dropErr <- r.Drop("proj-inflight") }()
	// Drop holds the registry lock for its whole locked section; once we
	// cannot take it, Drop is inside and — with our transaction open — has
	// nowhere to go but wait.
	deadline := time.Now().Add(5 * time.Second)
	for r.mu.TryLock() {
		r.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("drop never entered its locked section")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	select {
	case err := <-dropErr:
		t.Fatalf("drop returned (%v) while a write transaction was still open", err)
	default:
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := <-dropErr; !errors.Is(err, ErrProjectHasTasks) {
		t.Fatalf("drop after the in-flight create committed: %v", err)
	}
	// A refused drop leaves the handle other callers hold usable.
	if got, err := db.GetTask(task.ID); err != nil || got.Title != "in flight" {
		t.Fatalf("task through the original handle after a refused drop: %+v %v", got, err)
	}
	db2, err := r.OpenExisting("proj-inflight")
	if err != nil {
		t.Fatalf("project vanished: %v", err)
	}
	if got, err := db2.GetTask(task.ID); err != nil || got.Title != "in flight" {
		t.Fatalf("task after refused drop: %+v %v", got, err)
	}
}

// Files the dropping registry never opened: a legacy (pre-v11) file holds
// no tasks and is dropped; a v11 file that lost its tasks table but still
// holds history, or a file that is not a database, is refused — deletion
// never proceeds on a file that cannot be verified.
func TestDropChecksTasksOnUnopenedFiles(t *testing.T) {
	root := t.TempDir()
	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := r.Open("proj-legacy")
	for _, stmt := range []string{
		`DROP TABLE task_requests`, `DROP TABLE task_comments`, `DROP TABLE task_history`, `DROP TABLE tasks`,
		`UPDATE meta SET value='10' WHERE key='schema_version'`,
	} {
		if _, err := legacy.sql.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	broken, _ := r.Open("proj-broken")
	mustCreate(t, broken, "orphaned history", "k-orphan")
	for _, stmt := range []string{`PRAGMA foreign_keys=OFF`, `DROP TABLE tasks`} {
		if _, err := broken.sql.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	garbage := filepath.Join(root, "projects", "proj-garbage")
	if err := os.MkdirAll(garbage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(garbage, "journal.db"), []byte("not a database"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.Close()
	r2, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if err := r2.Drop("proj-legacy"); err != nil {
		t.Fatalf("legacy file must drop: %v", err)
	}
	for _, id := range []string{"proj-broken", "proj-garbage"} {
		err := r2.Drop(id)
		if err == nil || errors.Is(err, ErrProjectHasTasks) {
			t.Fatalf("%s: unverifiable file must be refused with its cause, got %v", id, err)
		}
		if _, err := os.Stat(filepath.Join(root, "projects", id)); err != nil {
			t.Fatalf("%s: directory removed despite the refusal: %v", id, err)
		}
	}
}

// The same for a merge: a task that lands in the source while the history
// is being copied keeps the source. The writer uses its own connection so
// the early check (cached handle, committed snapshot) passes and the late,
// locked re-check is the one that must catch it.
func TestMergeKeepsSourceThatGainsTaskDuringCopy(t *testing.T) {
	r := newTestRegistry(t)
	src, err := r.Open("proj-src")
	if err != nil {
		t.Fatal(err)
	}
	src.Append(testEvent("k1", "t1"))
	if _, err := r.Open("proj-dst"); err != nil {
		t.Fatal(err)
	}
	direct, err := sql.Open("sqlite", "file:"+src.path+"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	tx, err := direct.Begin()
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: uuidv7.New(), Revision: 1, TaskContent: content("late arrival"), CreatedAt: nowUTC(), UpdatedAt: nowUTC()}
	if err := saveTask(tx, task, aliceActor, true); err != nil {
		t.Fatal(err)
	}
	mergeErr := make(chan error, 1)
	var merged int
	go func() {
		events, _, _, _, err := r.MergeProject("proj-src", "proj-dst")
		merged = events
		mergeErr <- err
	}()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-mergeErr:
		t.Fatalf("merge returned (%v) while a write transaction was still open on the source", err)
	default:
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-mergeErr; !errors.Is(err, ErrProjectHasTasks) {
		t.Fatalf("merge must keep a source that gained a task: %v", err)
	}
	if merged != 1 {
		t.Fatalf("the refusal must come from the late check, after the copy: events=%d", merged)
	}
	src2, err := r.OpenExisting("proj-src")
	if err != nil {
		t.Fatalf("source vanished: %v", err)
	}
	if got, err := src2.GetTask(task.ID); err != nil || got.Title != "late arrival" {
		t.Fatalf("task after kept source: %+v %v", got, err)
	}
	dst, _ := r.OpenExisting("proj-dst")
	if has, _ := dst.HasTasks(); has {
		t.Fatal("merge must never move tasks")
	}
}

// Concurrent create versus drop, unsynchronized: whichever wins, no task
// is ever lost. A drop that succeeds means no create committed; a create
// that committed means the drop refused.
func TestDropRacesTaskCreation(t *testing.T) {
	for round := range 12 {
		r := newTestRegistry(t)
		db, err := r.Open("proj-race")
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var createErr, dropErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, createErr = db.CreateTask(content("racer"), aliceActor, "k-racer") }()
		go func() { defer wg.Done(); dropErr = r.Drop("proj-race") }()
		wg.Wait()
		created := createErr == nil
		dropped := dropErr == nil
		if created && dropped {
			t.Fatalf("round %d: task committed AND project dropped — data lost", round)
		}
		if !dropped && !errors.Is(dropErr, ErrProjectHasTasks) {
			t.Fatalf("round %d: drop failed for another reason: %v", round, dropErr)
		}
		if created {
			db2, err := r.OpenExisting("proj-race")
			if err != nil {
				t.Fatalf("round %d: created task's project vanished: %v", round, err)
			}
			if has, _ := db2.HasTasks(); !has {
				t.Fatalf("round %d: committed task missing after refused drop", round)
			}
		}
		r.Close()
	}
}

// LocateTask scans existing ordinary projects only, follows a rename, and
// never opens a store that cannot hold tasks.
func TestLocateTask(t *testing.T) {
	r := newTestRegistry(t)
	a, _ := r.Open("proj-a")
	b, _ := r.Open("proj-b")
	if _, err := r.Open(UserScopeProject); err != nil {
		t.Fatal(err)
	}
	ta := mustCreate(t, a, "in a", "k-a")
	tb := mustCreate(t, b, "in b", "k-b")
	if p, db, err := r.LocateTask(tb.ID); err != nil || p != "proj-b" || db != b {
		t.Fatalf("locate b: %s %v", p, err)
	}
	if _, _, err := r.LocateTask(uuidv7.New()); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if _, _, err := r.LocateTask("not-an-id"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("bad id: %v", err)
	}
	if err := r.Rename("proj-a", "proj-z"); err != nil {
		t.Fatal(err)
	}
	if p, _, err := r.LocateTask(ta.ID); err != nil || p != "proj-z" {
		t.Fatalf("after rename: %s %v", p, err)
	}
	// A dropped (task-free) project simply disappears from the scan; a
	// garbage directory that cannot be opened is reported, not hidden.
	if err := os.MkdirAll(filepath.Join(r.root, "projects", "proj-bad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.root, "projects", "proj-bad", "journal.db"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p, _, err := r.LocateTask(tb.ID); err != nil || p != "proj-b" {
		t.Fatalf("found despite a bad sibling: %s %v", p, err)
	}
	if _, _, err := r.LocateTask(uuidv7.New()); err == nil || errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("an unreadable project must make a miss inconclusive: %v", err)
	}
}

// Stop, copy the whole project tree, restore elsewhere: tasks, comments,
// history and receipts come back with stable IDs.
func TestTaskBackupRestore(t *testing.T) {
	root := t.TempDir()
	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := r.Open("proj-a")
	task := mustCreate(t, db, "backed up", "k-bak")
	c, err := db.AddTaskComment(task.ID, "note", aliceActor, "c-bak")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateTask(task.ID, content("backed up v2"), 1, adminActor, "k-bak-2"); err != nil {
		t.Fatal(err)
	}
	r.Close() // stopped service: WAL checkpointed, files quiescent
	dst := t.TempDir()
	if err := os.CopyFS(dst, os.DirFS(root)); err != nil {
		t.Fatal(err)
	}
	r2, err := NewRegistry(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	db2, err := r2.OpenExisting("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := db2.GetTask(task.ID); err != nil || got.Revision != 2 || got.Title != "backed up v2" {
		t.Fatalf("restored task: %+v %v", got, err)
	}
	if got, err := db2.GetTaskComment(task.ID, c.ID); err != nil || got.Body != "note" {
		t.Fatalf("restored comment: %+v %v", got, err)
	}
	if h, _ := db2.TaskHistory(task.ID, 0, 0); len(h.Changes) != 2 {
		t.Fatalf("restored history: %d", len(h.Changes))
	}
	if again, err := db2.CreateTask(content("backed up"), aliceActor, "k-bak"); err != nil || again.ID != task.ID {
		t.Fatalf("restored receipt: %+v %v", again, err)
	}
}
