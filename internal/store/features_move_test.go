package store

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// retireEpic retires an open epic created by goalEpic.
func retireEpic(t *testing.T, db *DB, id, key string) {
	t.Helper()
	e, err := db.GetEpic(id)
	if err != nil {
		t.Fatal(err)
	}
	c := e.EpicContent
	c.State = "RETIRED"
	if _, err := db.UpdateEpic(id, c, e.Revision, aliceActor, key); err != nil {
		t.Fatal(err)
	}
}

// featureWithTasks creates a feature of goal with two confirmed tasks on the
// same epic, and returns the feature and the two tasks' ids.
func featureWithTasks(t *testing.T, db *DB, goal, key string) (Task, []string) {
	t.Helper()
	f := mustCreateFeature(t, db, goal, "the members' hosts exist", key)
	var tasks []string
	for _, n := range []string{"-a", "-b"} {
		c := content("work" + n)
		c.Epic = goal
		task, err := db.CreateTask(c, aliceActor, key+n)
		if err != nil {
			t.Fatal(err)
		}
		f, err = db.ConfirmFeatureTask(f.ID, task.ID, false, f.Revision, aliceActor, key+n+"-confirm")
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task.ID)
	}
	return f, tasks
}

// A feature moves to another goal in one write: the same id, the new epic
// and the new link, its confirmed tasks with their own epics, and one
// history entry that names both goals.
func TestFeatureMovesToAnotherGoal(t *testing.T) {
	_, db := taskDB(t)
	left := goalEpic(t, db, "left")
	joined := goalEpic(t, db, "joined")
	f, tasks := featureWithTasks(t, db, left, "f")

	before := map[string]int64{}
	for _, id := range tasks {
		task, err := db.GetTask(id)
		if err != nil {
			t.Fatal(err)
		}
		before[id] = task.Revision
	}
	link := FeatureLink{Goal: joined, Position: 3, Mark: "optional", Alternative: "done by hand"}
	moved, err := db.SetFeature(f.ID, &link, false, f.Revision, aliceActor, "move")
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetTask(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []Task{moved, got} {
		if r.ID != f.ID || r.Epic != joined || r.Kind != KindFeature || *r.Feature != link || r.Revision != f.Revision+1 {
			t.Fatalf("the moved feature: %+v %+v", r, r.Feature)
		}
		if !slices.Equal(r.Dependencies, tasks) {
			t.Fatalf("the confirmed tasks changed: %v, want %v", r.Dependencies, tasks)
		}
	}
	for _, id := range tasks {
		task, err := db.GetTask(id)
		if err != nil || task.Epic != left || task.Revision != before[id] {
			t.Fatalf("a confirmed task was written: %+v, %v", task, err)
		}
	}
	p, err := db.ListTasks(TaskFilter{Feature: f.ID})
	if err != nil || len(p.Tasks) != 2 {
		t.Fatalf("list_tasks with feature after the move: %+v, %v", p, err)
	}
	for goal, want := range map[string]int{left: 0, joined: 1} {
		p, err := db.ListTasks(TaskFilter{Kind: TaskKindFeature, Epic: goal})
		if err != nil || len(p.Tasks) != want {
			t.Fatalf("features of %s: %+v, %v", goal, p, err)
		}
	}

	h, err := db.TaskHistory(f.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	last := h.Changes[len(h.Changes)-1]
	if last.Task.Revision != moved.Revision || last.MovedFrom != left || last.Task.Epic != joined || last.Task.Feature.Goal != joined {
		t.Fatalf("the move's history entry: %+v", last)
	}
	for _, c := range h.Changes[:len(h.Changes)-1] {
		if c.MovedFrom != "" {
			t.Fatalf("an entry that is not a move names a goal left: %+v", c)
		}
	}

	// A later change of the link within the goal is not a move.
	link.Position = 4
	again, err := db.SetFeature(f.ID, &link, false, moved.Revision, aliceActor, "reorder")
	if err != nil {
		t.Fatal(err)
	}
	h, err = db.TaskHistory(f.ID, moved.Revision, 100)
	if err != nil || len(h.Changes) != 1 || h.Changes[0].Task.Revision != again.Revision || h.Changes[0].MovedFrom != "" {
		t.Fatalf("a reorder recorded a move: %+v, %v", h, err)
	}
}

// A feature leaves a retired goal; a retired epic gains no record.
func TestFeatureMovesOutOfARetiredGoal(t *testing.T) {
	_, db := taskDB(t)
	left := goalEpic(t, db, "left")
	joined := goalEpic(t, db, "joined")
	f, tasks := featureWithTasks(t, db, left, "f")
	retireEpic(t, db, left, "retire")
	link := FeatureLink{Goal: joined, Position: 1, Mark: "mandatory", Ground: "check", GroundNote: "the check's first part"}
	moved, err := db.SetFeature(f.ID, &link, false, f.Revision, aliceActor, "move")
	if err != nil {
		t.Fatal(err)
	}
	if moved.Epic != joined || *moved.Feature != link || !slices.Equal(moved.Dependencies, tasks) {
		t.Fatalf("moved out of a retired goal: %+v %+v", moved, moved.Feature)
	}
	back := FeatureLink{Goal: left, Position: 1, Mark: "optional"}
	if _, err := db.SetFeature(f.ID, &back, false, moved.Revision, aliceActor, "back"); !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), "move the feature to an OPEN goal") {
		t.Fatalf("a move back into the retired goal: %v", err)
	}
}

// Every refusal of a move leaves the record as it was, and its message says
// what to do.
func TestFeatureMoveRefusals(t *testing.T) {
	reg, db := taskDB(t)
	left := goalEpic(t, db, "left")
	joined := goalEpic(t, db, "joined")
	retired := goalEpic(t, db, "retired")
	retireEpic(t, db, retired, "retire")
	other, err := reg.Open("proj-b")
	if err != nil {
		t.Fatal(err)
	}
	foreign := goalEpic(t, other, "foreign")

	unchanged := func(name string, f Task) {
		t.Helper()
		got, err := db.GetTask(f.ID)
		if err != nil || got.Revision != f.Revision || got.Epic != f.Epic || *got.Feature != *f.Feature {
			t.Fatalf("%s: the refused move wrote: %+v, %v", name, got, err)
		}
	}
	f, _ := featureWithTasks(t, db, left, "f")
	for _, tc := range []struct {
		name string
		link FeatureLink
		want string
	}{
		{"another project", FeatureLink{Goal: foreign, Position: 1, Mark: "optional"}, "give an epic id that list_epics returns here"},
		{"a retired goal joined", FeatureLink{Goal: retired, Position: 1, Mark: "optional"}, "move the feature to an OPEN goal"},
		{"a mandatory mark without a ground", FeatureLink{Goal: joined, Position: 1, Mark: "mandatory"}, "needs a ground: check or owner_condition"},
		{"no mark", FeatureLink{Goal: joined, Position: 1}, "mark must be mandatory or optional"},
		{"no position", FeatureLink{Goal: joined, Mark: "optional"}, "position must be between 1 and"},
	} {
		link := tc.link
		if _, err := db.SetFeature(f.ID, &link, false, f.Revision, aliceActor, "move-"+tc.name); !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", tc.name, err)
		}
		unchanged(tc.name, f)
	}

	// A stale revision returns the current record.
	link := FeatureLink{Goal: joined, Position: 1, Mark: "optional"}
	var conflict *TaskConflict
	if _, err := db.SetFeature(f.ID, &link, false, f.Revision-1, aliceActor, "stale"); !errors.As(err, &conflict) || conflict.Current.Revision != f.Revision {
		t.Fatalf("a stale move: %v", err)
	}
	unchanged("stale", f)

	// A reserved record is refused before the move is read. No guard lets a
	// feature be reserved, so the row is written as data no guard wrote.
	if _, err := db.sql.Exec(`INSERT INTO task_reservations(task_id, fence, active_id, holder_mode, holder_ref) VALUES(?,1,'r1','personal','someone')`, f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetFeature(f.ID, &link, false, f.Revision, aliceActor, "reserved"); !errors.Is(err, ErrTaskReserved) {
		t.Fatalf("a move of a reserved record: %v", err)
	}
	unchanged("reserved", f)
	if _, err := db.sql.Exec(`DELETE FROM task_reservations WHERE task_id=?`, f.ID); err != nil {
		t.Fatal(err)
	}

	// A finished feature stays with the goal it reached.
	for _, state := range []string{"DONE", "CANCELLED"} {
		g := mustCreateFeature(t, db, left, "finished "+state, "finished-"+state)
		c := g.TaskContent
		c.State = state
		c.EvidenceRefs = []TaskRef{{Kind: "text", Ref: "the check passed"}}
		g, err := db.UpdateTask(g.ID, c, g.Revision, aliceActor, "finish-"+state)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.SetFeature(g.ID, &link, false, g.Revision, aliceActor, "move-"+state); !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), "stays with") {
			t.Errorf("a move of a %s feature: %v", state, err)
		}
		unchanged(state, g)
	}
}

// A task's full update never changes a feature's epic: it cannot state the
// mark for the new goal, and its refusal names set_feature.
func TestFeatureEpicChangeByUpdateRefused(t *testing.T) {
	_, db := taskDB(t)
	left := goalEpic(t, db, "left")
	joined := goalEpic(t, db, "joined")
	f := mustCreateFeature(t, db, left, "a feature", "f")
	c := f.TaskContent
	c.Epic = joined
	_, err := db.UpdateTask(f.ID, c, f.Revision, aliceActor, "update")
	if !errors.Is(err, ErrTaskInvalid) || !strings.Contains(err.Error(), "changes its goal with set_feature") || strings.Contains(err.Error(), "cannot move") {
		t.Fatalf("an update that changes a feature's epic: %v", err)
	}
	if got, _ := db.GetTask(f.ID); got.Epic != left || got.Revision != f.Revision {
		t.Fatalf("the refused update wrote: %+v", got)
	}
}
