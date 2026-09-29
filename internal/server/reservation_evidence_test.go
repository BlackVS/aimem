package server

// C5-w3: a coordination-backed finalize carries exactly the delivery
// evidence the coordinator confirmed through aicrew. The accepted-for-
// finalization fact binds its e1_ digest, and the hub refuses any other
// evidence with evidence_mismatch, in the committing transaction.

import (
	"strconv"
	"testing"

	"aimem/internal/store"
	"aimem/internal/uuidv7"
)

var confirmedEvidence = []string{"review-head-example", "human-merge-example", "post-merge-ci-example"}

func TestFinalizeEvidenceDigestThroughTheRoutes(t *testing.T) {
	g := newReadRig(t)
	indep := g.member(t, "independent", "agent-indep", "sess-i")
	task := g.readyTask(t, g.alpha)
	held, _ := g.claim(t, indep, task.ID, "attempt-e")
	digest := store.EvidenceDigest(confirmedEvidence)

	// finalize sends evidence under key, on a fact that binds digest ("" for
	// a reply without one).
	finalize := func(key, digest string, evidence []string) identityResp {
		t.Helper()
		f := g.factAs("independent", "agent-indep", "sess-i", "accepted_for_finalization", "finalize", task.ID, key)
		f["attempt_ref"] = "attempt-e"
		if digest != "" {
			f["evidence_digest"] = digest
		}
		g.answerFact(f)
		proof := testProof(t)
		g.secrets = append(g.secrets, proof)
		return g.call(t, g.tls, "POST", g.rpath(task.ID, "/finalize"), g.alice,
			rhdr(key, g.teamAs(t, "independent", "agent-indep", "sess-i")), body(t, map[string]any{
				"expected_revision": held.Task.Revision, "reservation_id": held.Reservation.ID,
				"fence": strconv.FormatInt(held.Reservation.Fence, 10), "reason": "reviewed delivery",
				"content": map[string]any{"title": task.Title, "state": "DONE"}, "terminal_evidence": evidence,
				"coordination_proof": proof}), true)
	}
	unchanged := func(what string) {
		t.Helper()
		hold, err := g.alpha.GetTaskReservation(task.ID)
		cur, _ := g.alpha.GetTask(task.ID)
		if err != nil || hold.ID != held.Reservation.ID || hold.Fence != held.Reservation.Fence || cur.State == "DONE" {
			t.Fatalf("%s applied something: %+v %s %v", what, hold, cur.State, err)
		}
	}
	e := confirmedEvidence
	for name, evidence := range map[string][]string{
		"reordered":     {e[1], e[0], e[2]},
		"altered":       {e[0], "human-merge-substitute", e[2]},
		"trailing":      {e[0] + " ", e[1], e[2]},
		"dropped":       {e[0], e[1]},
		"extra":         {e[0], e[1], e[2], "extra-example"},
		"another order": {e[2], e[1], e[0]},
	} {
		wantRefusal(t, name, finalize("fin-"+uuidv7.New(), digest, evidence), 409, "evidence_mismatch", "team")
		unchanged(name)
	}
	// A reply without the digest, or a finalize fact from before C5-w3, is a
	// wrong-shaped answer: retryable, and nothing applied.
	wantRefusal(t, "a fact without the digest", finalize("fin-"+uuidv7.New(), "", confirmedEvidence), 503, "context_unavailable", "team")
	unchanged("a fact without the digest")

	// The confirmed evidence, in its order, commits, and the receipt records
	// the digest.
	key := "fin-" + uuidv7.New()
	o := decodeOutcome(t, finalize(key, digest, confirmedEvidence))
	if o.Receipt.Operation != "finalize" || o.Reservation.Active {
		t.Fatalf("finalize: %+v", o)
	}
	if cur, _ := g.alpha.GetTask(task.ID); cur.State != "DONE" {
		t.Fatalf("task state after finalize: %s", cur.State)
	}
	receipts, err := g.alpha.RecoveryReceipts(task.ID, store.ReservationFinalize, store.RequestKeyDigest(key))
	if err != nil || len(receipts) != 1 || receipts[0].Outcome.Coordination == nil ||
		receipts[0].Outcome.Coordination.EvidenceDigest != digest {
		t.Fatalf("the receipt does not record the digest: %+v %v", receipts, err)
	}
	// The same request replays from its receipt, without asking aicrew for
	// the fact; a changed list under the same key conflicts.
	g.answerInactive()
	if again := decodeOutcome(t, finalize(key, digest, confirmedEvidence)); !again.Receipt.Replayed {
		t.Fatalf("replay: %+v", again)
	}
	wantRefusal(t, "a changed list under the key", finalize(key, digest, []string{e[1], e[0], e[2]}), 409, "idempotency_conflict", "team")
	g.assertNoSecretLeak(t)
}

// A personal finalize to DONE carries no proof and no fact, so no digest:
// any terminal evidence is accepted, as before C5-w3.
func TestFinalizeEvidenceDigestLeavesPersonalDoneUnchanged(t *testing.T) {
	g := newReservationRig(t)
	alice := g.personal(g.aliceIdentity)
	task := g.readyTask(t, g.alpha)
	held, err := g.s.reserve(alice, store.ReservationClaim, claimOf(task), "p-claim", "")
	if err != nil {
		t.Fatal(err)
	}
	done := nextOf(task, held, "DONE")
	done.TerminalEvidence = []string{"any-evidence-example", "in-any-order"}
	out, err := g.s.reserve(alice, store.ReservationFinalize, done, "p-done", "")
	if err != nil || out.Task.State != "DONE" || out.Coordination != nil {
		t.Fatalf("personal DONE: %+v %v", out, err)
	}
}
