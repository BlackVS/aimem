package server

// identity.v1 HTTP wire (docs/DESIGN-AIFORGE-IDENTITY-WIRE.md) over the
// access-store proof ledger. Every route here requires TLS terminated by this
// hub (r.TLS): plain HTTP and the unix socket are refused, and forwarded
// headers never count. None of these routes is an MCP tool: a proof receipt
// is a secret that must never enter a model-visible tool result, transcript
// or log. Nothing here makes introspection operational.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"aimem/internal/access"
	"aimem/internal/uuidv7"
)

const identityVersionHeader = "X-Aimem-Identity-Version"

// identityWait bounds how long a proof or redemption waits for the store; an
// identical in-flight request that is still running past it gets the
// retryable request_in_progress and nothing is applied.
const identityWait = 5 * time.Second

// identityRefusal is the context contract's refusal envelope. It never
// carries a bearer, receipt or another actor's identity.
type identityRefusalBody struct {
	Code          string `json:"code"`
	Message       string `json:"message"`
	Retryable     bool   `json:"retryable"`
	NextAction    string `json:"next_action"`
	CorrelationID string `json:"correlation_id"`
}

var identityRefusals = map[string]struct {
	status    int
	retryable bool
	message   string
	next      string
}{
	"invalid_request":            {400, false, "The request is malformed.", "Correct the request."},
	"unsupported_version":        {400, false, "This identity version is not supported.", "Use identity version 1."},
	"tls_required":               {403, false, "This route requires TLS terminated by the hub.", "Connect to the hub's TLS listener with certificate verification."},
	"invalid_credential":         {401, false, "The individual credential is missing, expired or revoked.", "Recover the individual credential through its authorized flow."},
	"peer_unauthenticated":       {401, false, "The peer credential is not valid.", "Operator checks the peer registration and credential."},
	"credential_scope_forbidden": {403, false, "This credential cannot request identity proofs.", "Use the installation's user-scoped individual credential."},
	"peer_unknown":               {403, false, "No such peer is registered for this hub.", "Verify the hub and aicrew service locators."},
	"peer_forbidden":             {403, false, "The peer credential does not permit this operation.", "Operator checks the peer's permitted operations."},
	"proof_invalid":              {403, false, "The proof receipt is not valid for this redemption.", "Obtain a new receipt, or begin a new challenge."},
	"credential_inactive":        {403, false, "The proven credential is no longer active.", "Recover the individual credential; do not link."},
	"idempotency_conflict":       {409, false, "This request key was used with different input.", "Investigate the changed input; never reuse the key for other input."},
	"rate_limited":               {429, true, "Too many proof receipts were requested.", "Wait, then request again."},
	"request_in_progress":        {503, true, "An identical request is still being processed.", "Retry later with the same key; nothing was applied."},
	"identity_unavailable":       {503, true, "Identity storage is unavailable.", "Retry later with the same key; nothing was applied."},
}

func (s *Server) identityRefuse(w http.ResponseWriter, code string) {
	ref, ok := identityRefusals[code]
	if !ok {
		code, ref = "identity_unavailable", identityRefusals["identity_unavailable"]
	}
	id := uuidv7.New()
	s.log.Warn("identity request refused", "code", code, "correlation_id", id)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(ref.status)
	json.NewEncoder(w).Encode(identityRefusalBody{Code: code, Message: ref.message, Retryable: ref.retryable, NextAction: ref.next, CorrelationID: id})
}

// identityTLS reports whether the request arrived over TLS terminated by this
// hub. Headers such as X-Forwarded-Proto are deliberately ignored.
func identityTLS(r *http.Request) bool {
	return r.TLS != nil && r.TLS.HandshakeComplete
}

// identityWireMux holds exactly the two wire patterns, so the gate classifies
// a request by the same matching the route mux dispatches with, including its
// segment-by-segment unescaping: every spelling that reaches an identity wire
// handler (for example /v1/identity/%70roofs) is classified as that route.
var identityWireMux = func() *http.ServeMux {
	m := http.NewServeMux()
	for _, p := range []string{identityProofPattern, identityRedeemPattern} {
		m.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	return m
}()

const (
	identityProofPattern  = "POST /v1/identity/proofs"
	identityRedeemPattern = "POST /v1/identity/peers/{service_id}/redemptions"
)

// identityWireRoute names the identity.v1 wire route a request targets:
// "proof", "redeem" or "" for any other request.
func identityWireRoute(r *http.Request) string {
	if !canonicalPath(r) {
		return ""
	}
	switch _, pattern := identityWireMux.Handler(r); pattern {
	case identityProofPattern:
		return "proof"
	case identityRedeemPattern:
		return "redeem"
	}
	return ""
}

// gateAuthHook, when set by a test, sees each request just before the bearer
// gate authenticates it. It is nil in production.
var gateAuthHook func(*http.Request)

// identityUnauthenticated is the envelope code for a wire request whose bearer
// is missing, unknown, or not the kind of credential the route requires.
var identityUnauthenticated = map[string]string{"proof": "invalid_credential", "redeem": "peer_unauthenticated"}

// peerRouteAllowed is the whole surface of a peer credential: one POST shape.
// The handler checks that the path names the credential's own peer.
func peerRouteAllowed(r *http.Request) bool { return identityWireRoute(r) == "redeem" }

// identityStoreError maps a ledger error to its stable refusal code. A store
// that stayed busy past identityWait is an in-flight retry.
func identityStoreError(err error) string {
	switch {
	case errors.Is(err, access.ErrDenied):
		return "invalid_credential"
	case errors.Is(err, access.ErrCredentialScope):
		return "credential_scope_forbidden"
	case errors.Is(err, access.ErrPeerUnknown):
		return "peer_unknown"
	case errors.Is(err, access.ErrPeerUnauthenticated):
		return "peer_unauthenticated"
	case errors.Is(err, access.ErrProofInvalid):
		return "proof_invalid"
	case errors.Is(err, access.ErrCredentialInactive):
		return "credential_inactive"
	case errors.Is(err, access.ErrIdempotencyConflict):
		return "idempotency_conflict"
	case errors.Is(err, access.ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, access.ErrInvalidRequest):
		return "invalid_request"
	case errors.Is(err, context.DeadlineExceeded),
		strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked"):
		return "request_in_progress"
	}
	return "identity_unavailable"
}

func decodeIdentity(r *http.Request, w http.ResponseWriter, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return false
	}
	return dec.Decode(&struct{}{}) == io.EOF
}

// identityWireGate applies the checks shared by the two identity.v1 wire
// routes: TLS, then the protocol version.
func (s *Server) identityWireGate(w http.ResponseWriter, r *http.Request) bool {
	if !identityTLS(r) {
		s.identityRefuse(w, "tls_required")
		return false
	}
	if r.Header.Get(identityVersionHeader) != "1" {
		s.identityRefuse(w, "unsupported_version")
		return false
	}
	return true
}

func (s *Server) identityProof(w http.ResponseWriter, r *http.Request) {
	if !s.identityWireGate(w, r) {
		return
	}
	id, ok := IdentityFrom(r.Context())
	if !ok || id.Role != "user" {
		s.identityRefuse(w, "credential_scope_forbidden")
		return
	}
	var req struct {
		PeerServiceID string `json:"peer_service_id"`
		HubID         string `json:"hub_id"`
		ChallengeID   string `json:"challenge_id"`
	}
	if !decodeIdentity(r, w, &req) {
		s.identityRefuse(w, "invalid_request")
		return
	}
	db, err := s.openAccess(false)
	if err != nil {
		s.identityRefuse(w, "identity_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), identityWait)
	defer cancel()
	rec, err := db.IssueProof(ctx, id.UserID, id.TokenID, access.ProofRequest{PeerServiceID: req.PeerServiceID, HubID: req.HubID, ChallengeID: req.ChallengeID})
	if err != nil {
		s.identityRefuse(w, identityStoreError(err))
		return
	}
	type binding struct {
		HubID         string `json:"hub_id"`
		PeerServiceID string `json:"peer_service_id"`
		ChallengeID   string `json:"challenge_id"`
		UserID        string `json:"user_id"`
		TokenID       string `json:"token_id"`
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, struct {
		Receipt   string    `json:"receipt"`
		ReceiptID string    `json:"receipt_id"`
		ExpiresAt time.Time `json:"expires_at"`
		Binding   binding   `json:"binding"`
	}{rec.Receipt, rec.ID, rec.ExpiresAt, binding{rec.HubID, rec.PeerServiceID, rec.ChallengeID, rec.UserID, rec.TokenID}})
}

func (s *Server) identityRedeem(w http.ResponseWriter, r *http.Request) {
	if !s.identityWireGate(w, r) {
		return
	}
	id, ok := IdentityFrom(r.Context())
	if !ok || id.Role != "peer" {
		s.identityRefuse(w, "peer_unauthenticated")
		return
	}
	if r.PathValue("service_id") != id.Peer.ServiceID {
		s.identityRefuse(w, "peer_forbidden")
		return
	}
	var req struct {
		HubID       string `json:"hub_id"`
		ChallengeID string `json:"challenge_id"`
		Receipt     string `json:"receipt"`
	}
	if !decodeIdentity(r, w, &req) {
		s.identityRefuse(w, "invalid_request")
		return
	}
	db, err := s.openAccess(false)
	if err != nil {
		s.identityRefuse(w, "identity_unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), identityWait)
	defer cancel()
	red, err := db.RedeemProof(ctx, id.Peer, access.RedeemRequest{HubID: req.HubID, ChallengeID: req.ChallengeID,
		Receipt: req.Receipt, RequestKey: r.Header.Get("Idempotency-Key")})
	if err != nil {
		s.identityRefuse(w, identityStoreError(err))
		return
	}
	type verified struct {
		HubID   string `json:"hub_id"`
		UserID  string `json:"user_id"`
		TokenID string `json:"token_id"`
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, struct {
		RedemptionID  string    `json:"redemption_id"`
		RequestKey    string    `json:"request_key"`
		Replayed      bool      `json:"replayed"`
		PeerServiceID string    `json:"peer_service_id"`
		ChallengeID   string    `json:"challenge_id"`
		Identity      verified  `json:"identity"`
		TokenState    string    `json:"token_state"`
		RedeemedAt    time.Time `json:"redeemed_at"`
	}{red.ID, red.RequestKey, red.Replayed, red.PeerServiceID, red.ChallengeID, verified{red.HubID, red.UserID, red.TokenID}, "active", red.RedeemedAt})
}

// Peer management is hub-admin only (the route table marks it Admin) and,
// like the wire, requires TLS terminated by this hub.

type peerView struct {
	ServiceID string `json:"service_id"`
	HubID     string `json:"hub_id"`
	Operation string `json:"operation"`
	Endpoint  string `json:"introspection_endpoint"`
	TLSTrust  struct {
		Mode  string `json:"mode"`
		Value string `json:"value"`
	} `json:"tls_trust"`
	Disabled      bool `json:"disabled"`
	Introspection bool `json:"introspection_operational"`
}

type peerCredentialView struct {
	ID        string    `json:"id"`
	ServiceID string    `json:"service_id"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked   bool      `json:"revoked"`
}

func toPeerView(p access.IdentityPeer) peerView {
	v := peerView{ServiceID: p.ServiceID, HubID: p.HubID, Operation: "identity.redeem", Endpoint: p.Endpoint, Disabled: p.Disabled}
	v.TLSTrust.Mode, v.TLSTrust.Value = p.TLSMode, p.TLSValue
	return v
}

func toCredentialView(c access.PeerCredential) peerCredentialView {
	return peerCredentialView{ID: c.ID, ServiceID: c.ServiceID, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt, Revoked: c.Revoked}
}

// peerAdminStore applies the admin routes' TLS rule and opens the store,
// creating it if the hub has never enabled ordinary credentials.
func (s *Server) peerAdminStore(w http.ResponseWriter, r *http.Request) (*access.Store, bool) {
	if !identityTLS(r) {
		s.identityRefuse(w, "tls_required")
		return nil, false
	}
	db, err := s.openAccess(true)
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, fmt.Errorf("access store unavailable"))
		return nil, false
	}
	return db, true
}

func (s *Server) listIdentityPeers(w http.ResponseWriter, r *http.Request) {
	db, ok := s.peerAdminStore(w, r)
	if !ok {
		return
	}
	peers, err := db.ListIdentityPeers()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot list identity peers"))
		return
	}
	out := []peerView{}
	for _, p := range peers {
		out = append(out, toPeerView(p))
	}
	s.ok(w, map[string]any{"peers": out})
}

func (s *Server) registerIdentityPeer(w http.ResponseWriter, r *http.Request) {
	db, ok := s.peerAdminStore(w, r)
	if !ok {
		return
	}
	var req struct {
		ServiceID string `json:"service_id"`
		Endpoint  string `json:"introspection_endpoint"`
		TLSTrust  struct {
			Mode  string `json:"mode"`
			Value string `json:"value"`
		} `json:"tls_trust"`
	}
	if !decodeIdentity(r, w, &req) {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("invalid peer registration"))
		return
	}
	hub, err := db.HubID()
	if err != nil {
		s.fail(w, http.StatusServiceUnavailable, fmt.Errorf("hub identity unavailable"))
		return
	}
	p := access.IdentityPeer{ServiceID: req.ServiceID, HubID: hub, Endpoint: req.Endpoint, TLSMode: req.TLSTrust.Mode, TLSValue: req.TLSTrust.Value}
	if err := db.RegisterIdentityPeer(accessActor(r), p); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	peers, err := db.ListIdentityPeers()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot read identity peer"))
		return
	}
	for _, got := range peers {
		if got.ServiceID == req.ServiceID {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(toPeerView(got))
			return
		}
	}
	s.fail(w, http.StatusInternalServerError, fmt.Errorf("registered peer not found"))
}

func (s *Server) updateIdentityPeer(w http.ResponseWriter, r *http.Request) {
	db, ok := s.peerAdminStore(w, r)
	if !ok {
		return
	}
	var req struct {
		Disabled *bool `json:"disabled"`
	}
	if !decodeIdentity(r, w, &req) || req.Disabled == nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("body must be {\"disabled\": true|false}"))
		return
	}
	if err := db.SetIdentityPeerDisabled(accessActor(r), r.PathValue("service_id"), *req.Disabled); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.ok(w, map[string]bool{"ok": true})
}

func (s *Server) listPeerCredentials(w http.ResponseWriter, r *http.Request) {
	db, ok := s.peerAdminStore(w, r)
	if !ok {
		return
	}
	creds, err := db.ListPeerCredentials(r.PathValue("service_id"))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot list peer credentials"))
		return
	}
	out := []peerCredentialView{}
	for _, c := range creds {
		out = append(out, toCredentialView(c))
	}
	s.ok(w, map[string]any{"credentials": out})
}

// issuePeerCredential returns the bearer exactly once. A lost response is
// recovered by listing, revoking the unconfirmed credential and reissuing.
func (s *Server) issuePeerCredential(w http.ResponseWriter, r *http.Request) {
	db, ok := s.peerAdminStore(w, r)
	if !ok {
		return
	}
	var req struct {
		ExpiresAt time.Time `json:"expires_at"`
	}
	if !decodeIdentity(r, w, &req) {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("body must be {\"expires_at\": RFC 3339 time}"))
		return
	}
	cred, secret, err := db.IssuePeerCredential(accessActor(r), r.PathValue("service_id"), req.ExpiresAt)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, access.ErrPeerCredentialLimit) {
			status = http.StatusConflict
		}
		s.fail(w, status, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(struct {
		Credential peerCredentialView `json:"credential"`
		Secret     string             `json:"secret"`
	}{toCredentialView(cred), secret})
}

func (s *Server) revokePeerCredential(w http.ResponseWriter, r *http.Request) {
	db, ok := s.peerAdminStore(w, r)
	if !ok {
		return
	}
	if err := db.RevokePeerCredential(accessActor(r), r.PathValue("service_id"), r.PathValue("credential_id")); err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	s.ok(w, map[string]bool{"ok": true})
}
