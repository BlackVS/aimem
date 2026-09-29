package store

// Coordination-backed transitions (task C5b; docs/DESIGN-AIFORGE-
// COORDINATION-WIRE.md). The hub's authorizer verifies one coordination.v1
// fact online, before the transaction, and hands the ledger what it
// verified. The ledger checks, inside the committing transaction, the parts
// that can change under it: the hold's work reference, the offer's intended
// worker, the coordinator path, the process pin against the project's
// current selection, and last a finalize's terminal evidence against the
// fact's evidence digest (C5-w3).

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"regexp"
)

var (
	// ErrCoordinationMismatch: the verified fact no longer describes the
	// hold (another work reference, or not the offer's intended worker).
	ErrCoordinationMismatch = errors.New("the coordination fact does not match the hold")
	// ErrProcessMismatch: the fact's process pin is not the project's
	// current selection, or the project has none.
	ErrProcessMismatch = errors.New("the process pin is not the project's current selection")
	// ErrEvidenceMismatch: a finalize's terminal evidence is not exactly
	// the delivery evidence the fact's digest binds (C5-w3).
	ErrEvidenceMismatch = errors.New("the terminal evidence is not the confirmed delivery evidence")
	evidenceDigestRE    = regexp.MustCompile(`^e1_[A-Za-z0-9_-]{43}$`)
)

// EvidenceDigest is the e1_ digest of a finalize's terminal evidence
// (coordination wire, "Evidence digest"): each reference in order, as its
// 4-byte big-endian length and then its UTF-8 bytes, hashed with SHA-256,
// unpadded base64url. No reference is trimmed, folded, sorted or merged, so
// reordering, altering, dropping or adding one changes the digest.
func EvidenceDigest(refs []string) string {
	h := sha256.New()
	var n [4]byte
	for _, ref := range refs {
		binary.BigEndian.PutUint32(n[:], uint32(len(ref)))
		h.Write(n[:])
		h.Write([]byte(ref))
	}
	return "e1_" + base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

// ReservationWorker names an offer's intended worker: the user and the
// agent aicrew named. Only that worker's accepted attempt takes the hold.
type ReservationWorker struct {
	UserID  string `json:"user_id"`
	AgentID string `json:"agent_id"`
}

// ProcessPin is the process version a fact carries (C5-w2): the reference
// the project's selection is stored as, without its fetch hint or metadata.
type ProcessPin struct {
	Repo     string `json:"repo"`
	Commit   string `json:"commit"`
	Manifest string `json:"manifest"`
}

// CoordinationRecord is the immutable coordination reference a committed
// transition records in its receipt and event: the fact kind and the
// proof's p1_ digest, never the proof.
type CoordinationRecord struct {
	Kind        string `json:"kind"`
	ProofDigest string `json:"proof_digest"`
	// EvidenceDigest is the e1_ digest of the delivery evidence an
	// accepted_for_finalization fact binds (C5-w3); empty on other kinds.
	EvidenceDigest string `json:"evidence_digest,omitempty"`
}

// ReservationCoordination is what the authorizer verified from one fact.
// Only CoordinationRecord is recorded; the rest is checked in the
// transaction.
type ReservationCoordination struct {
	CoordinationRecord
	// WorkRef is the work reference the fact names for the current hold;
	// empty on a claim.
	WorkRef string
	// IntendedWorker is an offer's named worker, recorded on the hold.
	IntendedWorker *ReservationWorker
	// Worker is an accepted attempt's member, who must be the worker the
	// offer named.
	Worker *ReservationWorker
	// Coordinator marks the paths a coordinator of the hold's service and
	// team takes without holding the work: the release of a never-accepted
	// offer, and the finalize of accepted work.
	Coordinator bool
	// Process is the pin a fact that starts work carries: an offer, an
	// accepted attempt or an independent claim (C5-w2).
	Process *ProcessPin
}

func (c *ReservationCoordination) validate(op ReservationOperation) error {
	if c.Kind == "" || !proofDigestRE.MatchString(c.ProofDigest) {
		return invalid(errors.New("coordination needs its fact kind and proof digest"))
	}
	claim := op == ReservationClaim
	switch {
	case claim && c.WorkRef != "", !claim && c.WorkRef == "":
		return invalid(errors.New("a coordinated transition names the hold's work reference, a claim none"))
	case (c.IntendedWorker != nil) != (claim && c.Kind == "offer"):
		return invalid(errors.New("only an offer names an intended worker"))
	case (c.Worker != nil) != (op == ReservationTransfer):
		return invalid(errors.New("only a transfer names the accepting worker"))
	case (c.Process != nil) != (c.Kind == "offer" || c.Kind == "accepted_attempt" || c.Kind == "independent_claim"):
		return invalid(errors.New("the facts that start work carry the process pin, nothing else"))
	case c.Coordinator && op != ReservationRelease && op != ReservationFinalize:
		return invalid(errors.New("the coordinator path is a release or a finalize"))
	case (c.EvidenceDigest != "") != (c.Kind == "accepted_for_finalization"),
		c.EvidenceDigest != "" && !evidenceDigestRE.MatchString(c.EvidenceDigest):
		return invalid(errors.New("the finalize fact, and only it, binds the evidence digest"))
	}
	return nil
}

func (c *ReservationCoordination) pin() *ProcessPin {
	if c == nil {
		return nil
	}
	return c.Process
}

func (c *ReservationCoordination) intended() *ReservationWorker {
	if c == nil || c.IntendedWorker == nil {
		return nil
	}
	w := *c.IntendedWorker
	return &w
}

// record is what the receipt keeps, or nil.
func (c *ReservationCoordination) record() *CoordinationRecord {
	if c == nil {
		return nil
	}
	r := c.CoordinationRecord
	return &r
}

// checkCoordinatedHolder applies the actor rule for a transition on an
// existing hold. Without coordination the caller must be the holder, and a
// transfer is refused. A transfer goes to the offer's intended worker, of
// the hold's service and team. The coordinator path needs a coordinator of
// the hold's service and team. Every other coordinated transition is the
// holder's.
func checkCoordinatedHolder(op ReservationOperation, hold TaskReservation, binding ReservationBinding, c *ReservationCoordination) error {
	if c == nil {
		if op == ReservationTransfer {
			return ErrReservationHolder
		}
		if !hold.Binding.SameHolder(binding) {
			return ErrReservationHolder
		}
		return nil
	}
	if hold.Holder.Ref != c.WorkRef {
		return ErrCoordinationMismatch
	}
	sameTeam := binding.Mode == "team" && hold.Binding.Mode == "team" &&
		binding.ServiceID == hold.Binding.ServiceID && binding.TeamID == hold.Binding.TeamID
	switch {
	case op == ReservationTransfer:
		if !sameTeam {
			return ErrReservationHolder
		}
		if hold.IntendedWorker == nil || *hold.IntendedWorker != *c.Worker {
			return ErrCoordinationMismatch
		}
	case c.Coordinator:
		if !sameTeam || binding.Role != "coordinator" {
			return ErrReservationHolder
		}
	case !hold.Binding.SameHolder(binding):
		return ErrReservationHolder
	}
	return nil
}

// checkEvidenceDigest compares a finalize's terminal evidence with the
// digest its fact binds, in the committing transaction: the evidence must be
// exactly the delivery evidence the coordinator confirmed, in its order.
func checkEvidenceDigest(c *ReservationCoordination, evidence []string) error {
	if c == nil || c.EvidenceDigest == "" {
		return nil
	}
	if EvidenceDigest(evidence) != c.EvidenceDigest {
		return ErrEvidenceMismatch
	}
	return nil
}

// checkProcessPin compares a pin with the project's current selection, read
// in the committing transaction: repo, commit and manifest, byte for byte.
func checkProcessPin(tx *sql.Tx, pin *ProcessPin) error {
	if pin == nil {
		return nil
	}
	var raw string
	err := tx.QueryRow(`SELECT value FROM meta WHERE key='process'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && raw == "") {
		return ErrProcessMismatch
	}
	if err != nil {
		return err
	}
	var current ProcessPin
	if err := json.Unmarshal([]byte(raw), &current); err != nil {
		return err
	}
	if current != *pin {
		return ErrProcessMismatch
	}
	return nil
}

// actingMember reports whether caller may replay or read a receipt whose
// transition was made by recorded. It is the same holder (the session may
// have moved on), except a coordinator's finalize, which only that
// coordinator's recorded session replays.
func actingMember(op ReservationOperation, recorded, caller ReservationBinding) bool {
	if !recorded.SameHolder(caller) {
		return false
	}
	if op == ReservationFinalize && recorded.Role == "coordinator" {
		return recorded.SessionID == caller.SessionID
	}
	return true
}
