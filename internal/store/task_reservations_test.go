package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"aimem/internal/uuidv7"
)

func readyReservationTask(t *testing.T, db *DB, key string) Task {
	t.Helper()
	task, err := db.CreateTask(TaskContent{Title: "reserved work", State: "READY"}, aliceActor, key)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// Older migration fixtures rewind the schema by removing later tables.
// Remove this additive step before replaying it, never make production DDL
// silently tolerate a partially present reservation schema.
func rewindReservationSchema(t *testing.T, db *DB) {
	t.Helper()
	for _, stmt := range []string{
		`DROP TABLE task_reservation_requests`,
		`DROP TABLE task_reservation_events`,
		`DROP TABLE task_reservations`,
	} {
		if _, err := db.sql.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func claimInput(task Task, ref string) TaskReservationInput {
	return TaskReservationInput{TaskID: task.ID, ExpectedRevision: task.Revision,
		Holder: ReservationHolder{Mode: "standalone", Ref: ref}}
}

func TestTaskReservationFenceAndReceiptsSurviveRestart(t *testing.T) {
	root := t.TempDir()
	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	db, err := r.Open("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	task := readyReservationTask(t, db, "create-fence")
	claim := claimInput(task, "run-1")
	first, err := db.ApplyTaskReservation(ReservationClaim, claim, aliceActor, "claim-1")
	if err != nil || first.Reservation.Fence != 1 || first.Reservation.ID == "" {
		t.Fatalf("first claim: %+v, %v", first, err)
	}
	if first.Task.Revision != task.Revision || countRows(t, db, "task_history", "task_id=?", task.ID) != 1 {
		t.Fatal("pure claim changed task revision or history")
	}
	replayed, err := db.ApplyTaskReservation(ReservationClaim, claim, aliceActor, "claim-1")
	if err != nil || replayed.Reservation != first.Reservation {
		t.Fatalf("claim replay: %+v, %v", replayed, err)
	}
	if receipt, found, err := db.GetTaskReservationReceipt(ReservationClaim, claim, aliceActor, "claim-1"); err != nil || !found || receipt.Reservation != first.Reservation {
		t.Fatalf("receipt: %+v, %v, %v", receipt, found, err)
	}
	rotated := aliceActor
	rotated.TokenID = uuidv7.New()
	if receipt, err := db.ApplyTaskReservation(ReservationClaim, claim, rotated, "claim-1"); err != nil || receipt.Reservation != first.Reservation {
		t.Fatalf("stable actor replay after token rotation: %+v, %v", receipt, err)
	}
	changed := claimInput(task, "changed-holder")
	if _, err := db.ApplyTaskReservation(ReservationClaim, changed, aliceActor, "claim-1"); !errors.Is(err, ErrTaskRetryConflict) {
		t.Fatalf("changed input reused key: %v", err)
	}
	release := TaskReservationInput{TaskID: task.ID, ID: first.Reservation.ID, Fence: first.Reservation.Fence,
		ExpectedRevision: task.Revision, Content: &task.TaskContent, Reason: "offer declined"}
	released, err := db.ApplyTaskReservation(ReservationRelease, release, aliceActor, "release-1")
	if err != nil || released.Reservation.ID != "" || released.Reservation.Fence != 2 || released.Task.Revision != 2 {
		t.Fatalf("release: %+v, %v", released, err)
	}
	if countRows(t, db, "task_history", "task_id=?", task.ID) != 2 {
		t.Fatal("release content/history not recorded")
	}
	r.Close()
	r, err = NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	db, err = r.OpenExisting("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	status, err := db.GetTaskReservation(task.ID)
	if err != nil || status.ID != "" || status.Fence != 2 {
		t.Fatalf("released status after restart: %+v, %v", status, err)
	}
	secondClaim := claimInput(released.Task, "run-2")
	second, err := db.ApplyTaskReservation(ReservationClaim, secondClaim, aliceActor, "claim-2")
	if err != nil || second.Reservation.Fence != 3 || second.Reservation.ID == first.Reservation.ID {
		t.Fatalf("reclaim after restart: %+v, %v", second, err)
	}
	if _, err := db.ApplyTaskReservation(ReservationRelease, release, aliceActor, "stale-release"); !errors.Is(err, ErrTaskRetryConflict) && !errors.Is(err, ErrReservationStale) && !isTaskConflict(err) {
		t.Fatalf("old holder accepted: %v", err)
	}
	if receipt, found, err := db.GetTaskReservationReceipt(ReservationRelease, release, aliceActor, "release-1"); err != nil || !found || receipt.Reservation.Fence != 2 {
		t.Fatalf("release receipt after restart: %+v, %v, %v", receipt, found, err)
	}
}

func isTaskConflict(err error) bool {
	var conflict *TaskConflict
	return errors.As(err, &conflict)
}

func TestTaskReservationCompetingClaimsAndTransfer(t *testing.T) {
	_, db := taskDB(t)
	task := readyReservationTask(t, db, "create-compete")
	actors := []TaskActor{aliceActor, {Kind: "user", UserID: uuidv7.New(), TokenID: uuidv7.New(), Name: "bob"}}
	var wg sync.WaitGroup
	results := make([]TaskReservationOutcome, 2)
	errs := make([]error, 2)
	for i := range actors {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = db.ApplyTaskReservation(ReservationClaim, claimInput(task, fmt.Sprintf("run-%d", i)), actors[i], fmt.Sprintf("claim-%d", i))
		}(i)
	}
	wg.Wait()
	winner := -1
	for i, err := range errs {
		if err == nil {
			if winner != -1 {
				t.Fatalf("two winners: %+v", results)
			}
			winner = i
		} else if !errors.Is(err, ErrReservationConflict) {
			t.Fatalf("loser error: %v", err)
		}
	}
	if winner < 0 || countRows(t, db, "task_reservation_events", "task_id=?", task.ID) != 1 {
		t.Fatalf("claim count: winner %d, errors %v", winner, errs)
	}
	first := results[winner]
	transfer := TaskReservationInput{TaskID: task.ID, ID: first.Reservation.ID, Fence: first.Reservation.Fence,
		ExpectedRevision: task.Revision, Holder: ReservationHolder{Mode: "external", Ref: "attempt-1"}}
	moved, err := db.ApplyTaskReservation(ReservationTransfer, transfer, actors[winner], "transfer")
	if err != nil || moved.Reservation.Fence != 2 || moved.Reservation.Holder.Ref != "attempt-1" || moved.Task.Revision != task.Revision {
		t.Fatalf("transfer: %+v, %v", moved, err)
	}
	stale := TaskReservationInput{TaskID: task.ID, ID: first.Reservation.ID, Fence: first.Reservation.Fence,
		ExpectedRevision: task.Revision, Content: &TaskContent{Title: task.Title, State: "IN_PROGRESS"}}
	if _, err := db.ApplyTaskReservation(ReservationUpdate, stale, actors[winner], "old-update"); !errors.Is(err, ErrReservationStale) {
		t.Fatalf("old fence wrote after transfer: %v", err)
	}
	current := stale
	current.Fence = moved.Reservation.Fence
	updated, err := db.ApplyTaskReservation(ReservationUpdate, current, actors[winner], "new-update")
	if err != nil || updated.Task.Revision != task.Revision+1 || updated.Reservation.Fence != 3 {
		t.Fatalf("fenced update: %+v, %v", updated, err)
	}
	final := TaskReservationInput{TaskID: task.ID, ID: moved.Reservation.ID, Fence: updated.Reservation.Fence,
		ExpectedRevision: updated.Task.Revision, Content: &TaskContent{Title: task.Title, State: "DONE"}, Reason: "reviewed delivery"}
	closed, err := db.ApplyTaskReservation(ReservationFinalize, final, actors[winner], "finalize")
	if err != nil || closed.Task.State != "DONE" || closed.Reservation.ID != "" || closed.Reservation.Fence != 4 {
		t.Fatalf("finalize: %+v, %v", closed, err)
	}
	if countRows(t, db, "task_history", "task_id=?", task.ID) != 3 {
		t.Fatal("claim/transfer changed task history or update/finalize missed it")
	}
}

func TestTaskReservationTransitionAndReceiptRollbackTogether(t *testing.T) {
	_, db := taskDB(t)
	task := readyReservationTask(t, db, "create-rollback")
	claim := claimInput(task, "run")
	if _, err := db.sql.Exec(`CREATE TRIGGER reject_reservation_receipt BEFORE INSERT ON task_reservation_requests BEGIN SELECT RAISE(ABORT,'test receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyTaskReservation(ReservationClaim, claim, aliceActor, "retry-after-receipt-failure"); err == nil {
		t.Fatal("claim succeeded despite receipt failure")
	}
	if countRows(t, db, "task_reservations", "task_id=?", task.ID) != 0 || countRows(t, db, "task_reservation_events", "task_id=?", task.ID) != 0 {
		t.Fatal("failed receipt left hold or event")
	}
	if _, err := db.sql.Exec(`DROP TRIGGER reject_reservation_receipt`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`CREATE TRIGGER reject_reservation_event BEFORE INSERT ON task_reservation_events BEGIN SELECT RAISE(ABORT,'test event failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyTaskReservation(ReservationClaim, claim, aliceActor, "retry-after-rollback"); err == nil {
		t.Fatal("claim succeeded despite event failure")
	}
	if countRows(t, db, "task_reservations", "task_id=?", task.ID) != 0 || countRows(t, db, "task_reservation_requests", "operation=? AND task_id=?", "claim", task.ID) != 0 {
		t.Fatal("failed transition left hold or receipt")
	}
	if _, err := db.sql.Exec(`DROP TRIGGER reject_reservation_event`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyTaskReservation(ReservationClaim, claim, aliceActor, "retry-after-rollback"); err != nil {
		t.Fatalf("same key after rollback: %v", err)
	}
	status, err := db.GetTaskReservation(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`CREATE TRIGGER reject_task_history BEFORE INSERT ON task_history BEGIN SELECT RAISE(ABORT,'test history failure'); END`); err != nil {
		t.Fatal(err)
	}
	release := TaskReservationInput{TaskID: task.ID, ID: status.ID, Fence: status.Fence,
		ExpectedRevision: task.Revision, Content: &task.TaskContent, Reason: "stopped"}
	if _, err := db.ApplyTaskReservation(ReservationRelease, release, aliceActor, "retry-release"); err == nil {
		t.Fatal("release succeeded despite task history failure")
	}
	if current, err := db.GetTaskReservation(task.ID); err != nil || current.ID != status.ID || current.Fence != status.Fence {
		t.Fatalf("failed release changed hold: %+v, %v", current, err)
	}
	if countRows(t, db, "task_reservation_requests", "operation=? AND task_id=?", "release", task.ID) != 0 {
		t.Fatal("failed release left receipt")
	}
	if _, err := db.sql.Exec(`DROP TRIGGER reject_task_history`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyTaskReservation(ReservationRelease, release, aliceActor, "retry-release"); err != nil {
		t.Fatalf("release retry after rollback: %v", err)
	}
}

func TestTaskReservationRefusesLegacyManagedTask(t *testing.T) {
	_, db := taskDB(t)
	task := readyReservationTask(t, db, "create-legacy")
	teamID := uuidv7.New()
	if _, err := db.sql.Exec(`INSERT INTO teams(id,name,body) VALUES(?,?,?)`, teamID, "legacy-team", `{}`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.sql.Exec(`INSERT INTO team_managed_tasks(task_id,team_id,managed) VALUES(?,?,1)`, task.ID, teamID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyTaskReservation(ReservationClaim, claimInput(task, "run"), aliceActor, "claim-managed"); !errors.Is(err, ErrReservationConflict) {
		t.Fatalf("legacy managed task claimed: %v", err)
	}
	if countRows(t, db, "task_reservations", "task_id=?", task.ID) != 0 {
		t.Fatal("refused claim created a hold")
	}
}

func TestTaskReservationSchema19PreservesExistingTasks(t *testing.T) {
	root := t.TempDir()
	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	db, err := r.Open("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	task := readyReservationTask(t, db, "create-migrate")
	for _, stmt := range []string{
		`DROP TABLE task_reservation_requests`, `DROP TABLE task_reservation_events`, `DROP TABLE task_reservations`,
		`UPDATE meta SET value='18' WHERE key='schema_version'`,
	} {
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
	db, err = r.OpenExisting("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetTask(task.ID)
	if err != nil || got.Revision != task.Revision || got.Title != task.Title {
		t.Fatalf("existing task changed: %+v, %v", got, err)
	}
	if v, err := db.GetMeta("schema_version"); err != nil || v != "19" {
		t.Fatalf("schema version: %q, %v", v, err)
	}
	if countRows(t, db, "task_requests", "operation=? AND scope=?", "create", "") != 1 {
		t.Fatal("existing task receipt lost during migration")
	}
	if _, err := db.ApplyTaskReservation(ReservationClaim, claimInput(got, "migrated"), aliceActor, "migrated-claim"); err != nil {
		t.Fatalf("cannot claim migrated task: %v", err)
	}
	if _, err := db.sql.Exec(`UPDATE meta SET value='20' WHERE key='schema_version'`); err != nil {
		t.Fatal(err)
	}
	r.Close()
	r, err = NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.OpenExisting("proj-a"); err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("newer schema not refused: %v", err)
	}
}
