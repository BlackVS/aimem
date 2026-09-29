package introspect

// coordination.v1: aimem's client for aicrew's coordination facts
// (docs/DESIGN-AIFORGE-COORDINATION-WIRE.md §1). It shares introspection's
// peer, credential, pinned TLS, one 2 s attempt, 16 KiB reply and
// cache-free rules. A reply that is not about the fact (unreachable,
// malformed, wrong nonce, service or hub) is context_unavailable; an
// inactive or expired fact is coordination_rejected.

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"time"

	"aimem/internal/process"
)

const (
	CoordinationPath          = "/v1/crew/coordination"
	CoordinationVersionHeader = "X-Aimem-Coordination-Version"
	// CodeRejected is the refusal for a fact aicrew does not vouch for.
	CodeRejected = "coordination_rejected"
)

var (
	proofShape = regexp.MustCompile(`^acp1_[A-Za-z0-9_-]{43}$`)
	keyDigest  = regexp.MustCompile(`^k1_[A-Za-z0-9_-]{43}$`)
	// evidenceDigest is the e1_ digest of confirmed delivery evidence
	// (C5-w3), which only an accepted_for_finalization fact carries.
	evidenceDigest = regexp.MustCompile(`^e1_[A-Za-z0-9_-]{43}$`)
	// factKinds maps each fact kind to the operation it vouches for.
	factKinds = map[string]string{
		"offer": "claim", "accepted_attempt": "transfer", "never_accepted": "release",
		"stopped": "release", "accepted_for_finalization": "finalize", "independent_claim": "claim",
	}
	// pinnedKinds are the facts that start work: they carry the process pin
	// (C5-w2), and no other kind does.
	pinnedKinds = map[string]bool{"offer": true, "accepted_attempt": true, "independent_claim": true}
)

// ValidProof reports whether p has the shape of a coordination proof.
func ValidProof(p string) bool { return proofShape.MatchString(p) }

// Member is the acting member a fact names.
type Member struct {
	UserID, AgentID, TeamID, Role, SessionID, Generation string
}

// Worker names an offer's intended worker.
type Worker struct{ UserID, AgentID string }

// Process is the process version a fact that starts work carries: the
// reference aicrew recorded, in the forms of the hub's process selection.
type Process struct{ Repo, Commit, Manifest string }

// Fact is an active coordination fact, exactly as aicrew reported it. The
// caller binds it to the request, the hold and the verified caller.
type Fact struct {
	ServiceID, HubID                    string
	Kind, Operation, TaskID, RequestKey string
	Member                              Member
	OfferRef, AttemptRef                string
	IntendedWorker                      *Worker
	Process                             *Process
	// EvidenceDigest binds the delivery evidence the coordinator confirmed
	// (C5-w3); only an accepted_for_finalization fact carries it.
	EvidenceDigest string
	ExpiresAt      time.Time
}

type coordinationRequest struct {
	Version int    `json:"version"`
	HubID   string `json:"hub_id"`
	Nonce   string `json:"nonce"`
	Proof   string `json:"proof"`
}

type coordinationReply struct {
	Nonce     *string `json:"nonce"`
	Active    *bool   `json:"active"`
	ServiceID *string `json:"service_id"`
	HubID     *string `json:"hub_id"`
	Fact      *struct {
		Kind             *string `json:"kind"`
		Operation        *string `json:"operation"`
		TaskID           *string `json:"task_id"`
		RequestKeyDigest *string `json:"request_key_digest"`
		Member           *struct {
			UserID     *string `json:"user_id"`
			AgentID    *string `json:"agent_id"`
			TeamID     *string `json:"team_id"`
			Role       *string `json:"role"`
			SessionID  *string `json:"session_id"`
			Generation *string `json:"generation"`
		} `json:"member"`
		OfferRef       *string `json:"offer_ref"`
		AttemptRef     *string `json:"attempt_ref"`
		IntendedWorker *struct {
			UserID  *string `json:"user_id"`
			AgentID *string `json:"agent_id"`
		} `json:"intended_worker"`
		Process *struct {
			Repo     *string `json:"repo"`
			Commit   *string `json:"commit"`
			Manifest *string `json:"manifest"`
		} `json:"process"`
		EvidenceDigest *string `json:"evidence_digest"`
		ExpiresAt      *string `json:"expires_at"`
	} `json:"fact"`
}

// coordinationEndpoint is the coordination route on the origin of p's
// registered introspection endpoint.
func coordinationEndpoint(p Peer) (string, error) {
	u, err := url.Parse(p.Endpoint)
	if err != nil {
		return "", err
	}
	u.Path, u.RawPath = CoordinationPath, ""
	return u.String(), nil
}

// Coordinate asks p about proof and returns the verified active fact, or a
// *Failure. It checks the reply's own shape; binding the fact to the
// request, the hold and the caller is the caller's.
func (c *Client) Coordinate(ctx context.Context, p Peer, proof string) (Fact, error) {
	if !ValidProof(proof) {
		return Fact{}, &Failure{CodeRejected, "proof_shape"}
	}
	endpoint, err := coordinationEndpoint(p)
	if err != nil {
		return Fact{}, unavailable("peer_record")
	}
	nonce, err := newNonce()
	if err != nil {
		return Fact{}, err
	}
	body, err := json.Marshal(coordinationRequest{Version: 1, HubID: p.HubID, Nonce: nonce, Proof: proof})
	if err != nil {
		return Fact{}, unavailable("request")
	}
	data, err := c.exchange(ctx, p, endpoint, CoordinationVersionHeader, body)
	if err != nil {
		return Fact{}, err
	}
	return c.verifyFact(p, nonce, data)
}

func (c *Client) verifyFact(p Peer, nonce string, data []byte) (Fact, error) {
	var r coordinationReply
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		return Fact{}, unavailable("malformed")
	}
	if r.Nonce == nil || subtle.ConstantTimeCompare([]byte(*r.Nonce), []byte(nonce)) != 1 {
		return Fact{}, unavailable("nonce_mismatch")
	}
	if r.Active == nil {
		return Fact{}, unavailable("malformed")
	}
	if !*r.Active {
		if r.ServiceID != nil || r.HubID != nil || r.Fact != nil {
			return Fact{}, unavailable("malformed")
		}
		return Fact{}, &Failure{CodeRejected, "inactive"}
	}
	if r.Fact == nil || r.Fact.Member == nil {
		return Fact{}, unavailable("malformed")
	}
	if str(r.ServiceID) != p.ServiceID {
		return Fact{}, unavailable("wrong_service")
	}
	if str(r.HubID) != p.HubID {
		return Fact{}, unavailable("wrong_hub")
	}
	f, m := r.Fact, r.Fact.Member
	got := Fact{
		ServiceID: p.ServiceID, HubID: p.HubID,
		Kind: str(f.Kind), Operation: str(f.Operation), TaskID: str(f.TaskID), RequestKey: str(f.RequestKeyDigest),
		Member: Member{UserID: str(m.UserID), AgentID: str(m.AgentID), TeamID: str(m.TeamID), Role: str(m.Role),
			SessionID: str(m.SessionID), Generation: str(m.Generation)},
		OfferRef: str(f.OfferRef), AttemptRef: str(f.AttemptRef),
	}
	op, known := factKinds[got.Kind]
	if !known || got.Operation != op || !keyDigest.MatchString(got.RequestKey) || !generationShape.MatchString(got.Member.Generation) ||
		!roles[got.Member.Role] {
		return Fact{}, unavailable("malformed")
	}
	for _, id := range []string{got.TaskID, got.Member.UserID, got.Member.AgentID, got.Member.TeamID, got.Member.SessionID} {
		if !idShape.MatchString(id) {
			return Fact{}, unavailable("malformed")
		}
	}
	// Each kind carries exactly the references the contract names.
	needOffer := got.Kind == "offer" || got.Kind == "accepted_attempt" || got.Kind == "never_accepted"
	needAttempt := got.Kind != "offer" && got.Kind != "never_accepted"
	if (f.OfferRef != nil) != needOffer || (f.AttemptRef != nil) != needAttempt || (f.IntendedWorker != nil) != (got.Kind == "offer") {
		return Fact{}, unavailable("malformed")
	}
	if (needOffer && got.OfferRef == "") || (needAttempt && got.AttemptRef == "") {
		return Fact{}, unavailable("malformed")
	}
	if f.IntendedWorker != nil {
		w := Worker{UserID: str(f.IntendedWorker.UserID), AgentID: str(f.IntendedWorker.AgentID)}
		if !idShape.MatchString(w.UserID) || !idShape.MatchString(w.AgentID) {
			return Fact{}, unavailable("malformed")
		}
		got.IntendedWorker = &w
	}
	// The pin is exactly on the facts that start work, with exactly its
	// three fields in the selection's forms; anything else is a wrong shape.
	if (f.Process != nil) != pinnedKinds[got.Kind] {
		return Fact{}, unavailable("malformed")
	}
	if pin := f.Process; pin != nil {
		if pin.Repo == nil || pin.Commit == nil || pin.Manifest == nil {
			return Fact{}, unavailable("malformed")
		}
		ref := process.Ref{Repo: *pin.Repo, Commit: *pin.Commit, Manifest: *pin.Manifest}
		if ref.Validate() != nil {
			return Fact{}, unavailable("malformed")
		}
		got.Process = &Process{Repo: ref.Repo, Commit: ref.Commit, Manifest: ref.Manifest}
	}
	// The evidence digest is exactly on the finalize fact, in its e1_ form.
	if (f.EvidenceDigest != nil) != (got.Kind == "accepted_for_finalization") ||
		(f.EvidenceDigest != nil && !evidenceDigest.MatchString(*f.EvidenceDigest)) {
		return Fact{}, unavailable("malformed")
	}
	got.EvidenceDigest = str(f.EvidenceDigest)
	exp, err := time.Parse(time.RFC3339, str(f.ExpiresAt))
	if err != nil {
		return Fact{}, unavailable("malformed")
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	if !exp.After(now()) {
		return Fact{}, &Failure{CodeRejected, "expired"}
	}
	got.ExpiresAt = exp.UTC()
	return got, nil
}
