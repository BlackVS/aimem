package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The shared fixture's canonical vectors are exactly EvidenceDigest: aicrew
// computes the same digests from the same vectors (C5-w3).
func TestEvidenceDigestVectors(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "coordination-v1", "examples.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		EvidenceDigest struct {
			Vectors []struct {
				Case             string   `json:"case"`
				TerminalEvidence []string `json:"terminal_evidence"`
				Digest           string   `json:"digest"`
			} `json:"vectors"`
		} `json:"evidence_digest"`
	}
	if err := json.Unmarshal(b, &ex); err != nil || len(ex.EvidenceDigest.Vectors) < 10 {
		t.Fatalf("fixture vectors: %v", err)
	}
	for _, v := range ex.EvidenceDigest.Vectors {
		if got := EvidenceDigest(v.TerminalEvidence); got != v.Digest || !evidenceDigestRE.MatchString(got) {
			t.Errorf("%s: %s, fixture %s", v.Case, got, v.Digest)
		}
	}
}

// The finalize fact, and only it, binds a well-formed digest; a finalize
// whose evidence is not exactly the bound evidence is refused in the
// transaction, with nothing applied.
func TestFinalizeEvidenceMustMatchTheFact(t *testing.T) {
	r, db := coordDB(t)
	selectProcess(t, db, coordPin)
	task, offer := offerClaim(t, r, db, "offer-ev")
	confirmed := []string{"review-head-example", "human-merge-example", "post-merge-ci-example"}
	fact := func(digest string) *ReservationCoordination {
		return &ReservationCoordination{CoordinationRecord: CoordinationRecord{Kind: "accepted_for_finalization", ProofDigest: coordProof,
			EvidenceDigest: digest}, WorkRef: offer.Reservation.Holder.Ref, Coordinator: true}
	}
	fin := closing(task, offer, "DONE")
	fin.TerminalEvidence = confirmed
	coord := coordinator(aliceActor.UserID, "sess-coord")
	for name, c := range map[string]*ReservationCoordination{
		"no digest":        fact(""),
		"malformed digest": fact("e1_short"),
		"digest on a stop": {CoordinationRecord: CoordinationRecord{Kind: "stopped", ProofDigest: coordProof, EvidenceDigest: EvidenceDigest(confirmed)}, WorkRef: offer.Reservation.Holder.Ref},
	} {
		if _, err := db.ApplyTaskReservation(ReservationFinalize, fin, aliceActor, coord, "k-"+name, c, allowReservation); !errors.Is(err, ErrTaskInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	mismatch := fin
	mismatch.TerminalEvidence = []string{confirmed[1], confirmed[0], confirmed[2]}
	if _, err := db.ApplyTaskReservation(ReservationFinalize, mismatch, aliceActor, coord, "k-mismatch", fact(EvidenceDigest(confirmed)), allowReservation); !errors.Is(err, ErrEvidenceMismatch) {
		t.Fatalf("reordered evidence: %v", err)
	}
	if hold, err := db.GetTaskReservation(task.ID); err != nil || hold.ID != offer.Reservation.ID || hold.Fence != offer.Reservation.Fence {
		t.Fatalf("a refused finalize changed the hold: %+v %v", hold, err)
	}
	done, err := db.ApplyTaskReservation(ReservationFinalize, fin, aliceActor, coord, "k-match", fact(EvidenceDigest(confirmed)), allowReservation)
	if err != nil || done.Task.State != "DONE" || done.Coordination == nil || done.Coordination.EvidenceDigest != EvidenceDigest(confirmed) {
		t.Fatalf("matching finalize: %+v %v", done, err)
	}
}
