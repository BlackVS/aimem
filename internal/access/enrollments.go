package access

// The D1 enrollment ledger (docs/DESIGN-AIFORGE-ENROLLMENT-WIRE.md, §1 and
// §2): the hub admin issues a bundle with a one-time subcode, lists bundles
// and revokes unredeemed ones. The subcode is returned once, at issue, and
// stored only as its SHA-256 digest. Redemption is D1-b.

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	// EnrollmentPurposeNewUser is the one purpose enrollment.v1 serves;
	// existing_user_reissue is reserved for D1-c.
	EnrollmentPurposeNewUser = "new_user"
	// EnrollmentMaxLife caps a subcode's lifetime (D1).
	EnrollmentMaxLife = 72 * time.Hour
	// enrollmentSubcodePrefix starts every subcode: aes1_ and 43 base64url
	// characters of 256 random bits.
	enrollmentSubcodePrefix = "aes1_"
)

// Enrollment states. Expired is computed from the clock at read time.
const (
	EnrollmentIssued   = "issued"
	EnrollmentRedeemed = "redeemed"
	EnrollmentRevoked  = "revoked"
	EnrollmentExpired  = "expired"
)

var (
	// ErrEnrollmentExists answers an issue repeated with the same bundle ID
	// and input: the subcode cannot be shown again.
	ErrEnrollmentExists = errors.New("enrollment_exists")
	// ErrEnrollmentRedeemed refuses revoking a redeemed bundle; the bundle it
	// names carries the user and token that redemption issued.
	ErrEnrollmentRedeemed = errors.New("enrollment_redeemed")
	// ErrEnrollmentUnknown is an unknown bundle ID.
	ErrEnrollmentUnknown = errors.New("not_found")
)

var bundleIDShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// Enrollment is a bundle's non-secret record.
type Enrollment struct {
	BundleID  string
	Purpose   string
	State     string
	CreatedAt time.Time
	ExpiresAt time.Time
	IssuedBy  string
	UserName  string
	// Redeemed is set once D1-b has redeemed the bundle.
	Redeemed *EnrollmentRedemption
}

// EnrollmentRedemption names what a redemption issued.
type EnrollmentRedemption struct {
	UserID  string
	TokenID string
	At      time.Time
}

// EnrollmentRequest is an admin's issue request.
type EnrollmentRequest struct {
	BundleID  string
	Purpose   string
	ExpiresAt time.Time
	UserName  string
}

// inputDigest fingerprints the issue input, so a repeated bundle ID with the
// same input is told apart from one with other input.
func (r EnrollmentRequest) inputDigest() string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s", r.Purpose, r.ExpiresAt.Unix(), r.UserName))
	return hex.EncodeToString(sum[:])
}

// IssueEnrollment records a new bundle and returns its subcode, shown only
// here. A repeated bundle ID is ErrEnrollmentExists for the same input and
// ErrIdempotencyConflict for other input; neither reveals a subcode.
func (s *Store) IssueEnrollment(actor string, req EnrollmentRequest) (Enrollment, string, error) {
	now := s.now()
	req.ExpiresAt = req.ExpiresAt.UTC().Truncate(time.Second)
	switch {
	case !bundleIDShape.MatchString(req.BundleID):
		return Enrollment{}, "", fmt.Errorf("%w: bundle_id must be a lowercase UUID", ErrInvalidRequest)
	case req.Purpose != EnrollmentPurposeNewUser:
		return Enrollment{}, "", fmt.Errorf("%w: purpose must be %s", ErrInvalidRequest, EnrollmentPurposeNewUser)
	case !req.ExpiresAt.After(now) || req.ExpiresAt.After(now.Add(EnrollmentMaxLife)):
		return Enrollment{}, "", fmt.Errorf("%w: expires_at must be in the future and at most 72 hours ahead", ErrInvalidRequest)
	}
	if req.UserName != "" {
		if err := validName(req.UserName); err != nil {
			return Enrollment{}, "", fmt.Errorf("%w: user_name: %v", ErrInvalidRequest, err)
		}
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return Enrollment{}, "", err
	}
	subcode := enrollmentSubcodePrefix + base64.RawURLEncoding.EncodeToString(random[:])
	e := Enrollment{BundleID: req.BundleID, Purpose: req.Purpose, State: EnrollmentIssued,
		CreatedAt: now.UTC().Truncate(time.Second), ExpiresAt: req.ExpiresAt, IssuedBy: actor, UserName: req.UserName}
	err := s.change(actor, "enrollment.issued", enrollmentSubject(e), func(tx *sql.Tx) error {
		var input string
		switch err := tx.QueryRow("SELECT input_digest FROM enrollments WHERE bundle_id=?", req.BundleID).Scan(&input); {
		case err == nil && input == req.inputDigest():
			return ErrEnrollmentExists
		case err == nil:
			return ErrIdempotencyConflict
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err := tx.Exec("INSERT INTO enrollments(bundle_id,purpose,digest,input_digest,created_at,expires_at,issued_by,user_name) VALUES(?,?,?,?,?,?,?,?)",
			e.BundleID, e.Purpose, digestHex(subcode), req.inputDigest(), e.CreatedAt.Unix(), e.ExpiresAt.Unix(), actor, e.UserName)
		return err
	})
	if err != nil {
		return Enrollment{}, "", err
	}
	return e, subcode, nil
}

// RevokeEnrollment revokes an issued bundle. A revoked or expired bundle is
// returned unchanged. A redeemed bundle is ErrEnrollmentRedeemed, audited,
// and returned with what it issued: revoking a spent code does not revoke
// its token (D1). The store serializes this with redemption.
func (s *Store) RevokeEnrollment(actor, bundleID string) (Enrollment, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Enrollment{}, err
	}
	defer tx.Rollback()
	e, err := s.enrollmentIn(tx, bundleID)
	if err != nil {
		return Enrollment{}, err
	}
	var outcome error
	switch e.State {
	case EnrollmentIssued:
		if _, err := tx.Exec("UPDATE enrollments SET revoked_at=? WHERE bundle_id=? AND revoked_at IS NULL AND redeemed_at IS NULL",
			s.now().Unix(), bundleID); err != nil {
			return Enrollment{}, err
		}
		if err := audit(tx, actor, "enrollment.revoked", enrollmentSubject(e)); err != nil {
			return Enrollment{}, err
		}
		e.State = EnrollmentRevoked
	case EnrollmentRedeemed:
		if err := audit(tx, actor, "enrollment.revoke.refused.redeemed", enrollmentSubject(e)); err != nil {
			return Enrollment{}, err
		}
		outcome = ErrEnrollmentRedeemed
	}
	if err := tx.Commit(); err != nil {
		return Enrollment{}, err
	}
	return e, outcome
}

// ListEnrollments returns the bundles, newest first, optionally of one
// state. It never returns a subcode or a digest.
func (s *Store) ListEnrollments(state string) ([]Enrollment, error) {
	switch state {
	case "", EnrollmentIssued, EnrollmentRedeemed, EnrollmentRevoked, EnrollmentExpired:
	default:
		return nil, fmt.Errorf("%w: state must be issued, redeemed, revoked or expired", ErrInvalidRequest)
	}
	rows, err := s.db.Query("SELECT " + enrollmentColumns + " FROM enrollments ORDER BY created_at DESC, bundle_id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Enrollment{}
	for rows.Next() {
		e, err := s.scanEnrollment(rows)
		if err != nil {
			return nil, err
		}
		if state == "" || e.State == state {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

const enrollmentColumns = "bundle_id,purpose,created_at,expires_at,issued_by,user_name,revoked_at,redeemed_at,redeemed_user_id,redeemed_token_id"

func (s *Store) enrollmentIn(tx *sql.Tx, bundleID string) (Enrollment, error) {
	e, err := s.scanEnrollment(tx.QueryRow("SELECT "+enrollmentColumns+" FROM enrollments WHERE bundle_id=?", bundleID))
	if errors.Is(err, sql.ErrNoRows) {
		return Enrollment{}, ErrEnrollmentUnknown
	}
	return e, err
}

func (s *Store) scanEnrollment(row interface{ Scan(...any) error }) (Enrollment, error) {
	var e Enrollment
	var created, expires int64
	var revoked, redeemed sql.NullInt64
	var user, token sql.NullString
	if err := row.Scan(&e.BundleID, &e.Purpose, &created, &expires, &e.IssuedBy, &e.UserName, &revoked, &redeemed, &user, &token); err != nil {
		return Enrollment{}, err
	}
	e.CreatedAt, e.ExpiresAt = time.Unix(created, 0).UTC(), time.Unix(expires, 0).UTC()
	switch {
	case redeemed.Valid:
		e.State = EnrollmentRedeemed
		e.Redeemed = &EnrollmentRedemption{UserID: user.String, TokenID: token.String, At: time.Unix(redeemed.Int64, 0).UTC()}
	case revoked.Valid:
		e.State = EnrollmentRevoked
	case !s.now().Before(e.ExpiresAt):
		e.State = EnrollmentExpired
	default:
		e.State = EnrollmentIssued
	}
	return e, nil
}

// enrollmentSubject is the audit subject: the bundle's non-secret fields.
func enrollmentSubject(e Enrollment) string {
	subject := fmt.Sprintf("bundle=%s purpose=%s expires_at=%s", e.BundleID, e.Purpose, e.ExpiresAt.Format(time.RFC3339))
	if e.Redeemed != nil {
		subject += fmt.Sprintf(" user=%s token=%s", e.Redeemed.UserID, e.Redeemed.TokenID)
	}
	return subject
}
