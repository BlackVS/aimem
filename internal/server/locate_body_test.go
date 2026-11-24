package server

// Task 01a2073b-962f: locating a task asks only whether a project holds its
// id, so a record that exists but does not decode is located like any other
// and each route reaches its grant check before it reads the record. Before
// the change such a record made the lookup inconclusive, and the task routes
// answered 500 to a caller without a grant where an unknown id gets 404.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/store"
)

// breakBody replaces a task's stored body with JSON that does not decode
// into a task, through a second connection to the project's store.
func breakBody(t *testing.T, reg *store.Registry, taskID string) {
	t.Helper()
	project, _, err := reg.LocateTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(reg.Root(), "projects", project, "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res, err := db.Exec(`UPDATE tasks SET body='[]' WHERE id=?`, taskID); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("broke %d records", n)
	}
	if pdb, err := reg.Open(project); err != nil {
		t.Fatal(err)
	} else if _, err := pdb.GetTask(taskID); err == nil {
		t.Fatal("the record still decodes")
	}
}

// A caller without a grant gets, on every ordinary route, byte for byte the
// answer for an id no project holds, also for a record that does not decode:
// the task routes and the reservation routes among them.
func TestUngrantedAnswersAsUnknownForARecordThatDoesNotDecode(t *testing.T) {
	f := newTaskFixture(t)
	id := walkTask(t, f)
	breakBody(t, f.reg, id)
	walkUngranted(t, f, id)

	// The two named in the task, spelled out: a task read and a reservation
	// claim.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/tasks/{id}", ""},
		{"POST", "/v1/projects/alpha/tasks/{id}/reservation/claim", "{}"},
	} {
		ce, be := walkCall(f, c.method, strings.ReplaceAll(c.path, "{id}", id), f.stranger, "nd-e", c.body, id)
		cu, bu := walkCall(f, c.method, strings.ReplaceAll(c.path, "{id}", walkUnknown), f.stranger, "nd-u", c.body, walkUnknown)
		if ce != cu || be != bu || ce != http.StatusNotFound {
			t.Errorf("%s %s: broken %d %s, unknown %d %s", c.method, c.path, ce, be, cu, bu)
		}
	}
}

// A caller with a grant reaches the read, and the read's failure answers:
// a storage failure on the task route, context_unavailable on a reservation
// route. Neither is a not-found answer, and neither is given before the
// grant check.
func TestGrantedReadOfARecordThatDoesNotDecode(t *testing.T) {
	f := newTaskFixture(t)
	id := walkTask(t, f)
	breakBody(t, f.reg, id)
	if c, b := walkCall(f, "GET", "/v1/tasks/"+id, f.alice, "g-1", "", id); c != http.StatusInternalServerError || !strings.Contains(b, "task storage failure") {
		t.Fatalf("task read with a grant: %d %s", c, b)
	}
	body := `{"expected_revision":1,"holder":{"mode":"standalone","work_ref":"x"}}`
	if c, b := walkCall(f, "POST", "/v1/projects/alpha/tasks/"+id+"/reservation/claim", f.alice, "g-2", body, id); c != http.StatusServiceUnavailable || !strings.Contains(b, `"code":"context_unavailable"`) {
		t.Fatalf("claim with a grant: %d %s", c, b)
	}
}

// The recovery admin's receipts read only the receipts tables, so it answers
// a damaged record's receipts; its status read reads the task and still
// answers task_unavailable.
func TestRecoveryReadsOfARecordThatDoesNotDecode(t *testing.T) {
	g := newRecoveryRig(t)
	breakBody(t, g.s.reg, g.task.ID)
	path := "/v1/admin/reservations/" + g.task.ID + "/recovery/receipts/claim/" + store.RequestKeyDigest("seed-"+g.task.ID)
	r := g.call(t, g.tls, "GET", path, g.env, nil, "", true)
	var receipts struct {
		Receipts []store.RecoveryReceipt `json:"receipts"`
	}
	json.Unmarshal(r.body, &receipts)
	if r.status != http.StatusOK || len(receipts.Receipts) != 1 {
		t.Fatalf("receipts of a damaged record: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "GET", "/v1/admin/reservations/"+g.task.ID+"/recovery", g.env, nil, "", true); r.status != http.StatusNotFound || !strings.Contains(string(r.body), "task_unavailable") {
		t.Fatalf("status of a damaged record: %d %s", r.status, r.body)
	}
}

// The service scope's receipt by key reads only the receipts tables, so it
// answers a damaged record's receipt; its hold status reads the task and
// still refuses as before.
func TestReadScopeReadsOfARecordThatDoesNotDecode(t *testing.T) {
	g := newReadRig(t)
	indep := g.member(t, "independent", "agent-indep", "sess-i")
	task := g.readyTask(t, g.alpha)
	claimed, _ := g.claim(t, indep, task.ID, "attempt-i")
	key := g.lastKey
	breakBody(t, g.s.reg, task.ID)
	got := g.answer(t, byKey(task.ID, "claim", key))
	rc, _ := got["receipt"].(map[string]any)
	if got["state"] == "none" || rc == nil || rc["reservation_id"] != claimed.Reservation.ID {
		t.Fatalf("receipt by key of a damaged record: %v", got)
	}
	if r := g.get(t, holdOf(task.ID)); r.status == http.StatusOK || !strings.Contains(string(r.body), "request_in_progress") {
		t.Fatalf("hold status of a damaged record: %d %s", r.status, r.body)
	}
}
