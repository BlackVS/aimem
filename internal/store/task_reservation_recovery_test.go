package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

var recoveryAdmin = TaskActor{Kind: "recovery", Name: "admin-recovery"}

const attestationText = "worker machine decommissioned; the attempt is abandoned"

func attestation(ref string) RecoveryEvidence {
	return RecoveryEvidence{Kind: "attestation", Ref: ref, Statement: attestationText}
}

func teamHold(t *testing.T, db *DB, key string, service string) (Task, TaskReservationOutcome) {
	t.Helper()
	task := readyReservationTask(t, db, "create-"+key)
	b := teamBinding(aliceActor.UserID, "profile-1", "worker")
	b.ServiceID = service
	in := claimInput(task, "attempt-"+key)
	in.Holder.Mode = "external"
	held, err := db.ApplyTaskReservation(ReservationClaim, in, aliceActor, b, key, nil, allowReservation)
	if err != nil {
		t.Fatal(err)
	}
	return task, held
}

func closing(task Task, held TaskReservationOutcome, state string) TaskReservationInput {
	return TaskReservationInput{TaskID: task.ID, ID: held.Reservation.ID, Fence: held.Reservation.Fence,
		ExpectedRevision: held.Task.Revision, Content: &TaskContent{Title: task.Title, State: state}, Reason: "holder is gone"}
}

func TestReservationRecoveryReleaseAndCancel(t *testing.T) {
	r, db := taskDB(t)
	ctx := context.Background()
	task, held := teamHold(t, db, "rec-1", "aicrew-example")
	out, err := r.RecoverTaskReservation(ctx, RecoveryRelease, closing(task, held, "READY"), attestation("ticket-1"), recoveryAdmin, "rec-key-1", allowReservation)
	if err != nil {
		t.Fatal(err)
	}
	if out.Task.State != "READY" || out.Reservation.ID != "" || out.Reservation.Fence != held.Reservation.Fence+1 {
		t.Fatalf("recovery release: %+v", out)
	}
	rec := out.Recovery
	if rec == nil || rec.Operation != RecoveryRelease || rec.EvidenceKind != "attestation" || rec.EvidenceRef != "ticket-1" ||
		rec.Affected != held.Reservation.Binding || out.Binding != (ReservationBinding{UserID: "admin-recovery", Mode: "recovery"}) {
		t.Fatalf("recovery record: %+v %+v", rec, out.Binding)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), attestationText) {
		t.Fatal("the receipt carries the attestation statement")
	}
	// The event log keeps the statement; the receipt reader finds the
	// recovery principal's receipt by key digest.
	var body string
	if err := db.sql.QueryRow(`SELECT body FROM task_reservation_events WHERE task_id=? AND operation='recovery_release'`, task.ID).Scan(&body); err != nil ||
		!strings.Contains(body, attestationText) || !strings.Contains(body, `"affected"`) {
		t.Fatalf("recovery event: %v %s", err, body)
	}
	got, err := db.RecoveryReceipts(task.ID, ReservationOperation(RecoveryRelease), RequestKeyDigest("rec-key-1"))
	if err != nil || len(got) != 1 || got[0].Principal != "recovery/admin-recovery" || got[0].Outcome.Recovery == nil {
		t.Fatalf("recovery receipts: %+v %v", got, err)
	}
	if got, err := db.RecoveryReceipts(task.ID, ReservationClaim, RequestKeyDigest("rec-1")); err != nil || len(got) != 1 || got[0].Principal != "user/"+aliceActor.UserID {
		t.Fatalf("the reader sees any actor's receipt: %+v %v", got, err)
	}
	// Replay is served to the same principal; changed evidence under the key conflicts.
	if again, err := r.RecoverTaskReservation(ctx, RecoveryRelease, closing(task, held, "READY"), attestation("ticket-1"), recoveryAdmin, "rec-key-1", allowReservation); err != nil || again.Reservation.Fence != out.Reservation.Fence {
		t.Fatalf("replay: %+v %v", again, err)
	}
	if _, err := r.RecoverTaskReservation(ctx, RecoveryRelease, closing(task, held, "READY"), attestation("ticket-2"), recoveryAdmin, "rec-key-1", allowReservation); !errors.Is(err, ErrTaskRetryConflict) {
		t.Fatalf("changed evidence under the key: %v", err)
	}
	// Cancel finalizes CANCELLED; DONE is refused whichever operation asks.
	task2, held2 := teamHold(t, db, "rec-2", "aicrew-example")
	if _, err := r.RecoverTaskReservation(ctx, RecoveryCancel, closing(task2, held2, "DONE"), attestation("t"), recoveryAdmin, "done-1", allowReservation); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("recovery produced DONE: %v", err)
	}
	if _, err := r.RecoverTaskReservation(ctx, RecoveryRelease, closing(task2, held2, "DONE"), attestation("t"), recoveryAdmin, "done-2", allowReservation); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("recovery release to DONE: %v", err)
	}
	if out, err := r.RecoverTaskReservation(ctx, RecoveryCancel, closing(task2, held2, "CANCELLED"), attestation("t-3"), recoveryAdmin, "cancel-1", allowReservation); err != nil || out.Task.State != "CANCELLED" {
		t.Fatalf("recovery cancel: %+v %v", out, err)
	}
	// Only the recovery kind, with valid evidence, against the current fence.
	task3, held3 := teamHold(t, db, "rec-3", "aicrew-example")
	admin := TaskActor{Kind: "admin", Name: "admin-recovery"}
	if _, err := r.RecoverTaskReservation(ctx, RecoveryRelease, closing(task3, held3, "READY"), attestation("t"), admin, "k", allowReservation); err == nil {
		t.Fatal("an admin actor recovered without the recovery kind")
	}
	for name, ev := range map[string]RecoveryEvidence{
		"none":              {},
		"short statement":   {Kind: "attestation", Ref: "t", Statement: "too short"},
		"stop without p1":   {Kind: "stop_evidence", Ref: "acp1_raw-proof"},
		"stop with a quote": {Kind: "stop_evidence", Ref: "p1_" + strings.Repeat("A", 43), Statement: attestationText},
		"unknown kind":      {Kind: "vibes", Ref: "x"},
	} {
		if _, err := r.RecoverTaskReservation(ctx, RecoveryRelease, closing(task3, held3, "READY"), ev, recoveryAdmin, "ev-"+name, allowReservation); !errors.Is(err, ErrRecoveryEvidence) {
			t.Fatalf("evidence %s: %v", name, err)
		}
	}
	stale := closing(task3, held3, "READY")
	stale.Fence++
	if _, err := r.RecoverTaskReservation(ctx, RecoveryRelease, stale, attestation("t"), recoveryAdmin, "stale", allowReservation); !errors.Is(err, ErrReservationStale) {
		t.Fatalf("stale fence: %v", err)
	}
	refused := errors.New("admin revoked")
	if _, err := r.RecoverTaskReservation(ctx, RecoveryRelease, closing(task3, held3, "READY"), attestation("t"), recoveryAdmin, "refused", func() error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("pre-commit refusal: %v", err)
	}
	if got, _ := db.GetTaskReservation(task3.ID); got.ID != held3.Reservation.ID {
		t.Fatal("a refused recovery changed the hold")
	}
	// A recovery binding never acts through the member path: not even a
	// claim of a free task.
	free := readyReservationTask(t, db, "free")
	if _, err := db.ApplyTaskReservation(ReservationClaim, claimInput(free, "grab"), recoveryAdmin,
		ReservationBinding{UserID: "admin-recovery", Mode: "recovery"}, "member-path", nil, allowReservation); err == nil {
		t.Fatal("a recovery binding claimed through the member path")
	}
}

func TestReservationRecoveryUnboundHoldNeedsAttestation(t *testing.T) {
	r, db := taskDB(t)
	ctx := context.Background()
	task, held := teamHold(t, db, "unbound", "aicrew-example")
	if _, err := db.sql.Exec(`UPDATE task_reservations SET bound_user='',bound_mode='',bound_service='',bound_profile='',bound_team='',bound_role='',bound_session='',bound_generation='' WHERE task_id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	stop := RecoveryEvidence{Kind: "stop_evidence", Ref: "p1_" + strings.Repeat("A", 43), HolderRef: held.Reservation.Holder.Ref}
	if _, err := r.RecoverTaskReservation(ctx, RecoveryRelease, closing(task, held, "READY"), stop, recoveryAdmin, "stop", allowReservation); !errors.Is(err, ErrRecoveryEvidence) {
		t.Fatalf("an unbound hold recovered on stop evidence: %v", err)
	}
	out, err := r.RecoverTaskReservation(ctx, RecoveryRelease, closing(task, held, "BLOCKED"), attestation("legacy"), recoveryAdmin, "attest", allowReservation)
	if err != nil || out.Recovery.Affected != (ReservationBinding{}) || out.Task.State != "BLOCKED" {
		t.Fatalf("unbound recovery: %+v %v", out, err)
	}
}

// A holder transition and a recovery on the same fence: exactly one commits.
func TestReservationRecoveryRacesTheHolder(t *testing.T) {
	r, db := taskDB(t)
	task, held := teamHold(t, db, "race", "aicrew-example")
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		up := closing(task, held, "IN_PROGRESS")
		up.Reason = ""
		_, errs[0] = r.ApplyTaskReservation(context.Background(), ReservationUpdate, up, aliceActor, held.Reservation.Binding, "holder", nil, allowReservation)
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = r.RecoverTaskReservation(context.Background(), RecoveryRelease, closing(task, held, "READY"), attestation("t"), recoveryAdmin, "recover", allowReservation)
	}()
	wg.Wait()
	won := 0
	for _, err := range errs {
		var conflict *TaskConflict
		switch {
		case err == nil:
			won++
		case errors.Is(err, ErrReservationStale), errors.As(err, &conflict):
		default:
			t.Fatalf("race: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d transitions won: %v", won, errs)
	}
}

// Closure evidence per service: held, then closed however it closed, and
// still closed after someone else takes the task.
func TestServiceHoldStatusClosureEvidence(t *testing.T) {
	r, db := taskDB(t)
	ctx := context.Background()
	for _, c := range []struct {
		key, want string
		close     func(Task, TaskReservationOutcome) error
	}{
		{"hr", "holder_release", func(task Task, h TaskReservationOutcome) error {
			_, err := db.ApplyTaskReservation(ReservationRelease, closing(task, h, "READY"), aliceActor, h.Reservation.Binding, "rel-hr", nil, allowReservation)
			return err
		}},
		{"hf", "holder_finalize", func(task Task, h TaskReservationOutcome) error {
			in := closing(task, h, "DONE")
			in.TerminalEvidence = []string{"review-head-example", "human-merge-example"}
			_, err := db.ApplyTaskReservation(ReservationFinalize, in, aliceActor, h.Reservation.Binding, "fin-hf", nil, allowReservation)
			return err
		}},
		{"rr", "recovery_release", func(task Task, h TaskReservationOutcome) error {
			_, err := r.RecoverTaskReservation(ctx, RecoveryRelease, closing(task, h, "READY"), attestation("t"), recoveryAdmin, "rr", allowReservation)
			return err
		}},
		{"rc", "recovery_cancel", func(task Task, h TaskReservationOutcome) error {
			_, err := r.RecoverTaskReservation(ctx, RecoveryCancel, closing(task, h, "CANCELLED"), attestation("t"), recoveryAdmin, "rc", allowReservation)
			return err
		}},
	} {
		task, held := teamHold(t, db, "svc-"+c.key, "aicrew-example")
		if got, err := db.ServiceHoldStatus(task.ID, "aicrew-example"); err != nil || got.State != "held" || got.ReservationID != held.Reservation.ID ||
			got.OwnWorkRef != "attempt-svc-"+c.key || got.HolderMode != "external" {
			t.Fatalf("%s held: %+v %v", c.key, got, err)
		}
		if got, _ := db.ServiceHoldStatus(task.ID, "aicrew-other"); got.State != "none" {
			t.Fatalf("%s: another service sees %+v", c.key, got)
		}
		if err := c.close(task, held); err != nil {
			t.Fatal(err)
		}
		got, err := db.ServiceHoldStatus(task.ID, "aicrew-example")
		if err != nil || got.State != "closed" || got.ReservationID != held.Reservation.ID || got.ClosedBy != c.want ||
			got.ClosingFence <= held.Reservation.Fence || got.ClosedAt == "" || got.TaskRevision != held.Task.Revision+1 {
			t.Fatalf("%s closed: %+v %v", c.key, got, err)
		}
	}
	// Someone else takes the task: the service still sees its own closure.
	task, held := teamHold(t, db, "taken", "aicrew-example")
	if _, err := db.ApplyTaskReservation(ReservationRelease, closing(task, held, "READY"), aliceActor, held.Reservation.Binding, "rel-taken", nil, allowReservation); err != nil {
		t.Fatal(err)
	}
	current, _ := db.GetTask(task.ID)
	bob := TaskActor{Kind: "user", UserID: "01a0e62c-0000-7000-8000-00000000b0b0", TokenID: "01a0e62c-0000-7000-8000-00000000b0b1", Name: "bob"}
	personal, err := db.ApplyTaskReservation(ReservationClaim, claimInput(current, "bob-work"), bob, testBinding(bob), "bob-claim", nil, allowReservation)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.ServiceHoldStatus(task.ID, "aicrew-example")
	if err != nil || got.State != "closed" || got.ReservationID != held.Reservation.ID || got.OwnWorkRef != "" || got.HolderMode != "" {
		t.Fatalf("closed after another holder took the task: %+v %v", got, err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "bob") || strings.Contains(string(raw), personal.Reservation.ID) {
		t.Fatalf("the closed answer describes the new holder: %s", raw)
	}
	// A newer reservation by the service replaces the closed one.
	rel, _ := db.ApplyTaskReservation(ReservationRelease, closing(current, personal, "READY"), bob, testBinding(bob), "bob-rel", nil, allowReservation)
	b := teamBinding(aliceActor.UserID, "profile-1", "worker")
	again := claimInput(rel.Task, "attempt-again")
	again.Holder.Mode = "external"
	newer, err := db.ApplyTaskReservation(ReservationClaim, again, aliceActor, b, "claim-again", nil, allowReservation)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ServiceHoldStatus(task.ID, "aicrew-example"); got.State != "held" || got.ReservationID != newer.Reservation.ID {
		t.Fatalf("newer reservation: %+v", got)
	}
	// A personal hold is never tracked for any service.
	ptask := readyReservationTask(t, db, "personal-only")
	if _, err := db.ApplyTaskReservation(ReservationClaim, claimInput(ptask, "mine"), aliceActor, testBinding(aliceActor), "p", nil, allowReservation); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ServiceHoldStatus(ptask.ID, "aicrew-example"); got.State != "none" {
		t.Fatalf("a personal hold reads as %+v", got)
	}
}
