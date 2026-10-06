package server

// enrollment.v1 §3: the redemption route. It takes no bearer, because the
// subcode is the authority; the bearer gate does not evaluate one here.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"aimem/internal/access"
	"aimem/internal/enrollment"
)

const enrollRedeemPattern = "POST /v1/identity/enrollments/redemptions"

// enrollRedeemBodyMax is enrollment.v1's bound on a redemption body.
const enrollRedeemBodyMax = 4096

var enrollRedeemMux = func() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc(enrollRedeemPattern, func(http.ResponseWriter, *http.Request) {})
	return m
}()

// bearerFreeRoute reports whether r is a route the bearer gate passes
// without evaluating any bearer: only enrollment redemption, whose authority
// is the subcode in its body.
func bearerFreeRoute(r *http.Request) bool {
	if !canonicalPath(r) {
		return false
	}
	_, pattern := enrollRedeemMux.Handler(r)
	return pattern == enrollRedeemPattern
}

// redeemLimits are the route's two bounds, one per client address and one
// per subcode (enrollment.v1, Bounds).
type redeemLimits struct {
	once             sync.Once
	address, subcode *enrollment.Limiter
}

func (l *redeemLimits) get() (*enrollment.Limiter, *enrollment.Limiter) {
	l.once.Do(func() {
		l.address = enrollment.NewLimiter(enrollment.RedemptionsPerAddress, enrollment.RateWindow)
		l.subcode = enrollment.NewLimiter(enrollment.RedemptionsPerBundle, enrollment.RateWindow)
	})
	return l.address, l.subcode
}

// redeemEnrollment serves a redemption: TLS, version, request key and body
// checks, the two rate limits, then the ledger transaction.
func (s *Server) redeemEnrollment(w http.ResponseWriter, r *http.Request) {
	if !identityTLS(r) {
		s.enrollRefuse(w, "tls_required", nil)
		return
	}
	if r.Header.Get(enrollmentVersionHeader) != "1" {
		s.enrollRefuse(w, "unsupported_version", nil)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	var req struct {
		HubID    string `json:"hub_id"`
		Subcode  string `json:"subcode"`
		Label    string `json:"label"`
		Delivery struct {
			Suite     string `json:"suite"`
			PublicKey string `json:"public_key"`
		} `json:"delivery"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, enrollRedeemBodyMax))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() || req.Subcode == "" {
		s.enrollRefuse(w, "invalid_request", nil)
		return
	}
	byAddress, bySubcode := s.redeemLimits.get()
	now := time.Now()
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	sum := sha256.Sum256([]byte(req.Subcode))
	if !byAddress.Allow(host, now) || !bySubcode.Allow(hex.EncodeToString(sum[:]), now) {
		s.enrollRefuse(w, "rate_limited", nil)
		return
	}
	db, err := s.openAccess(true)
	if err != nil {
		s.enrollRefuse(w, "enrollment_unavailable", nil)
		return
	}
	out, err := db.RedeemEnrollment(access.EnrollmentRedeemRequest{HubID: req.HubID, Subcode: req.Subcode, RequestKey: key,
		Label: req.Label, Suite: req.Delivery.Suite, PublicKey: req.Delivery.PublicKey})
	if err != nil {
		code := "enrollment_unavailable"
		switch {
		case errors.Is(err, access.ErrInvalidRequest):
			code = "invalid_request"
		case errors.Is(err, access.ErrEnrollmentInvalid):
			code = "enrollment_invalid"
		case errors.Is(err, access.ErrEnrollmentConflict):
			code = "enrollment_conflict"
		}
		s.enrollRefuse(w, code, nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, map[string]any{
		"bundle_id": out.BundleID, "replayed": out.Replayed, "hub_id": out.HubID,
		"identity":    map[string]string{"user_id": out.UserID, "token_id": out.TokenID},
		"delivery":    map[string]string{"suite": out.Suite, "enc": out.Enc, "ciphertext": out.Ciphertext},
		"redeemed_at": out.RedeemedAt.Format(time.RFC3339),
	})
}
