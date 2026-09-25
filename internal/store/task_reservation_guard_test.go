package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestReservationGuardsGenericWritesAndReceipts(t *testing.T) {
	_, db := taskDB(t)
	task := readyReservationTask(t, db, "guard-create")
	prior, err := db.UpdateTask(task.ID, task.TaskContent, task.Revision, aliceActor, "prior-update")
	if err != nil {
		t.Fatal(err)
	}
	hold, err := db.ApplyTaskReservation(ReservationClaim, claimInput(prior, "work-1"), aliceActor, "guard-claim")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateTask(task.ID, task.TaskContent, task.Revision, aliceActor, "prior-update"); !errors.Is(err, ErrTaskReserved) {
		t.Fatalf("prior update receipt replayed through new hold: %v", err)
	}
	for i, tc := range []struct {
		name     string
		actor    TaskActor
		content  TaskContent
		expected int64
	}{
		{"user content", aliceActor, TaskContent{Title: "unfenced", State: "READY"}, prior.Revision},
		{"admin terminal archive", adminActor, TaskContent{Title: "unfenced", State: "DONE", Archived: true}, prior.Revision},
		{"stale revision", aliceActor, TaskContent{Title: "unfenced", State: "READY"}, task.Revision},
		{"evidence reference", aliceActor, TaskContent{Title: prior.Title, State: "READY", EvidenceRefs: []TaskRef{{Kind: "text", Ref: "unfenced evidence"}}}, prior.Revision},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := fmt.Sprintf("guard-update-%d", i)
			if _, err := db.UpdateTask(task.ID, tc.content, tc.expected, tc.actor, key); !errors.Is(err, ErrTaskReserved) {
				t.Fatalf("generic update bypassed hold: %v", err)
			}
			if countRows(t, db, "task_requests", "operation='update' AND scope=? AND key=?", task.ID, key) != 0 {
				t.Fatal("refused update consumed a receipt")
			}
		})
	}
	if got, err := db.GetTask(task.ID); err != nil || got.Revision != prior.Revision || got.State != "READY" || len(got.EvidenceRefs) != 0 {
		t.Fatalf("generic writer changed held task: %+v, %v", got, err)
	}
	if countRows(t, db, "task_history", "task_id=?", task.ID) != 2 {
		t.Fatal("refused writes changed task history")
	}
	if _, err := db.AddTaskComment(task.ID, "discussion only", aliceActor, "held-comment"); err != nil {
		t.Fatalf("immutable comment refused: %v", err)
	}
	if got, err := db.GetTask(task.ID); err != nil || got.Revision != prior.Revision {
		t.Fatalf("comment changed task revision: %+v, %v", got, err)
	}
	changed := prior.TaskContent
	changed.State = "IN_PROGRESS"
	updated, err := db.ApplyTaskReservation(ReservationUpdate, TaskReservationInput{TaskID: task.ID, ID: hold.Reservation.ID,
		Fence: hold.Reservation.Fence, ExpectedRevision: prior.Revision, Content: &changed}, aliceActor, "fenced-update")
	if err != nil || updated.Task.State != "IN_PROGRESS" || updated.Task.Revision != prior.Revision+1 {
		t.Fatalf("fenced holder write failed: %+v, %v", updated, err)
	}
	changed.State = "READY"
	released, err := db.ApplyTaskReservation(ReservationRelease, TaskReservationInput{TaskID: task.ID, ID: updated.Reservation.ID,
		Fence: updated.Reservation.Fence, ExpectedRevision: updated.Task.Revision, Content: &changed, Reason: "work stopped"}, aliceActor, "guard-release")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.UpdateTask(task.ID, TaskContent{Title: "ordinary edit", State: "READY"}, released.Task.Revision, aliceActor, "after-release"); err != nil {
		t.Fatalf("unreserved edit refused: %v", err)
	}
}

func TestReservationClaimVersusGenericUpdate(t *testing.T) {
	_, db := taskDB(t)
	for i := 0; i < 20; i++ {
		task := readyReservationTask(t, db, fmt.Sprintf("race-create-%d", i))
		var claim TaskReservationOutcome
		var claimErr, updateErr error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			claim, claimErr = db.ApplyTaskReservation(ReservationClaim, claimInput(task, "racing-work"), aliceActor, fmt.Sprintf("race-claim-%d", i))
		}()
		go func() {
			defer wg.Done()
			_, updateErr = db.UpdateTask(task.ID, TaskContent{Title: "racing edit", State: "READY"}, task.Revision, adminActor, fmt.Sprintf("race-update-%d", i))
		}()
		wg.Wait()
		switch {
		case claimErr == nil && errors.Is(updateErr, ErrTaskReserved):
			if claim.Reservation.ID == "" || countRows(t, db, "task_history", "task_id=?", task.ID) != 1 {
				t.Fatal("claim-first race changed content or history")
			}
		case updateErr == nil && isTaskConflict(claimErr):
			if got, err := db.GetTaskReservation(task.ID); err != nil || got.ID != "" || got.Fence != 0 {
				t.Fatalf("update-first race left a hold: %+v, %v", got, err)
			}
		default:
			t.Fatalf("race outcome claim=%v update=%v", claimErr, updateErr)
		}
	}
}

func TestReservationUpdateBeforeClaimRechecksRevision(t *testing.T) {
	_, db := taskDB(t)
	task := readyReservationTask(t, db, "update-first-create")
	updated, err := db.UpdateTask(task.ID, TaskContent{Title: "new scope", State: "READY"}, task.Revision, adminActor, "update-first")
	if err != nil || updated.Revision != task.Revision+1 {
		t.Fatalf("ordinary update: %+v, %v", updated, err)
	}
	if _, err := db.ApplyTaskReservation(ReservationClaim, claimInput(task, "old-scope"), aliceActor, "claim-stale-scope"); !isTaskConflict(err) {
		t.Fatalf("claim did not recheck revised task: %v", err)
	}
	if got, err := db.GetTaskReservation(task.ID); err != nil || got.ID != "" || got.Fence != 0 {
		t.Fatalf("stale claim left a hold: %+v, %v", got, err)
	}
}

func TestReservationBlocksLegacyOfferTakeover(t *testing.T) {
	_, db, team, a, _, offer := assignmentFixture(t)
	task, err := db.GetTask(offer.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := db.ApplyTaskReservation(ReservationClaim, claimInput(task, "standalone-work"), a.Actor, "legacy-race-claim")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.OfferTeamAssignment(team.ID, offer, a, "legacy-offer", allowAssignmentWorker); !errors.Is(err, ErrTaskReserved) {
		t.Fatalf("legacy offer took a reserved task: %v", err)
	}
	if countRows(t, db, "team_managed_tasks", "task_id=?", task.ID) != 0 || countRows(t, db, "team_assignments", "task_id=?", task.ID) != 0 {
		t.Fatal("refused offer changed legacy ownership")
	}
	released, err := db.ApplyTaskReservation(ReservationRelease, TaskReservationInput{TaskID: task.ID, ID: hold.Reservation.ID,
		Fence: hold.Reservation.Fence, ExpectedRevision: task.Revision, Content: &task.TaskContent, Reason: "standalone stopped"}, a.Actor, "legacy-race-release")
	if err != nil {
		t.Fatal(err)
	}
	offer.ExpectedRevision = released.Task.Revision
	if _, err := db.OfferTeamAssignment(team.ID, offer, a, "legacy-offer", allowAssignmentWorker); err != nil {
		t.Fatalf("legacy offer after release: %v", err)
	}
	if _, err := db.ApplyTaskReservation(ReservationClaim, claimInput(released.Task, "new-work"), a.Actor, "claim-managed"); !errors.Is(err, ErrReservationConflict) {
		t.Fatalf("new ledger claimed legacy managed task: %v", err)
	}
}
