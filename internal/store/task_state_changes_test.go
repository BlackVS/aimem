package store

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
)

func feedOf(t *testing.T, db *DB) []TaskStateChange {
	t.Helper()
	got, _, err := db.TaskStateChanges(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A creation is a change from no state; an edit that keeps the state adds
// nothing; each state change is one entry with its revision and time.
func TestTaskStateChangesRecordsStateChangesOnly(t *testing.T) {
	_, db := taskDB(t)
	task := mustCreate(t, db, "one", "c1")
	c := content("one, edited")
	edited, err := db.UpdateTask(task.ID, c, task.Revision, aliceActor, "u1")
	if err != nil {
		t.Fatal(err)
	}
	c.State = "READY"
	ready, err := db.UpdateTask(task.ID, c, edited.Revision, aliceActor, "u2")
	if err != nil {
		t.Fatal(err)
	}
	got := feedOf(t, db)
	want := []TaskStateChange{
		{got[0].Sequence, task.ID, 1, "", "BACKLOG", task.UpdatedAt},
		{got[0].Sequence + 1, task.ID, 3, "BACKLOG", "READY", ready.UpdatedAt},
	}
	if len(got) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("feed:\n got %+v\nwant %+v", got, want)
	}
}

// Pages resume exactly after the given sequence, the last sequence is
// reported with every page, and a cursor at the end returns nothing.
func TestTaskStateChangesPages(t *testing.T) {
	_, db := taskDB(t)
	for i := 0; i < 5; i++ {
		mustCreate(t, db, fmt.Sprint("t", i), fmt.Sprint("c", i))
	}
	all := feedOf(t, db)
	var seen []TaskStateChange
	after := int64(0)
	for {
		page, last, err := db.TaskStateChanges(after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if last != all[4].Sequence {
			t.Fatalf("last %d, want %d", last, all[4].Sequence)
		}
		if len(page) == 0 {
			break
		}
		again, _, _ := db.TaskStateChanges(after, 2)
		if !reflect.DeepEqual(page, again) {
			t.Fatalf("the same cursor gave %+v, then %+v", page, again)
		}
		seen = append(seen, page...)
		after = page[len(page)-1].Sequence
	}
	if !reflect.DeepEqual(seen, all) {
		t.Fatalf("paged %+v, want %+v", seen, all)
	}
	if _, _, err := db.TaskStateChanges(-1, 2); err == nil {
		t.Fatal("negative cursor accepted")
	}
	if _, _, err := db.TaskStateChanges(0, 0); err == nil {
		t.Fatal("zero limit accepted")
	}
}

// Schema 24 backfills the feed from the history: the same entries the
// live writes recorded, in time order.
func TestTaskStateChangesBackfill(t *testing.T) {
	root := t.TempDir()
	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	db, err := r.Open("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	for i, states := range [][]string{{"READY", "READY", "IN_PROGRESS", "DONE"}, {"BACKLOG", "BLOCKED"}, {}} {
		task := mustCreate(t, db, fmt.Sprint("t", i), fmt.Sprint("c", i))
		rev := task.Revision
		for j, s := range states {
			c := content(fmt.Sprint("t", i, " r", j))
			c.State = s
			u, err := db.UpdateTask(task.ID, c, rev, aliceActor, fmt.Sprint("u", i, j))
			if err != nil {
				t.Fatal(err)
			}
			rev = u.Revision
		}
	}
	live := feedOf(t, db)
	if len(live) != 7 {
		t.Fatalf("live feed has %d entries, want 7: %+v", len(live), live)
	}
	for _, stmt := range []string{`DROP TABLE task_state_changes`, `UPDATE meta SET value='23' WHERE key='schema_version'`} {
		if _, err := db.sql.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	r.Close()
	r, err = NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if db, err = r.OpenExisting("proj-a"); err != nil {
		t.Fatal(err)
	}
	back := feedOf(t, db)
	for i := range back {
		if i > 0 && back[i].At < back[i-1].At {
			t.Fatalf("backfill out of time order: %+v", back)
		}
		back[i].Sequence = 0
	}
	for i := range live {
		live[i].Sequence = 0
	}
	key := func(s []TaskStateChange) func(i, j int) bool {
		return func(i, j int) bool {
			return s[i].TaskID+fmt.Sprint(s[i].Revision) < s[j].TaskID+fmt.Sprint(s[j].Revision)
		}
	}
	sort.Slice(live, key(live))
	sort.Slice(back, key(back))
	if !reflect.DeepEqual(back, live) {
		t.Fatalf("backfill:\n got %+v\nwant %+v", back, live)
	}
}
