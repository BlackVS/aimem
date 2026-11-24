package server

// Task 01a2073b-962f: locating a task asks only whether a project holds its
// id, so a record that exists but does not decode is located like any other
// and each route reaches its grant check before it reads the record. Before
// the change such a record made the lookup inconclusive, and the task routes
// answered 500 to a caller without a grant where an unknown id gets 404.

import (
	"database/sql"
	"encoding/json"
	"fmt"
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

// The routes that read a record other than the body answer for a damaged
// record to a caller with a grant (task comment 693): a comment, a
// reservation receipt by key, and the replay of a committed claim.
func TestGrantedNonBodyReadsOfARecordThatDoesNotDecode(t *testing.T) {
	f := newTaskFixture(t)
	id := walkTask(t, f)
	w := taskReq(t, f.h, "POST", "/v1/tasks/"+id+"/comments", f.alice, "nb-c", `{"body":"before the damage"}`)
	var comment struct{ ID string }
	if json.Unmarshal(w.Body.Bytes(), &comment); comment.ID == "" {
		t.Fatalf("comment: %d %s", w.Code, w.Body)
	}
	claim := `{"expected_revision":1,"holder":{"mode":"standalone","work_ref":"x"}}`
	claimPath := "/v1/projects/alpha/tasks/" + id + "/reservation/claim"
	if c, b := walkCall(f, "POST", claimPath, f.alice, "nb-claim", claim, id); c != http.StatusOK {
		t.Fatalf("claim: %d %s", c, b)
	}
	breakBody(t, f.reg, id)

	if c, b := walkCall(f, "GET", "/v1/tasks/"+id+"/comments/"+comment.ID, f.alice, "", "", id); c != http.StatusOK || !strings.Contains(b, "before the damage") {
		t.Errorf("the comment: %d %s", c, b)
	}
	if c, b := walkCall(f, "GET", "/v1/projects/alpha/tasks/"+id+"/reservation/receipts/claim/nb-claim", f.alice, "", "", id); c != http.StatusOK || !strings.Contains(b, `"reservation"`) {
		t.Errorf("the receipt by key: %d %s", c, b)
	}
	if c, b := walkCall(f, "POST", claimPath, f.alice, "nb-claim", claim, id); c != http.StatusOK || !strings.Contains(b, `"replayed":true`) {
		t.Errorf("the claim's replay: %d %s", c, b)
	}
	// A new transition reads the body and fails; nothing is written.
	if c, b := walkCall(f, "POST", claimPath, f.alice, "nb-claim-2", claim, id); c != http.StatusServiceUnavailable || !strings.Contains(b, `"code":"context_unavailable"`) {
		t.Errorf("a new claim: %d %s", c, b)
	}
}

// A new write on a damaged record reads the body in its transaction first,
// fails, and writes nothing: the comment count stays as it was.
func TestNewCommentOnARecordThatDoesNotDecodeWritesNothing(t *testing.T) {
	f := newTaskFixture(t)
	id := walkTask(t, f)
	if w := taskReq(t, f.h, "POST", "/v1/tasks/"+id+"/comments", f.alice, "nw-1", `{"body":"one"}`); w.Code != 201 {
		t.Fatalf("comment: %d %s", w.Code, w.Body)
	}
	breakBody(t, f.reg, id)
	if c, b := walkCall(f, "POST", "/v1/tasks/"+id+"/comments", f.alice, "nw-2", `{"body":"two"}`, id); c != http.StatusInternalServerError || !strings.Contains(b, "task storage failure") {
		t.Fatalf("a new comment: %d %s", c, b)
	}
	if n := commentRows(t, f.reg, id); n != 1 {
		t.Fatalf("the refused write left %d comments, want 1", n)
	}
	// The comment list reads the task too, and answers the read's failure.
	if c, b := walkCall(f, "GET", "/v1/tasks/"+id+"/comments", f.alice, "", "", id); c != http.StatusInternalServerError {
		t.Fatalf("the comment list: %d %s", c, b)
	}
}

// commentRows counts a task's stored comments through a second connection.
func commentRows(t *testing.T, reg *store.Registry, taskID string) int {
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
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_comments WHERE task_id=?`, taskID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A feature's confirmation of a task replays from its receipt once the
// confirmed task's record no longer decodes; a new confirmation reads the
// task, fails, and writes nothing.
func TestFeatureConfirmReplayOfARecordThatDoesNotDecode(t *testing.T) {
	g := newFeatureRig(t)
	work := g.task(t, "the work", "READY")
	f1 := g.feature(t, "a feature", 1)
	body := fmt.Sprintf(`{"task":%q,"expected_revision":1}`, work)
	if w := g.confirm(t, g.alice, f1, "fc-1", body); w.Code != 200 {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	breakBody(t, g.reg, work)
	if w := g.confirm(t, g.alice, f1, "fc-1", body); w.Code != 200 || !strings.Contains(w.Body.String(), work) {
		t.Fatalf("the confirmation's replay: %d %s", w.Code, w.Body)
	}
	other := g.feature(t, "another feature", 2)
	if w := g.confirm(t, g.alice, other, "fc-2", body); w.Code != http.StatusInternalServerError {
		t.Fatalf("a new confirmation of the damaged task: %d %s", w.Code, w.Body)
	}
	w := taskReq(t, g.h, "GET", "/v1/tasks/"+other, g.alice, "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("read the other feature back: %d %s", w.Code, w.Body)
	}
	if back := decodeTask(t, w); back.Revision != 1 || len(back.Dependencies) != 0 {
		t.Fatalf("the refused confirmation wrote: revision %d, dependencies %v", back.Revision, back.Dependencies)
	}
}
