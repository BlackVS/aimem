package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// setEpic writes an epic's state (and keeps everything else), failing the
// test on a refusal.
func setEpicState(t *testing.T, db *DB, id, state, key string) Epic {
	t.Helper()
	e, err := db.GetEpic(id)
	if err != nil {
		t.Fatal(err)
	}
	c := e.EpicContent
	c.State = state
	got, err := db.UpdateEpic(id, c, nil, e.Revision, aliceActor, key)
	if err != nil {
		t.Fatalf("set %s: %v", state, err)
	}
	return got
}

// tryReach tries to mark the epic REACHED and returns the refusal.
func tryReach(t *testing.T, db *DB, id, key string) error {
	t.Helper()
	e, err := db.GetEpic(id)
	if err != nil {
		t.Fatal(err)
	}
	c := e.EpicContent
	c.State = EpicReached
	_, err = db.UpdateEpic(id, c, nil, e.Revision, aliceActor, key)
	return err
}

// finish moves a record to state, with evidence where DONE needs it.
func finish(t *testing.T, db *DB, id, state, key string) Task {
	t.Helper()
	cur, err := db.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	c := cur.TaskContent
	c.State = state
	if state == "DONE" {
		c.EvidenceRefs = []TaskRef{{Kind: "url", Ref: "https://example.com/ci/1"}}
	}
	got, err := db.UpdateTask(id, c, cur.Revision, aliceActor, key)
	if err != nil {
		t.Fatalf("finish %s: %v", state, err)
	}
	return got
}

// A goal becomes REACHED only when every feature it holds is DONE or
// CANCELLED; the refusal names each unfinished one, archived ones as such.
func TestGoalReachedOnlyWithFinishedFeatures(t *testing.T) {
	_, db := taskDB(t)
	goal := goalEpic(t, db, "goal")
	a := mustCreateFeature(t, db, goal, "feature a", "fa")
	b := mustCreateFeature(t, db, goal, "feature b", "fb")
	finish(t, db, a.ID, "DONE", "fa-done")

	err := tryReach(t, db, goal, "reach-1")
	if !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), b.ID+" (BACKLOG)") || strings.Contains(err.Error(), a.ID) {
		t.Fatalf("an open feature must refuse REACHED and be named: %v", err)
	}
	if !strings.Contains(err.Error(), "set_feature") {
		t.Fatalf("the refusal names the way: %v", err)
	}
	// An archived feature that is unfinished (data no guard wrote: archiving
	// needs DONE or CANCELLED) still blocks, and is named as archived.
	if _, err := db.sql.Exec(`UPDATE tasks SET archived=1 WHERE id=?`, b.ID); err != nil {
		t.Fatal(err)
	}
	err = tryReach(t, db, goal, "reach-2")
	if !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), b.ID+" (BACKLOG, archived)") {
		t.Fatalf("an archived unfinished feature must refuse REACHED, named as archived: %v", err)
	}
	if _, err := db.sql.Exec(`UPDATE tasks SET archived=0 WHERE id=?`, b.ID); err != nil {
		t.Fatal(err)
	}
	finish(t, db, b.ID, "CANCELLED", "fb-cancel")
	e := setEpicState(t, db, goal, EpicReached, "reach-3")
	if e.State != EpicReached {
		t.Fatalf("state: %q", e.State)
	}
	// Listings leave a reached goal out by default, as a retired one.
	open, _ := db.ListEpics(false)
	all, _ := db.ListEpics(true)
	if len(open) != 0 || len(all) != 1 || all[0].State != EpicReached {
		t.Fatalf("listing: open=%v all=%v", open, all)
	}
}

// A goal closed as RETIRED before the state existed becomes REACHED by the
// same check; leaving REACHED is free; a new epic is never REACHED.
func TestGoalReachedFromRetiredAndBack(t *testing.T) {
	_, db := taskDB(t)
	goal := goalEpic(t, db, "goal")
	f := mustCreateFeature(t, db, goal, "feature", "f")
	setEpicState(t, db, goal, EpicRetired, "retire")
	if err := tryReach(t, db, goal, "reach-1"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("a retired goal with an open feature must refuse REACHED: %v", err)
	}
	finish(t, db, f.ID, "DONE", "done")
	setEpicState(t, db, goal, EpicReached, "reach-2")
	setEpicState(t, db, goal, EpicOpen, "reopen")
	setEpicState(t, db, goal, EpicReached, "reach-3")
	setEpicState(t, db, goal, EpicRetired, "retire-2")
	if _, err := db.CreateEpic(EpicContent{Title: "born reached", State: EpicReached}, aliceActor, "new"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("a new epic must not be REACHED: %v", err)
	}
}

// A reached goal refuses what a retired one refuses: a new task, a new
// feature and a feature's move into it. What it already holds survives.
func TestGoalReachedRefusesNewRecords(t *testing.T) {
	_, db := taskDB(t)
	goal := goalEpic(t, db, "goal")
	other := goalEpic(t, db, "other")
	f, tasks := featureWithTasks(t, db, goal, "f")
	for i, id := range tasks {
		finish(t, db, id, "DONE", "t-done"+string(rune('a'+i)))
	}
	finish(t, db, f.ID, "DONE", "f-done")
	setEpicState(t, db, goal, EpicReached, "reach")

	c := content("late")
	c.Epic = goal
	if _, err := db.CreateTask(c, aliceActor, "late"); !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), "reached goal") {
		t.Fatalf("a new task in a reached goal: %v", err)
	}
	if _, err := db.CreateFeature(c, mandatory(goal), aliceActor, "late-f"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("a new feature in a reached goal: %v", err)
	}
	mover := mustCreateFeature(t, db, other, "mover", "m")
	link := mandatory(goal)
	if _, err := db.SetFeature(mover.ID, &link, false, mover.Revision, aliceActor, "move"); !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), "the goal is reached") {
		t.Fatalf("a move into a reached goal: %v", err)
	}
	// An existing assignment survives: a confirmed task under the goal is
	// still edited, and an edit that leaves the DONE feature DONE passes.
	task, _ := db.GetTask(tasks[0])
	tc := task.TaskContent
	tc.NextAction = "kept as history"
	if _, err := db.UpdateTask(task.ID, tc, task.Revision, aliceActor, "edit-task"); err != nil {
		t.Fatalf("an existing assignment must survive: %v", err)
	}
	f, _ = db.GetTask(f.ID)
	fc := f.TaskContent
	fc.NextAction = "the evidence is on record"
	if _, err := db.UpdateTask(f.ID, fc, f.Revision, aliceActor, "edit-feature"); err != nil {
		t.Fatalf("an edit that leaves a DONE feature DONE must pass: %v", err)
	}
}

// A feature in a reached goal stays DONE or CANCELLED. Each way to break
// that is refused with the way that exists for it.
func TestGoalReachedGuardsItsFeatures(t *testing.T) {
	_, db := taskDB(t)
	goal := goalEpic(t, db, "goal")
	done := mustCreateFeature(t, db, goal, "done feature", "d")
	cancelled := mustCreateFeature(t, db, goal, "cancelled feature", "c")
	plain := content("a task under the goal")
	plain.Epic = goal
	task, err := db.CreateTask(plain, aliceActor, "t")
	if err != nil {
		t.Fatal(err)
	}
	done = finish(t, db, done.ID, "DONE", "d-done")
	cancelled = finish(t, db, cancelled.ID, "CANCELLED", "c-cancel")
	setEpicState(t, db, goal, EpicReached, "reach")

	for _, f := range []Task{done, cancelled} {
		c := f.TaskContent
		c.State = "IN_PROGRESS"
		_, err := db.UpdateTask(f.ID, c, f.Revision, aliceActor, "reopen-"+f.ID)
		if !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), "reopen the goal with update_epic") {
			t.Fatalf("reopening a %s feature in a reached goal: %v", f.State, err)
		}
	}
	link := mandatory(goal)
	_, err = db.SetFeature(task.ID, &link, false, task.Revision, aliceActor, "to-feature")
	if !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), "set the task's epic to an OPEN goal") {
		t.Fatalf("a task turned into a feature of a reached goal: %v", err)
	}
	// A feature already unfinished there (data no guard wrote) is edited
	// only out of the goal: the refusal says to move, finish or cancel it.
	legacy := mustCreateFeature(t, db, goalEpic(t, db, "spare"), "legacy", "l")
	if _, err := db.sql.Exec(`UPDATE tasks SET epic=?, body=json_set(body,'$.epic',?,'$.feature.goal',?) WHERE id=?`, goal, goal, goal, legacy.ID); err != nil {
		t.Fatal(err)
	}
	legacy, _ = db.GetTask(legacy.ID)
	lc := legacy.TaskContent
	lc.NextAction = "touched"
	if _, err := db.UpdateTask(legacy.ID, lc, legacy.Revision, aliceActor, "legacy-edit"); !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), "move this feature to an OPEN goal") {
		t.Fatalf("an unfinished feature already in a reached goal: %v", err)
	}
	open := goalEpic(t, db, "open")
	out := FeatureLink{Goal: open, Position: 1, Mark: "optional"}
	if _, err := db.SetFeature(legacy.ID, &out, false, legacy.Revision, aliceActor, "legacy-move"); err != nil {
		t.Fatalf("moving it out must pass: %v", err)
	}
}

// The check is written, read back, kept by an update that does not give it,
// cleared only by one that gives it empty, and checked as prose.
func TestEpicCheckField(t *testing.T) {
	_, db := taskDB(t)
	e, err := db.CreateEpic(EpicContent{Title: "a goal", Check: "a fresh session delivers one task"}, aliceActor, "create")
	if err != nil || e.Check != "a fresh session delivers one task" {
		t.Fatalf("create: %+v %v", e, err)
	}
	c := e.EpicContent
	c.Title, c.Check = "renamed", "ignored: the parameter decides"
	kept, err := db.UpdateEpic(e.ID, c, nil, e.Revision, aliceActor, "keep")
	if err != nil || kept.Check != e.Check || kept.Title != "renamed" {
		t.Fatalf("an update without a check must keep it: %+v %v", kept, err)
	}
	read, _ := db.GetEpic(e.ID)
	if read.Check != e.Check {
		t.Fatalf("read back: %q", read.Check)
	}
	next := "two sessions deliver one task each"
	set, err := db.UpdateEpic(e.ID, kept.EpicContent, &next, kept.Revision, aliceActor, "set")
	if err != nil || set.Check != next {
		t.Fatalf("set: %+v %v", set, err)
	}
	empty := ""
	cleared, err := db.UpdateEpic(e.ID, set.EpicContent, &empty, set.Revision, aliceActor, "clear")
	if err != nil || cleared.Check != "" {
		t.Fatalf("clear: %+v %v", cleared, err)
	}
	bad := "a check\x00with a NUL"
	if _, err := db.UpdateEpic(e.ID, cleared.EpicContent, &bad, cleared.Revision, aliceActor, "bad"); !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), "check") {
		t.Fatalf("the check is prose-checked: %v", err)
	}
	if _, err := db.CreateEpic(EpicContent{Title: "big", Check: strings.Repeat("x", MaxEpicTextBytes+1)}, aliceActor, "big"); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("the check is bounded: %v", err)
	}
}

// An update without a state keeps the epic's state: a retired or reached
// goal is reopened only by OPEN sent explicitly.
func TestEpicUpdateWithoutStateKeepsIt(t *testing.T) {
	_, db := taskDB(t)
	for _, state := range []string{EpicRetired, EpicReached} {
		goal := goalEpic(t, db, "goal-"+state)
		e := setEpicState(t, db, goal, state, "close-"+state)
		c := e.EpicContent
		c.Title, c.State = "renamed by a client that sends no state", ""
		got, err := db.UpdateEpic(goal, c, nil, e.Revision, aliceActor, "rename-"+state)
		if err != nil || got.State != state || got.Title != c.Title {
			t.Fatalf("%s: an update without a state must keep it: %+v %v", state, got, err)
		}
		c.State = EpicOpen
		if got, err = db.UpdateEpic(goal, c, nil, got.Revision, aliceActor, "reopen-"+state); err != nil || got.State != EpicOpen {
			t.Fatalf("%s: OPEN sent explicitly reopens: %+v %v", state, got, err)
		}
	}
}

// The goal view counts the goal's tasks that no feature of the project
// confirms, and reads the confirmed tasks' states.
func TestGoalViewCountAndStates(t *testing.T) {
	_, db := taskDB(t)
	goal := goalEpic(t, db, "goal")
	other := goalEpic(t, db, "other")
	f, tasks := featureWithTasks(t, db, goal, "f")
	finish(t, db, tasks[0], "DONE", "a-done")

	under := func(title, key string) Task {
		c := content(title)
		c.Epic = goal
		task, err := db.CreateTask(c, aliceActor, key)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	under("the goal's log", "log")
	elsewhere := under("confirmed by a feature of another goal", "else")
	g := mustCreateFeature(t, db, other, "a feature of the other goal", "g")
	if _, err := db.ConfirmFeatureTask(g.ID, elsewhere.ID, false, g.Revision, aliceActor, "g-confirm"); err != nil {
		t.Fatal(err)
	}
	gone := under("archived", "arch")
	finish(t, db, gone.ID, "DONE", "arch-done")
	gone, _ = db.GetTask(gone.ID)
	gc := gone.TaskContent
	gc.Archived = true
	if _, err := db.UpdateTask(gone.ID, gc, gone.Revision, aliceActor, "arch-archive"); err != nil {
		t.Fatal(err)
	}
	// A task confirmed only by an archived feature is not on no feature.
	byArchived := under("confirmed by an archived feature", "by-arch")
	h := mustCreateFeature(t, db, goal, "archived feature", "h")
	h, err := db.ConfirmFeatureTask(h.ID, byArchived.ID, false, h.Revision, aliceActor, "h-confirm")
	if err != nil {
		t.Fatal(err)
	}
	h = finish(t, db, h.ID, "CANCELLED", "h-cancel")
	hc := h.TaskContent
	hc.Archived = true
	if _, err := db.UpdateTask(h.ID, hc, h.Revision, aliceActor, "h-archive"); err != nil {
		t.Fatal(err)
	}

	v, err := db.ReadGoalView(goal, false, 100)
	if err != nil {
		t.Fatal(err)
	}
	if v.Unconfirmed != 1 {
		t.Fatalf("on no feature: the log alone, got %d", v.Unconfirmed)
	}
	if len(v.Features) != 1 || v.Features[0].ID != f.ID {
		t.Fatalf("features without archived: %+v", v.Features)
	}
	if v.Local[tasks[0]] != "DONE" || v.Local[tasks[1]] != "BACKLOG" {
		t.Fatalf("confirmed states: %v", v.Local)
	}
	all, err := db.ReadGoalView(goal, true, 100)
	if err != nil || len(all.Features) != 2 || all.Unconfirmed != 1 {
		t.Fatalf("with archived: %d features, %d unconfirmed, %v", len(all.Features), all.Unconfirmed, err)
	}
	if _, err := db.ReadGoalView(goal, false, 1); err != nil {
		t.Fatal(err)
	}
	if one, _ := db.ReadGoalView(goal, true, 1); !one.Truncated || len(one.Features) != 1 {
		t.Fatalf("truncation: %+v", one)
	}
}

// The goal view reads one snapshot and holds no writer lock: a write from
// another connection, made between its reads, commits at once and changes
// none of the answer.
func TestGoalViewReadsOneSnapshotWithoutWriterLock(t *testing.T) {
	_, db := taskDB(t)
	goal := goalEpic(t, db, "goal")
	f, tasks := featureWithTasks(t, db, goal, "f")

	direct, err := sql.Open("sqlite", "file:"+db.path+"?_txlock=immediate&_pragma=busy_timeout(100)&_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	var wrote error
	var took time.Duration
	goalViewBetweenReads = func() {
		start := time.Now()
		// busy_timeout is 100ms: a writer lock held by the view would
		// fail this write with SQLITE_BUSY instead of letting it commit.
		_, wrote = direct.Exec(`UPDATE tasks SET state='DONE' WHERE id IN (?, ?)`, tasks[0], tasks[1])
		if wrote == nil {
			_, wrote = direct.Exec(`INSERT INTO tasks(id, state, archived, assignee, epic, kind, body)
				SELECT 'zz'||id, state, archived, assignee, epic, kind, json_set(body,'$.id','zz'||id) FROM tasks WHERE id=?`, tasks[0])
		}
		took = time.Since(start)
	}
	defer func() { goalViewBetweenReads = func() {} }()

	v, err := db.ReadGoalView(goal, false, 100)
	if err != nil {
		t.Fatal(err)
	}
	if wrote != nil {
		t.Fatalf("a write from another connection must commit while the view reads (no writer lock): %v", wrote)
	}
	if took > time.Second {
		t.Fatalf("the write waited %v", took)
	}
	if len(v.Features) != 1 || v.Features[0].ID != f.ID || v.Local[tasks[0]] != "BACKLOG" || v.Local[tasks[1]] != "BACKLOG" || v.Unconfirmed != 0 {
		t.Fatalf("the view must read the snapshot from before the write: %+v", v)
	}
	after, err := db.ReadGoalView(goal, false, 100)
	if err != nil || after.Local[tasks[0]] != "DONE" || after.Unconfirmed != 1 {
		t.Fatalf("the next view reads the write: %+v %v", after, err)
	}
}
