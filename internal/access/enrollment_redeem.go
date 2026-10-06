package access

// Enrollment redemption (docs/DESIGN-AIFORGE-ENROLLMENT-WIRE.md §3): one
// store transaction consumes an issued subcode, creates one user and one
// user-scoped token, seals the token to the client's key, and records the
// outcome so that an identical retry replays it. The hub serves no route for
// it yet (D1-b2).

import (
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"aimem/internal/enrollment"
	"aimem/internal/uuidv7"
)

// Redemption bounds (enrollment.v1, Bounds).
const (
	// EnrollmentDeliveryRetention is how long an identical retry replays a
	// redemption's delivery, also past the subcode's expiry.
	EnrollmentDeliveryRetention = time.Hour
	// enrolledTokenLife keeps the issued token inside the store's 366-day
	// limit for ordinary tokens.
	enrolledTokenLife = 365 * 24 * time.Hour
	// enrollmentClient is the audit actor of a redemption, which no bearer
	// authenticates.
	enrollmentClient = "enrollment-client"
)

var (
	// ErrEnrollmentInvalid is the one non-disclosing refusal for an unknown,
	// expired, revoked, wrong-hub or no-longer-replayable subcode.
	ErrEnrollmentInvalid = errors.New("enrollment_invalid")
	// ErrEnrollmentConflict refuses a redeemed subcode with another request
	// key, or its key with other input.
	ErrEnrollmentConflict = errors.New("enrollment_conflict")
)

var (
	enrollmentLabelShape = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	requestKeyShape      = regexp.MustCompile(`^k1_[A-Za-z0-9_-]{43}$`)
)

// EnrollmentRedeemRequest is a client's redemption. RequestKey is already in k1_ form;
// PublicKey is the client's X25519 key in unpadded base64url.
type EnrollmentRedeemRequest struct {
	HubID      string
	Subcode    string
	RequestKey string
	Label      string
	Suite      string
	PublicKey  string
}

// EnrollmentRedeemResult is a redemption's answer, first or replayed.
type EnrollmentRedeemResult struct {
	BundleID   string
	Replayed   bool
	HubID      string
	UserID     string
	TokenID    string
	Suite      string
	Enc        string
	Ciphertext string
	RedeemedAt time.Time
}

// inputDigest fingerprints what a replay must repeat besides the subcode and
// the request key.
func (r EnrollmentRedeemRequest) inputDigest() string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s\x00%s", r.HubID, r.Label, r.Suite, r.PublicKey))
	return hex.EncodeToString(sum[:])
}

type storedDelivery struct {
	Suite      string `json:"suite"`
	Enc        string `json:"enc"`
	Ciphertext string `json:"ciphertext"`
}

// RedeemEnrollment consumes an issued subcode, or replays its redemption for
// an identical retry within EnrollmentDeliveryRetention. Every refusal of a
// subcode reaches the caller as ErrEnrollmentInvalid or
// ErrEnrollmentConflict; the audit keeps the exact reason.
func (s *Store) RedeemEnrollment(req EnrollmentRedeemRequest) (EnrollmentRedeemResult, error) {
	switch {
	case !requestKeyShape.MatchString(req.RequestKey):
		return EnrollmentRedeemResult{}, fmt.Errorf("%w: the request key must be in k1_ form", ErrInvalidRequest)
	case !enrollmentLabelShape.MatchString(req.Label):
		return EnrollmentRedeemResult{}, fmt.Errorf("%w: label must be 1 to 64 of a-z, 0-9 and -", ErrInvalidRequest)
	case req.Suite != enrollment.Suite:
		return EnrollmentRedeemResult{}, fmt.Errorf("%w: suite must be %s", ErrInvalidRequest, enrollment.Suite)
	}
	pub, err := enrollment.ParsePublicKey(req.PublicKey)
	if err != nil {
		return EnrollmentRedeemResult{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	out, reason, err := s.redeem(req, pub)
	if reason != "" {
		// The redemption's transaction rolled back, so the refusal is
		// audited in its own. The subject never holds the subcode or key.
		if aerr := s.change(enrollmentClient, "enrollment.redeem.refused."+reason, "bundle="+out.BundleID,
			func(*sql.Tx) error { return nil }); aerr != nil {
			return EnrollmentRedeemResult{}, aerr
		}
		return EnrollmentRedeemResult{}, err
	}
	return out, err
}

// enrollmentRow is one ledger row as redemption reads it.
type enrollmentRow struct {
	bundle, purpose, userName string
	expires                   int64
	revoked, redeemed         sql.NullInt64
	user, token, key, input   sql.NullString
	delivery                  sql.NullString
}

// redeem returns the answer, or a refusal with its audit reason.
func (s *Store) redeem(req EnrollmentRedeemRequest, pub *ecdh.PublicKey) (EnrollmentRedeemResult, string, error) {
	now := s.now()
	tx, err := s.db.Begin()
	if err != nil {
		return EnrollmentRedeemResult{}, "", err
	}
	defer tx.Rollback()
	var hub string
	if err := tx.QueryRow("SELECT id FROM hub_identity WHERE singleton=1").Scan(&hub); err != nil {
		return EnrollmentRedeemResult{}, "", err
	}
	var r enrollmentRow
	err = tx.QueryRow(`SELECT bundle_id,purpose,user_name,expires_at,revoked_at,redeemed_at,redeemed_user_id,
 redeemed_token_id,request_key,redeem_input_digest,delivery FROM enrollments WHERE digest=?`, digestHex(req.Subcode)).Scan(
		&r.bundle, &r.purpose, &r.userName, &r.expires, &r.revoked, &r.redeemed, &r.user, &r.token, &r.key, &r.input, &r.delivery)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return EnrollmentRedeemResult{}, "unknown", ErrEnrollmentInvalid
	case err != nil:
		return EnrollmentRedeemResult{}, "", err
	case req.HubID != hub:
		return EnrollmentRedeemResult{BundleID: r.bundle}, "wrong_hub", ErrEnrollmentInvalid
	case r.redeemed.Valid:
		return s.replay(tx, now, hub, req, r)
	case r.revoked.Valid:
		return EnrollmentRedeemResult{BundleID: r.bundle}, "revoked", ErrEnrollmentInvalid
	case !now.Before(time.Unix(r.expires, 0)):
		return EnrollmentRedeemResult{BundleID: r.bundle}, "expired", ErrEnrollmentInvalid
	case r.purpose != EnrollmentPurposeNewUser:
		return EnrollmentRedeemResult{BundleID: r.bundle}, "purpose", ErrEnrollmentInvalid
	}
	name := r.userName
	if name == "" {
		name = req.Label
	}
	userID, tokenID := uuidv7.New(), uuidv7.New()
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return EnrollmentRedeemResult{}, "", err
	}
	bearer := "aimem_user_" + hex.EncodeToString(random[:])
	at := now.UTC().Truncate(time.Second)
	if _, err := tx.Exec("INSERT INTO users(id,name,disabled) VALUES(?,?,0)", userID, name); err != nil {
		return EnrollmentRedeemResult{}, "", err
	}
	if _, err := tx.Exec("INSERT INTO tokens(id,user_id,label,project,digest,expires_at,scope) VALUES(?,?,?,'',?,?,?)",
		tokenID, userID, req.Label, digestHex(bearer), at.Add(enrolledTokenLife).Unix(), ScopeUser); err != nil {
		return EnrollmentRedeemResult{}, "", err
	}
	enc, ct, err := enrollment.Seal(req.Suite, pub, enrollment.AAD(hub, r.bundle, req.RequestKey, userID, tokenID),
		enrollment.Plaintext{Token: bearer, UserID: userID, TokenID: tokenID, HubID: hub})
	if err != nil {
		return EnrollmentRedeemResult{}, "", err
	}
	stored, err := json.Marshal(storedDelivery{Suite: req.Suite, Enc: enc, Ciphertext: ct})
	if err != nil {
		return EnrollmentRedeemResult{}, "", err
	}
	res, err := tx.Exec(`UPDATE enrollments SET redeemed_at=?,redeemed_user_id=?,redeemed_token_id=?,request_key=?,
 redeem_input_digest=?,delivery=? WHERE bundle_id=? AND redeemed_at IS NULL AND revoked_at IS NULL`,
		at.Unix(), userID, tokenID, req.RequestKey, req.inputDigest(), string(stored), r.bundle)
	if err != nil {
		return EnrollmentRedeemResult{}, "", err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return EnrollmentRedeemResult{BundleID: r.bundle}, "raced", ErrEnrollmentInvalid
	}
	if err := audit(tx, enrollmentClient, "enrollment.redeemed", redeemSubject(r.bundle, userID, tokenID, req.Label)); err != nil {
		return EnrollmentRedeemResult{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return EnrollmentRedeemResult{}, "", err
	}
	return EnrollmentRedeemResult{BundleID: r.bundle, HubID: hub, UserID: userID, TokenID: tokenID, Suite: req.Suite,
		Enc: enc, Ciphertext: ct, RedeemedAt: at}, "", nil
}

// replay answers a redeemed bundle: the stored delivery for the identical
// request within the retention, while the token it delivers is still live.
func (s *Store) replay(tx *sql.Tx, now time.Time, hub string, req EnrollmentRedeemRequest, r enrollmentRow) (EnrollmentRedeemResult, string, error) {
	if r.key.String != req.RequestKey || r.input.String != req.inputDigest() {
		return EnrollmentRedeemResult{BundleID: r.bundle}, "conflict", ErrEnrollmentConflict
	}
	at := time.Unix(r.redeemed.Int64, 0).UTC()
	if !now.Before(at.Add(EnrollmentDeliveryRetention)) {
		return EnrollmentRedeemResult{BundleID: r.bundle}, "retention_expired", ErrEnrollmentInvalid
	}
	var revoked, disabled bool
	var expires int64
	err := tx.QueryRow("SELECT t.revoked,t.expires_at,u.disabled FROM tokens t JOIN users u ON u.id=t.user_id WHERE t.id=?",
		r.token.String).Scan(&revoked, &expires, &disabled)
	if err != nil || revoked || disabled || !now.Before(time.Unix(expires, 0)) {
		return EnrollmentRedeemResult{BundleID: r.bundle}, "credential_inactive", ErrEnrollmentInvalid
	}
	var d storedDelivery
	if err := json.Unmarshal([]byte(r.delivery.String), &d); err != nil {
		return EnrollmentRedeemResult{}, "", err
	}
	if err := audit(tx, enrollmentClient, "enrollment.redeem.replayed", redeemSubject(r.bundle, r.user.String, r.token.String, req.Label)); err != nil {
		return EnrollmentRedeemResult{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return EnrollmentRedeemResult{}, "", err
	}
	return EnrollmentRedeemResult{BundleID: r.bundle, Replayed: true, HubID: hub, UserID: r.user.String, TokenID: r.token.String,
		Suite: d.Suite, Enc: d.Enc, Ciphertext: d.Ciphertext, RedeemedAt: at}, "", nil
}

func redeemSubject(bundle, user, token, label string) string {
	return fmt.Sprintf("bundle=%s user=%s token=%s label=%s", bundle, user, token, label)
}
