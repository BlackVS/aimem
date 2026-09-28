package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"aimem/internal/uuidv7"
)

var (
	coordPin   = ProcessPin{Repo: "https://git.example/team/process.git", Commit: strings.Repeat("3f", 20), Manifest: "process/manifest.json"}
	coordProof = "p1_" + strings.Repeat("B", 43)
	coordAgent = ReservationWorker{UserID: "user-worker", AgentID: "agent-worker"}
)

// selectProcess stores a project selection the way the hub does: the full
// reference with its fetch hint and metadata.
func selectProcess(t *testing.T, db *DB, pin ProcessPin) {
	t.Helper()
	b, err := json.Marshal(map[string]string{"repo": pin.Repo, "ref": "main", "commit": pin.Commit, "manifest": pin.Manifest,
		"selected_at": "2026-09-28T06:00:00Z", "selected_by": "user-lead"})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta("process", string(b)); err != nil {
		t.Fatal(err)
	}
}

// coordDB is a project a coordinated claim can reach: tasks enabled and an
// access identity, as the hub's projects have.
func coordDB(t *testing.T) (*Registry, *DB) {
	t.Helper()
	r, db := taskDB(t)
	if err := db.SetMeta(TasksMetaKey, "on"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProjectAccessID(db.projectID); err != nil {
		t.Fatal(err)
	}
	return r, db
}

func coordinator(user, session string) ReservationBinding {
	b := teamBinding(user, "profile-1", "coordinator")
	b.SessionID = session
	return b
}

func offerCoordination() *ReservationCoordination {
	w, pin := coordAgent, coordPin
	return &ReservationCoordination{CoordinationRecord: CoordinationRecord{Kind: "offer", ProofDigest: coordProof}, IntendedWorker: &w, Process: &pin}
}

func acceptCoordination(offerRef string, worker ReservationWorker) *ReservationCoordination {
	pin := coordPin
	return &ReservationCoordination{CoordinationRecord: CoordinationRecord{Kind: "accepted_attempt", ProofDigest: coordProof},
		WorkRef: offerRef, Worker: &worker, Process: &pin}
}

func offerClaim(t *testing.T, r *Registry, db *DB, key string) (Task, TaskReservationOutcome) {
	t.Helper()
	task := readyReservationTask(t, db, "create-"+key)
	in := TaskReservationInput{TaskID: task.ID, ExpectedRevision: task.Revision, Holder: ReservationHolder{Mode: "external", Ref: "offer-" + key}}
	out, err := r.ClaimTaskReservation(context.Background(), in, aliceActor, coordinator(aliceActor.UserID, "sess-coord"), key,
		offerCoordination(), func(context.Context, string, string) error { return nil })
	if err != nil {
		t.Fatalf("offer claim: %v", err)
	}
	return task, out
}

func transferInput(task Task, held TaskReservationOutcome, attempt string) TaskReservationInput {
	return TaskReservationInput{TaskID: task.ID, ID: held.Reservation.ID, Fence: held.Reservation.Fence,
		ExpectedRevision: held.Task.Revision, Holder: ReservationHolder{Mode: "external", Ref: attempt}}
}

// The offer records its intended worker, and only that worker (user and
// agent) of the hold's service and team takes it over on its own attempt.
func TestCoordinatedOfferTransferAndFinalize(t *testing.T) {
	r, db := coordDB(t)
	selectProcess(t, db, coordPin)
	task, offer := offerClaim(t, r, db, "o1")
	if offer.Reservation.IntendedWorker == nil || *offer.Reservation.IntendedWorker != coordAgent ||
		offer.Coordination == nil || offer.Coordination.Kind != "offer" || offer.Coordination.ProofDigest != coordProof {
		t.Fatalf("offer hold: %+v", offer)
	}
	worker := teamBinding(coordAgent.UserID, "profile-1", "worker")
	in := transferInput(task, offer, "attempt-o1")
	for name, c := range map[string]struct {
		binding ReservationBinding
		coord   *ReservationCoordination
		want    error
	}{
		"another agent of the intended user": {worker, acceptCoordination("offer-o1", ReservationWorker{UserID: coordAgent.UserID, AgentID: "agent-2"}), ErrCoordinationMismatch},
		"another offer":                      {worker, acceptCoordination("offer-other", coordAgent), ErrCoordinationMismatch},
		"another team": {func() ReservationBinding { b := worker; b.TeamID = "team-2"; return b }(),
			acceptCoordination("offer-o1", coordAgent), ErrReservationHolder},
		"another service": {func() ReservationBinding { b := worker; b.ServiceID = "aicrew-other"; return b }(),
			acceptCoordination("offer-o1", coordAgent), ErrReservationHolder},
		"no coordination": {worker, nil, ErrReservationHolder},
	} {
		if _, err := db.ApplyTaskReservation(ReservationTransfer, in, aliceActor, c.binding, "tr-"+name, c.coord, allowReservation); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v, want %v", name, err, c.want)
		}
	}
	moved, err := db.ApplyTaskReservation(ReservationTransfer, in, aliceActor, worker, "tr-ok", acceptCoordination("offer-o1", coordAgent), allowReservation)
	if err != nil || moved.Reservation.Holder.Ref != "attempt-o1" || moved.Reservation.Binding != worker || moved.Reservation.IntendedWorker != nil ||
		moved.Reservation.ID != offer.Reservation.ID || moved.Coordination.Kind != "accepted_attempt" {
		t.Fatalf("transfer: %+v %v", moved, err)
	}
	// A never-accepted release no longer applies: the hold is the attempt now.
	never := &ReservationCoordination{CoordinationRecord: CoordinationRecord{Kind: "never_accepted", ProofDigest: coordProof}, WorkRef: "offer-o1", Coordinator: true}
	rel := closing(task, moved, "READY")
	if _, err := db.ApplyTaskReservation(ReservationRelease, rel, aliceActor, coordinator(aliceActor.UserID, "sess-coord"), "never", never, allowReservation); !errors.Is(err, ErrCoordinationMismatch) {
		t.Fatalf("never-accepted release of an accepted attempt: %v", err)
	}
	// The reviewing coordinator finalizes accepted work with evidence.
	fin := closing(task, moved, "DONE")
	accepted := &ReservationCoordination{CoordinationRecord: CoordinationRecord{Kind: "accepted_for_finalization", ProofDigest: coordProof}, WorkRef: "attempt-o1", Coordinator: true}
	if _, err := db.ApplyTaskReservation(ReservationFinalize, fin, aliceActor, coordinator(aliceActor.UserID, "sess-review"), "fin-noev", accepted, allowReservation); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("DONE without evidence: %v", err)
	}
	fin.TerminalEvidence = []string{"review-head-example", "human-merge-example", "post-merge-ci-example"}
	if _, err := db.ApplyTaskReservation(ReservationFinalize, fin, aliceActor, teamBinding("user-other", "profile-1", "worker"), "fin-worker", accepted, allowReservation); !errors.Is(err, ErrReservationHolder) {
		t.Fatalf("a worker that is not the holder finalized: %v", err)
	}
	reviewer := coordinator(aliceActor.UserID, "sess-review")
	done, err := db.ApplyTaskReservation(ReservationFinalize, fin, aliceActor, reviewer, "fin", accepted, allowReservation)
	if err != nil || done.Task.State != "DONE" || done.Reservation.ID != "" || done.Binding != reviewer {
		t.Fatalf("coordinator finalize: %+v %v", done, err)
	}
	// Only the reviewing coordinator's recorded session replays it.
	if again, err := db.ApplyTaskReservation(ReservationFinalize, fin, aliceActor, reviewer, "fin", accepted, allowReservation); err != nil || !again.Replayed {
		t.Fatalf("coordinator finalize replay: %v", err)
	}
	if _, err := db.ApplyTaskReservation(ReservationFinalize, fin, aliceActor, coordinator(aliceActor.UserID, "sess-later"), "fin", accepted, allowReservation); !errors.Is(err, ErrReservationHolder) {
		t.Fatalf("another session replayed the coordinator finalize: %v", err)
	}
	if _, found, err := db.GetTaskReservationReceipt(ReservationFinalize, fin, aliceActor, coordinator(aliceActor.UserID, "sess-later"), "fin"); found || !errors.Is(err, ErrReservationHolder) {
		t.Fatalf("another session read the coordinator finalize receipt: %v %v", found, err)
	}
	// The events keep the coordination reference and the evidence, never a proof.
	var body string
	if err := db.sql.QueryRow(`SELECT body FROM task_reservation_events WHERE task_id=? AND operation='finalize'`, task.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"kind":"accepted_for_finalization"`) || !strings.Contains(body, "post-merge-ci-example") || strings.Contains(body, "acp1_") {
		t.Fatalf("finalize event: %s", body)
	}
}

// A never-accepted offer is released by its coordinator or a successor of
// the same service and team, never by a worker or another team.
func TestCoordinatedNeverAcceptedRelease(t *testing.T) {
	r, db := coordDB(t)
	selectProcess(t, db, coordPin)
	task, offer := offerClaim(t, r, db, "o2")
	never := &ReservationCoordination{CoordinationRecord: CoordinationRecord{Kind: "never_accepted", ProofDigest: coordProof}, WorkRef: "offer-o2", Coordinator: true}
	rel := closing(task, offer, "READY")
	otherTeam := coordinator("user-successor", "sess-s")
	otherTeam.TeamID = "team-2"
	for name, b := range map[string]ReservationBinding{
		"a worker":        teamBinding("user-successor", "profile-1", "worker"),
		"another team":    otherTeam,
		"a personal user": {UserID: "user-successor", Mode: "personal"},
		"another service": func() ReservationBinding {
			c := coordinator("user-successor", "sess-s")
			c.ServiceID = "aicrew-other"
			return c
		}(),
	} {
		if _, err := db.ApplyTaskReservation(ReservationRelease, rel, aliceActor, b, "rel-"+name, never, allowReservation); !errors.Is(err, ErrReservationHolder) {
			t.Fatalf("%s released the offer: %v", name, err)
		}
	}
	successor := coordinator("user-successor", "sess-s")
	out, err := db.ApplyTaskReservation(ReservationRelease, rel, TaskActor{Kind: "user", UserID: uuidv7.New(), TokenID: uuidv7.New(), Name: "successor"}, successor, "rel", never, allowReservation)
	if err != nil || out.Task.State != "READY" || out.Reservation.ID != "" || out.Reservation.IntendedWorker != nil {
		t.Fatalf("successor release: %+v %v", out, err)
	}
	if hold, _ := db.GetTaskReservation(task.ID); hold.IntendedWorker != nil {
		t.Fatalf("a closed hold keeps its intended worker: %+v", hold)
	}
}

// The pin is compared with the selection in the committing transaction:
// repo, commit and manifest byte for byte, and nothing to match without one.
func TestCoordinatedProcessPin(t *testing.T) {
	r, db := coordDB(t)
	claim := func(key string, pin ProcessPin) error {
		task := readyReservationTask(t, db, "create-"+key)
		c := offerCoordination()
		c.Process = &pin
		in := TaskReservationInput{TaskID: task.ID, ExpectedRevision: task.Revision, Holder: ReservationHolder{Mode: "external", Ref: "offer-" + key}}
		_, err := r.ClaimTaskReservation(context.Background(), in, aliceActor, coordinator(aliceActor.UserID, "sess-coord"), key, c,
			func(context.Context, string, string) error { return nil })
		return err
	}
	if err := claim("none", coordPin); !errors.Is(err, ErrProcessMismatch) {
		t.Fatalf("no selection: %v", err)
	}
	selectProcess(t, db, coordPin)
	for name, pin := range map[string]ProcessPin{
		"another commit":      {coordPin.Repo, strings.Repeat("9b", 20), coordPin.Manifest},
		"another repo form":   {"https://git.example/team/process", coordPin.Commit, coordPin.Manifest},
		"another manifest":    {coordPin.Repo, coordPin.Commit, "process/other.json"},
		"another letter case": {strings.ToUpper(coordPin.Repo[:8]) + coordPin.Repo[8:], coordPin.Commit, coordPin.Manifest},
	} {
		if err := claim("p-"+strings.ReplaceAll(name, " ", "-"), pin); !errors.Is(err, ErrProcessMismatch) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := claim("match", coordPin); err != nil {
		t.Fatalf("matching pin: %v", err)
	}
	// A selection change between the offer and its acceptance refuses the
	// transfer: it is checked against the current selection.
	task, offer := offerClaim(t, r, db, "o3")
	selectProcess(t, db, ProcessPin{coordPin.Repo, strings.Repeat("9b", 20), coordPin.Manifest})
	worker := teamBinding(coordAgent.UserID, "profile-1", "worker")
	if _, err := db.ApplyTaskReservation(ReservationTransfer, transferInput(task, offer, "attempt-o3"), aliceActor, worker, "tr",
		acceptCoordination("offer-o3", coordAgent), allowReservation); !errors.Is(err, ErrProcessMismatch) {
		t.Fatalf("transfer after a selection change: %v", err)
	}
	// The fact's own checks come first: another offer is a mismatch of the
	// fact, whatever its pin.
	if _, err := db.ApplyTaskReservation(ReservationTransfer, transferInput(task, offer, "attempt-o3"), aliceActor, worker, "tr-2",
		acceptCoordination("offer-other", coordAgent), allowReservation); !errors.Is(err, ErrCoordinationMismatch) {
		t.Fatalf("fact checked after the pin: %v", err)
	}
	// A committed claim replays after the selection moved on.
	selectProcess(t, db, coordPin)
	replayTask := readyReservationTask(t, db, "create-replay")
	in := TaskReservationInput{TaskID: replayTask.ID, ExpectedRevision: replayTask.Revision, Holder: ReservationHolder{Mode: "external", Ref: "offer-r"}}
	allow := func(context.Context, string, string) error { return nil }
	first, err := r.ClaimTaskReservation(context.Background(), in, aliceActor, coordinator(aliceActor.UserID, "sess-coord"), "replay", offerCoordination(), allow)
	if err != nil {
		t.Fatal(err)
	}
	selectProcess(t, db, ProcessPin{coordPin.Repo, strings.Repeat("9b", 20), coordPin.Manifest})
	again, err := r.ClaimTaskReservation(context.Background(), in, aliceActor, coordinator(aliceActor.UserID, "sess-coord"), "replay", offerCoordination(), allow)
	if err != nil || again.Reservation.ID != first.Reservation.ID {
		t.Fatalf("replay compared the pin again: %+v %v", again, err)
	}
}

// Stop evidence closes only the hold it was verified against (seq209): a
// transfer after verification keeps the ID and moves the work reference.
func TestRecoveryStopEvidenceBindsTheVerifiedHold(t *testing.T) {
	r, db := coordDB(t)
	selectProcess(t, db, coordPin)
	task, offer := offerClaim(t, r, db, "o4")
	verified := offer.Reservation.Holder.Ref
	moved, err := db.ApplyTaskReservation(ReservationTransfer, transferInput(task, offer, "attempt-o4"), aliceActor,
		teamBinding(coordAgent.UserID, "profile-1", "worker"), "tr", acceptCoordination("offer-o4", coordAgent), allowReservation)
	if err != nil {
		t.Fatal(err)
	}
	stop := RecoveryEvidence{Kind: "stop_evidence", Ref: coordProof, HolderRef: verified}
	in := closing(task, moved, "READY")
	if _, err := r.RecoverTaskReservation(context.Background(), RecoveryRelease, in, stop, recoveryAdmin, "rec", allowReservation); !errors.Is(err, ErrReservationStale) {
		t.Fatalf("a stop verified against the offer closed the attempt: %v", err)
	}
	stop.HolderRef = ""
	if _, err := r.RecoverTaskReservation(context.Background(), RecoveryRelease, in, stop, recoveryAdmin, "rec-2", allowReservation); !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("stop evidence without its verified hold: %v", err)
	}
	if hold, _ := db.GetTaskReservation(task.ID); hold.ID != moved.Reservation.ID || hold.Holder.Ref != "attempt-o4" {
		t.Fatalf("the attempt's hold changed: %+v", hold)
	}
	stop.HolderRef = "attempt-o4"
	if out, err := r.RecoverTaskReservation(context.Background(), RecoveryRelease, in, stop, recoveryAdmin, "rec-3", allowReservation); err != nil || out.Task.State != "READY" {
		t.Fatalf("a stop verified against the attempt: %+v %v", out, err)
	}
}

func TestReservationCoordinationShape(t *testing.T) {
	pin := coordPin
	w := coordAgent
	rec := func(kind string) CoordinationRecord { return CoordinationRecord{Kind: kind, ProofDigest: coordProof} }
	for name, c := range map[string]struct {
		op    ReservationOperation
		coord ReservationCoordination
	}{
		"no proof digest":                   {ReservationClaim, ReservationCoordination{CoordinationRecord: CoordinationRecord{Kind: "offer", ProofDigest: "acp1_x"}, IntendedWorker: &w, Process: &pin}},
		"offer without its worker":          {ReservationClaim, ReservationCoordination{CoordinationRecord: rec("offer"), Process: &pin}},
		"offer without its pin":             {ReservationClaim, ReservationCoordination{CoordinationRecord: rec("offer"), IntendedWorker: &w}},
		"claim naming a work ref":           {ReservationClaim, ReservationCoordination{CoordinationRecord: rec("independent_claim"), WorkRef: "x", Process: &pin}},
		"independent claim without its pin": {ReservationClaim, ReservationCoordination{CoordinationRecord: rec("independent_claim")}},
		"transfer without its worker":       {ReservationTransfer, ReservationCoordination{CoordinationRecord: rec("accepted_attempt"), WorkRef: "o", Process: &pin}},
		"release without a work ref":        {ReservationRelease, ReservationCoordination{CoordinationRecord: rec("stopped")}},
		"coordinator update":                {ReservationUpdate, ReservationCoordination{CoordinationRecord: rec("stopped"), WorkRef: "a", Coordinator: true}},
		"stop with a pin":                   {ReservationRelease, ReservationCoordination{CoordinationRecord: rec("stopped"), WorkRef: "a", Process: &pin}},
	} {
		if err := c.coord.validate(c.op); !errors.Is(err, ErrTaskInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, in := range []TaskReservationInput{
		{TerminalEvidence: []string{"x"}, Content: &TaskContent{Title: "t", State: "READY"}, Reason: "r"},
		{TerminalEvidence: make([]string, 17), Content: &TaskContent{Title: "t", State: "DONE"}, Reason: "r"},
		{TerminalEvidence: []string{""}, Content: &TaskContent{Title: "t", State: "CANCELLED"}, Reason: "r"},
	} {
		op := ReservationFinalize
		if in.Content.State == "READY" {
			op = ReservationRelease
		}
		in.TaskID, in.ID, in.Fence, in.ExpectedRevision = "01a0e39c-0000-7000-8000-00000000c501", "01a0e39c-0000-7000-8000-00000000c502", 1, 1
		if err := validateReservationInput(op, &in); !errors.Is(err, ErrTaskInvalid) {
			t.Errorf("terminal evidence %v on %s: %v", in.TerminalEvidence, op, err)
		}
	}
}

// "DONE requires terminal evidence" governs finalizing a reservation hold
// only. A plain task update to DONE, by a user holding no reservation, is
// unchanged: no evidence field exists there and none is asked for.
func TestPlainTaskUpdateToDoneNeedsNoEvidence(t *testing.T) {
	_, db := taskDB(t)
	task, err := db.CreateTask(TaskContent{Title: "plain work", State: "IN_PROGRESS"}, aliceActor, "plain-create")
	if err != nil {
		t.Fatal(err)
	}
	done, err := db.UpdateTask(task.ID, TaskContent{Title: "plain work", State: "DONE"}, task.Revision, aliceActor, "plain-done")
	if err != nil || done.State != "DONE" || done.Revision != task.Revision+1 {
		t.Fatalf("plain update to DONE: %+v %v", done, err)
	}
	if hold, _ := db.GetTaskReservation(task.ID); hold.ID != "" {
		t.Fatalf("a plain update took a hold: %+v", hold)
	}
}
