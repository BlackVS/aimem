package server

// Coordination-backed reservation transitions (task C5b;
// docs/DESIGN-AIFORGE-COORDINATION-WIRE.md §1). A team transition other
// than a holder's update carries the member's coordination_proof. The hub
// asks aicrew about it once, before the ledger transaction, binds the fact
// to the request and the verified caller, and hands the ledger what it
// verified; the ledger checks the hold-dependent parts and the process pin
// inside the committing transaction.

import (
	"context"
	"errors"
	"slices"

	"aimem/internal/introspect"
	"aimem/internal/store"
)

// coordinationAnswerMaxAge bounds how old a coordination answer may be when
// its transition commits (wire: at most 5 s, decision D2a). Tests shrink it.
var coordinationAnswerMaxAge = reservationVerificationAge

// teamFactKinds is the wire's fact table: per operation and caller role,
// the fact kinds that authorize it. A missing entry is refused.
var teamFactKinds = map[store.ReservationOperation]map[string][]string{
	store.ReservationClaim:    {"coordinator": {"offer"}, "independent": {"independent_claim"}},
	store.ReservationTransfer: {"worker": {"accepted_attempt"}},
	store.ReservationRelease:  {"coordinator": {"never_accepted"}, "worker": {"stopped"}, "independent": {"stopped"}},
	store.ReservationFinalize: {"coordinator": {"accepted_for_finalization"}, "worker": {"accepted_for_finalization"},
		"independent": {"accepted_for_finalization"}},
}

// coordinate verifies the member's proof through coordination.v1 and binds
// the fact to this request and caller. It never names another holder.
func (s *Server) coordinate(ctx context.Context, c reservationCaller, op store.ReservationOperation, in store.TaskReservationInput,
	key, proof string, kinds []string) (*store.ReservationCoordination, error) {
	rejected := func(detail string) error { return refuseReservation("coordination_rejected", detail) }
	if !introspect.ValidProof(proof) {
		return nil, rejected("the coordination proof is malformed")
	}
	peer, ok := s.operationalPeer()
	if !ok || s.introspect == nil {
		return nil, refuseReservation("context_unavailable", "no operational aicrew peer")
	}
	if peer.ServiceID != c.team.ServiceID {
		return nil, rejected("the caller's team profile is not linked to the answering service")
	}
	fact, err := s.introspect.Coordinate(ctx, toIntrospectPeer(peer), proof)
	if err != nil {
		var f *introspect.Failure
		if errors.As(err, &f) && f.Code == introspect.CodeRejected {
			return nil, rejected("aicrew does not vouch for this step")
		}
		return nil, refuseReservation("context_unavailable", "aicrew could not be asked")
	}
	tc, m := c.team, fact.Member
	switch {
	case !slices.Contains(kinds, fact.Kind), fact.Operation != string(op):
		return nil, rejected("the fact is for another step")
	case fact.TaskID != in.TaskID:
		return nil, rejected("the fact is for another task")
	case fact.RequestKey != store.RequestKeyDigest(key):
		return nil, rejected("the fact is for another request key")
	case m.UserID != tc.UserID, m.AgentID != tc.AgentID, m.TeamID != tc.TeamID, m.Role != tc.Role,
		m.SessionID != tc.SessionID, m.Generation != tc.Generation:
		return nil, rejected("the fact names another member")
	}
	coord := &store.ReservationCoordination{CoordinationRecord: store.CoordinationRecord{Kind: fact.Kind, ProofDigest: proofP1Digest(proof)}}
	if p := fact.Process; p != nil {
		coord.Process = &store.ProcessPin{Repo: p.Repo, Commit: p.Commit, Manifest: p.Manifest}
	}
	holder := func(ref string) bool { return in.Holder == store.ReservationHolder{Mode: "external", Ref: ref} }
	switch fact.Kind {
	case "offer":
		if !holder(fact.OfferRef) {
			return nil, rejected("the claim is not the offer's")
		}
		coord.IntendedWorker = &store.ReservationWorker{UserID: fact.IntendedWorker.UserID, AgentID: fact.IntendedWorker.AgentID}
	case "independent_claim":
		if !holder(fact.AttemptRef) {
			return nil, rejected("the claim is not the attempt's")
		}
	case "accepted_attempt":
		if !holder(fact.AttemptRef) {
			return nil, rejected("the transfer is not to the accepted attempt")
		}
		coord.WorkRef = fact.OfferRef
		coord.Worker = &store.ReservationWorker{UserID: m.UserID, AgentID: m.AgentID}
	case "never_accepted":
		coord.WorkRef, coord.Coordinator = fact.OfferRef, true
	case "stopped":
		coord.WorkRef = fact.AttemptRef
	case "accepted_for_finalization":
		coord.WorkRef, coord.Coordinator = fact.AttemptRef, c.binding.Role == "coordinator"
		coord.EvidenceDigest = fact.EvidenceDigest
	}
	return coord, nil
}
