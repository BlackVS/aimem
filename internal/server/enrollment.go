package server

// enrollment.v1's admin routes (docs/DESIGN-AIFORGE-ENROLLMENT-WIRE.md §1
// and §2): the hub admin issues, lists and revokes enrollment bundles over
// TLS the hub terminates itself. Redemption (§3) is D1-b; its route is not
// served yet.

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"aimem/internal/access"
	"aimem/internal/uuidv7"
)

const enrollmentVersionHeader = "X-Aimem-Enrollment-Version"

const (
	enrollIssuePattern  = "POST /v1/identity/enrollments"
	enrollListPattern   = "GET /v1/identity/enrollments"
	enrollRevokePattern = "POST /v1/identity/enrollments/{bundle_id}/revocation"
)

// enrollmentMux recognizes the enrollment routes with the router's own
// pattern matching, so an encoded spelling cannot slip past the gate.
var enrollmentMux = func() *http.ServeMux {
	m := http.NewServeMux()
	for _, p := range []string{enrollIssuePattern, enrollListPattern, enrollRevokePattern} {
		m.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	return m
}()

// enrollmentRoute reports whether r is one of the enrollment admin routes.
func enrollmentRoute(r *http.Request) bool {
	if !canonicalPath(r) {
		return false
	}
	switch _, pattern := enrollmentMux.Handler(r); pattern {
	case enrollIssuePattern, enrollListPattern, enrollRevokePattern:
		return true
	}
	return false
}

// enrollmentRefusals is enrollment.v1's refusal table for the admin routes.
var enrollmentRefusals = map[string]struct {
	status  int
	message string
	next    string
}{
	"invalid_request":            {400, "The request is malformed.", "Correct the request."},
	"unsupported_version":        {400, "This enrollment version is not supported.", "Use enrollment version 1."},
	"tls_required":               {403, "This route requires TLS terminated by the hub.", "Connect to the hub's TLS listener with certificate verification."},
	"credential_scope_forbidden": {403, "This route needs the hub admin credential.", "Use the hub admin credential."},
	"peer_forbidden":             {403, "A peer credential cannot manage enrollments.", "Use the hub admin credential."},
	"not_found":                  {404, "No enrollment bundle has this ID.", "Check the bundle ID with aimem identity enroll list."},
	"enrollment_exists":          {409, "This bundle was already issued; its subcode cannot be shown again.", "Revoke the bundle and issue a new one; its subcode cannot be shown again."},
	"enrollment_redeemed":        {409, "This bundle was redeemed; revoking it does not revoke what it issued.", "Revoke the issued token and disable the user if the redemption was not legitimate."},
	"idempotency_conflict":       {409, "This bundle ID was used with other input.", "Use a new bundle ID for other input."},
	"enrollment_unavailable":     {503, "Enrollment storage is unavailable.", "Retry with the same input; nothing was applied."},
}

// enrollmentRefusal is the context contract's envelope, with the issued
// identity when a redeemed bundle refuses revocation.
type enrollmentRefusal struct {
	identityRefusalBody
	Redeemed *enrollmentIdentity `json:"redeemed,omitempty"`
}

type enrollmentIdentity struct {
	UserID  string `json:"user_id"`
	TokenID string `json:"token_id"`
}

func (s *Server) enrollRefuse(w http.ResponseWriter, code string, redeemed *enrollmentIdentity) {
	ref, ok := enrollmentRefusals[code]
	if !ok {
		code, ref = "enrollment_unavailable", enrollmentRefusals["enrollment_unavailable"]
	}
	cid := uuidv7.New()
	s.log.Warn("enrollment request refused", "code", code, "correlation_id", cid)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(ref.status)
	json.NewEncoder(w).Encode(enrollmentRefusal{identityRefusalBody: identityRefusalBody{Code: code, Message: ref.message,
		Retryable: ref.status == http.StatusServiceUnavailable, NextAction: ref.next, CorrelationID: cid}, Redeemed: redeemed})
}

// enrollmentGateRefuse answers a non-admin bearer on an enrollment route
// before any other gate or handler sees it: tls_required first, then the
// credential's kind. It reports whether it answered.
func (s *Server) enrollmentGateRefuse(w http.ResponseWriter, r *http.Request, id Identity) bool {
	if !enrollmentRoute(r) {
		return false
	}
	switch {
	case !identityTLS(r):
		s.enrollRefuse(w, "tls_required", nil)
	case id.Role == "admin":
		return false
	case id.Role == "peer":
		s.enrollRefuse(w, "peer_forbidden", nil)
	default:
		s.enrollRefuse(w, "credential_scope_forbidden", nil)
	}
	return true
}

// enrollmentAdminStore applies the route checks the gate leaves to the
// handler: TLS (the local socket has no gate), then the version.
func (s *Server) enrollmentAdminStore(w http.ResponseWriter, r *http.Request) (*access.Store, bool) {
	if !identityTLS(r) {
		s.enrollRefuse(w, "tls_required", nil)
		return nil, false
	}
	if r.Header.Get(enrollmentVersionHeader) != "1" {
		s.enrollRefuse(w, "unsupported_version", nil)
		return nil, false
	}
	db, err := s.openAccess(true)
	if err != nil {
		s.enrollRefuse(w, "enrollment_unavailable", nil)
		return nil, false
	}
	return db, true
}

func enrollmentStoreCode(err error) string {
	switch {
	case errors.Is(err, access.ErrInvalidRequest):
		return "invalid_request"
	case errors.Is(err, access.ErrEnrollmentExists):
		return "enrollment_exists"
	case errors.Is(err, access.ErrEnrollmentRedeemed):
		return "enrollment_redeemed"
	case errors.Is(err, access.ErrEnrollmentUnknown):
		return "not_found"
	case errors.Is(err, access.ErrIdempotencyConflict):
		return "idempotency_conflict"
	}
	return "enrollment_unavailable"
}

type enrollmentView struct {
	BundleID  string                 `json:"bundle_id"`
	Purpose   string                 `json:"purpose"`
	State     string                 `json:"state"`
	CreatedAt time.Time              `json:"created_at"`
	ExpiresAt time.Time              `json:"expires_at"`
	IssuedBy  string                 `json:"issued_by"`
	UserName  string                 `json:"user_name"`
	Redeemed  *enrollmentRedemptionV `json:"redeemed"`
}

type enrollmentRedemptionV struct {
	UserID  string    `json:"user_id"`
	TokenID string    `json:"token_id"`
	At      time.Time `json:"at"`
}

func toEnrollmentView(e access.Enrollment) enrollmentView {
	v := enrollmentView{BundleID: e.BundleID, Purpose: e.Purpose, State: e.State, CreatedAt: e.CreatedAt,
		ExpiresAt: e.ExpiresAt, IssuedBy: e.IssuedBy, UserName: e.UserName}
	if e.Redeemed != nil {
		v.Redeemed = &enrollmentRedemptionV{UserID: e.Redeemed.UserID, TokenID: e.Redeemed.TokenID, At: e.Redeemed.At}
	}
	return v
}

// issueEnrollment returns the subcode exactly once (no-store). A lost reply
// is recovered by revoking the bundle, whose ID the caller chose.
func (s *Server) issueEnrollment(w http.ResponseWriter, r *http.Request) {
	db, ok := s.enrollmentAdminStore(w, r)
	if !ok {
		return
	}
	var req struct {
		BundleID  string    `json:"bundle_id"`
		Purpose   string    `json:"purpose"`
		ExpiresAt time.Time `json:"expires_at"`
		UserName  string    `json:"user_name"`
	}
	if !decodeIdentity(r, w, &req) {
		s.enrollRefuse(w, "invalid_request", nil)
		return
	}
	hub, err := db.HubID()
	if err != nil {
		s.enrollRefuse(w, "enrollment_unavailable", nil)
		return
	}
	e, subcode, err := db.IssueEnrollment(accessActor(r), access.EnrollmentRequest{BundleID: req.BundleID, Purpose: req.Purpose,
		ExpiresAt: req.ExpiresAt, UserName: req.UserName})
	if err != nil {
		s.enrollRefuse(w, enrollmentStoreCode(err), nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(struct {
		BundleID  string    `json:"bundle_id"`
		Purpose   string    `json:"purpose"`
		ExpiresAt time.Time `json:"expires_at"`
		HubID     string    `json:"hub_id"`
		Subcode   string    `json:"subcode"`
	}{e.BundleID, e.Purpose, e.ExpiresAt, hub, subcode})
}

func (s *Server) listEnrollments(w http.ResponseWriter, r *http.Request) {
	db, ok := s.enrollmentAdminStore(w, r)
	if !ok {
		return
	}
	list, err := db.ListEnrollments(r.URL.Query().Get("state"))
	if err != nil {
		s.enrollRefuse(w, enrollmentStoreCode(err), nil)
		return
	}
	out := []enrollmentView{}
	for _, e := range list {
		out = append(out, toEnrollmentView(e))
	}
	s.ok(w, map[string]any{"enrollments": out})
}

func (s *Server) revokeEnrollment(w http.ResponseWriter, r *http.Request) {
	db, ok := s.enrollmentAdminStore(w, r)
	if !ok {
		return
	}
	e, err := db.RevokeEnrollment(accessActor(r), r.PathValue("bundle_id"))
	if err != nil {
		var issued *enrollmentIdentity
		if errors.Is(err, access.ErrEnrollmentRedeemed) && e.Redeemed != nil {
			issued = &enrollmentIdentity{UserID: e.Redeemed.UserID, TokenID: e.Redeemed.TokenID}
		}
		s.enrollRefuse(w, enrollmentStoreCode(err), issued)
		return
	}
	s.ok(w, map[string]string{"bundle_id": e.BundleID, "state": e.State})
}
