package store

import (
	"errors"
	"testing"

	"aimem/internal/uuidv7"
)

// bodyUnreadable puts a view in place of the tasks table whose body raises
// an error whenever it is evaluated, so a statement that reads a record's
// body fails and one that asks only for the id succeeds.
func bodyUnreadable(t *testing.T, d *DB) {
	t.Helper()
	for _, q := range []string{
		`ALTER TABLE tasks RENAME TO tasks_real`,
		`CREATE VIEW tasks AS SELECT id, json('not json') AS body FROM tasks_real`,
	} {
		if _, err := d.sql.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// Locating a task reads nothing of its record but the id: on the hint and
// in the scan, a project whose bodies cannot be read at all still answers
// where the task is, while a read of the task fails there.
func TestLocateTaskReadsNoBody(t *testing.T) {
	r := newTestRegistry(t)
	a, _ := r.Open("proj-a")
	if _, err := r.Open("proj-b"); err != nil {
		t.Fatal(err)
	}
	ta := mustCreate(t, a, "in a", "k-a")
	bodyUnreadable(t, a)
	if _, err := a.GetTask(ta.ID); err == nil || errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("the control: a read of the body must fail here, got %v", err)
	}
	if p, db, err := r.LocateTask(ta.ID); err != nil || p != "proj-a" || db != a {
		t.Fatalf("on the hint: %q %v", p, err)
	}
	if n := r.taskScans.Load(); n != 0 {
		t.Fatalf("the hint was not used: %d scans", n)
	}
	r.hints.forget(ta.ID)
	if p, db, err := r.LocateTask(ta.ID); err != nil || p != "proj-a" || db != a {
		t.Fatalf("in the scan: %q %v", p, err)
	}
	if n := r.taskScans.Load(); n != 1 {
		t.Fatalf("the scan did not run: %d scans", n)
	}
	// An id the project does not hold is a conclusive miss there too.
	if _, _, err := r.LocateTask(uuidv7.New()); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing id: %v", err)
	}
}

// A record that exists but does not decode is located like any other, on
// the hint and in the scan; until this change it made the lookup
// inconclusive ("task lookup incomplete").
func TestLocateTaskFindsARecordThatDoesNotDecode(t *testing.T) {
	r := newTestRegistry(t)
	a, _ := r.Open("proj-a")
	if _, err := r.Open("proj-b"); err != nil {
		t.Fatal(err)
	}
	ta := mustCreate(t, a, "in a", "k-a")
	if _, err := a.sql.Exec(`UPDATE tasks SET body='[]' WHERE id=?`, ta.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.GetTask(ta.ID); err == nil || errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("the control: the record must not decode, got %v", err)
	}
	for _, path := range []string{"hint", "scan"} {
		if path == "scan" {
			r.hints.forget(ta.ID)
		}
		if p, _, err := r.LocateTask(ta.ID); err != nil || p != "proj-a" {
			t.Fatalf("%s: %q %v", path, p, err)
		}
	}
}
