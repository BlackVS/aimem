package store

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
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
		{got[0].Sequence, task.ID, 1, "", "BACKLOG", task.UpdatedAt, ""},
		{got[0].Sequence + 1, task.ID, 3, "BACKLOG", "READY", ready.UpdatedAt, ""},
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

// A task's required capability is set, kept in the history, read in the
// feed at the change's revision, and cleared by an update that omits it.
// An invalid value is refused and changes nothing.
func TestTaskRequiredCapability(t *testing.T) {
	_, db := taskDB(t)
	c := content("needs network x")
	c.RequiredCapability = "ops: network-x"
	task, err := db.CreateTask(c, aliceActor, "cap-create")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetTask(task.ID); err != nil || got.RequiredCapability != "ops: network-x" {
		t.Fatalf("read back: %+v %v", got, err)
	}
	for i, bad := range []string{"ops:network-x", "Ops: x", "ops:  x", "ops: x ", "ops: ", "ops", ": x", "1ops: x",
		"ops: a\nb", "ops: a\tb", "ops: x\u00a0", "ops: \u2003x", "ops: token ghp_" + strings.Repeat("A", 36),
		"ops: " + strings.Repeat("x", MaxCapabilityBytes)} {
		key := fmt.Sprint("bad-", i)
		b := content("needs network x")
		b.RequiredCapability = bad
		if _, err := db.UpdateTask(task.ID, b, task.Revision, aliceActor, key); err == nil {
			t.Fatalf("accepted %q", bad)
		}
		if _, err := db.CreateTask(b, aliceActor, "create-"+key); err == nil {
			t.Fatalf("created with %q", bad)
		}
	}
	if got, _ := db.GetTask(task.ID); got.Revision != task.Revision || got.RequiredCapability != "ops: network-x" {
		t.Fatalf("a refused write changed the task: %+v", got)
	}
	// Moved to READY with another capability, then an edit of the
	// capability alone (no feed entry), then cleared on the way to DONE.
	c.State, c.RequiredCapability = "READY", "code: forge github.com"
	ready, err := db.UpdateTask(task.ID, c, task.Revision, aliceActor, "cap-ready")
	if err != nil {
		t.Fatal(err)
	}
	c.RequiredCapability = "ops: network-y"
	narrowed, err := db.UpdateTask(task.ID, c, ready.Revision, aliceActor, "cap-narrow")
	if err != nil {
		t.Fatal(err)
	}
	c.State, c.RequiredCapability = "DONE", ""
	done, err := db.UpdateTask(task.ID, c, narrowed.Revision, aliceActor, "cap-done")
	if err != nil || done.RequiredCapability != "" {
		t.Fatalf("clear: %+v %v", done, err)
	}
	var got []string
	for _, e := range feedOf(t, db) {
		got = append(got, e.To+"="+e.RequiredCapability)
	}
	if want := "BACKLOG=ops: network-x READY=code: forge github.com DONE="; strings.Join(got, " ") != want {
		t.Fatalf("feed: %q, want %q", strings.Join(got, " "), want)
	}
	h, err := db.TaskHistory(task.ID, 0, 10)
	if err != nil || len(h.Changes) != 4 || h.Changes[2].Task.RequiredCapability != "ops: network-y" {
		t.Fatalf("history: %+v %v", h, err)
	}
}
