package store

import (
	"errors"
	"testing"

	"aimem/internal/uuidv7"
)

func teamBinding(user, profile, role string) ReservationBinding {
	return ReservationBinding{UserID: user, Mode: "team", ServiceID: "aicrew-example", ProfileID: profile, TeamID: "team-1", Role: role,
		SessionID: "sess-1", Generation: "4"}
}

// The hold is bound to the verified holder: only the same user, mode,
// profile and role act on it; a moved-on session or generation still does.
func TestTaskReservationBindsTheHolder(t *testing.T) {
	_, db := taskDB(t)
	task := readyReservationTask(t, db, "create-bind")
	alice := teamBinding(aliceActor.UserID, "profile-1", "worker")
	held, err := db.ApplyTaskReservation(ReservationClaim, claimInput(task, "run"), aliceActor, alice, "bind-claim", nil, allowReservation)
	if err != nil || held.Reservation.Binding != alice || held.Binding != alice {
		t.Fatalf("claim: %+v, %v", held, err)
	}
	if got, err := db.GetTaskReservation(task.ID); err != nil || got.Binding != alice {
		t.Fatalf("stored binding: %+v, %v", got, err)
	}
	update := TaskReservationInput{TaskID: task.ID, ID: held.Reservation.ID, Fence: held.Reservation.Fence,
		ExpectedRevision: task.Revision, Content: &TaskContent{Title: task.Title, State: "IN_PROGRESS"}}
	bob := TaskActor{Kind: "user", UserID: uuidv7.New(), TokenID: uuidv7.New(), Name: "bob"}
	others := map[string]ReservationBinding{
		"another user":    teamBinding(bob.UserID, "profile-1", "worker"),
		"personal mode":   {UserID: aliceActor.UserID, Mode: "personal"},
		"another profile": teamBinding(aliceActor.UserID, "profile-2", "worker"),
		"another role":    teamBinding(aliceActor.UserID, "profile-1", "coordinator"),
	}
	for name, b := range others {
		if _, err := db.ApplyTaskReservation(ReservationUpdate, update, aliceActor, b, "update-"+name, nil, allowReservation); !errors.Is(err, ErrReservationHolder) {
			t.Fatalf("%s acted on the hold: %v", name, err)
		}
	}
	resumed := alice
	resumed.SessionID, resumed.Generation = "sess-2", "5"
	out, err := db.ApplyTaskReservation(ReservationUpdate, update, aliceActor, resumed, "update-resumed", nil, allowReservation)
	if err != nil || out.Reservation.Binding != alice {
		t.Fatalf("the holder's resumed session: %+v, %v", out, err)
	}
	// Replay and receipt lookup belong to the binding that made the transition.
	if _, err := db.ApplyTaskReservation(ReservationUpdate, update, aliceActor, others["another profile"], "update-resumed", nil, allowReservation); !errors.Is(err, ErrReservationHolder) {
		t.Fatalf("a replay was served to another binding: %v", err)
	}
	if _, _, err := db.GetTaskReservationReceipt(ReservationUpdate, update, aliceActor, others["personal mode"], "update-resumed"); !errors.Is(err, ErrReservationHolder) {
		t.Fatalf("a receipt was served to another binding: %v", err)
	}
	if got, found, err := db.GetTaskReservationReceipt(ReservationUpdate, update, aliceActor, resumed, "update-resumed"); err != nil || !found || got.Binding != resumed {
		t.Fatalf("own receipt: %+v %v %v", got, found, err)
	}
	// Release clears the binding with the hold.
	release := update
	release.Fence, release.ExpectedRevision = out.Reservation.Fence, out.Task.Revision
	release.Content, release.Reason = &TaskContent{Title: task.Title, State: "READY"}, "done here"
	closed, err := db.ApplyTaskReservation(ReservationRelease, release, aliceActor, alice, "release", nil, allowReservation)
	if err != nil || closed.Reservation.Binding != (ReservationBinding{}) || closed.Binding != alice {
		t.Fatalf("release: %+v, %v", closed, err)
	}
}

// The authorizer's check is required, and a refusal from it rolls the whole
// transition back, on a fresh transition and on a replay.
func TestTaskReservationAuthorizeIsRequiredAndFinal(t *testing.T) {
	_, db := taskDB(t)
	task := readyReservationTask(t, db, "create-authz")
	b := testBinding(aliceActor)
	if _, err := db.ApplyTaskReservation(ReservationClaim, claimInput(task, "run"), aliceActor, b, "nil-authz", nil, nil); err == nil {
		t.Fatal("a transition ran without an authorization check")
	}
	if _, err := db.ApplyTaskReservation(ReservationClaim, claimInput(task, "run"), aliceActor, ReservationBinding{UserID: aliceActor.UserID}, "no-mode", nil, allowReservation); err == nil {
		t.Fatal("an incomplete binding was accepted")
	}
	revoked := errors.New("revoked meanwhile")
	if _, err := db.ApplyTaskReservation(ReservationClaim, claimInput(task, "run"), aliceActor, b, "refused", nil, func() error { return revoked }); !errors.Is(err, revoked) {
		t.Fatalf("pre-commit refusal: %v", err)
	}
	if got, err := db.GetTaskReservation(task.ID); err != nil || got.ID != "" || got.Fence != 0 {
		t.Fatalf("a refused claim left state: %+v, %v", got, err)
	}
	if _, err := db.ApplyTaskReservation(ReservationClaim, claimInput(task, "run"), aliceActor, b, "ok", nil, allowReservation); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyTaskReservation(ReservationClaim, claimInput(task, "run"), aliceActor, b, "ok", nil, func() error { return revoked }); !errors.Is(err, revoked) {
		t.Fatalf("a replay skipped the current check: %v", err)
	}
}
