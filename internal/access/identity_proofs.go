package access

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"aimem/internal/uuidv7"
)

// identity.v1 proof ledger (docs/DESIGN-AIFORGE-IDENTITY-WIRE.md): the
// registered aicrew peer, its credentials, proof receipts and redemptions.
//
// The hub's identity routes (internal/server/identity.go) are the only
// callers; no MCP tool reaches this ledger. The introspection client itself
// lives in internal/introspect and reads its credential from a file, never
// from this store. Every secret (peer bearer, receipt) is returned once to
// its creator and stored only as a SHA-256 digest; audit subjects carry IDs,
// never secrets.

const (
	peerOperationRedeem       = "identity.redeem"
	peerCredentialPrefix      = "aimem_peer_"
	peerCredentialMaxLife     = 366 * 24 * time.Hour
	peerCredentialMaxActive   = 2
	receiptPrefix             = "amr1_"
	receiptLifetime           = 60 * time.Second
	receiptsPerTokenPerMinute = 10
	redemptionRetention       = 15 * time.Minute
)

var (
	ErrInvalidRequest       = errors.New("invalid_request")
	ErrCredentialScope      = errors.New("credential_scope_forbidden")
	ErrPeerUnknown          = errors.New("peer_unknown")
	ErrPeerUnauthenticated  = errors.New("peer_unauthenticated")
	ErrPeerCredentialLimit  = errors.New("peer already has the maximum number of active credentials")
	ErrProofInvalid         = errors.New("proof_invalid")
	ErrCredentialInactive   = errors.New("credential_inactive")
	ErrIdempotencyConflict  = errors.New("idempotency_conflict")
	ErrRateLimited          = errors.New("rate_limited")
	identityIDPattern       = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	identityRequestKeyShape = regexp.MustCompile(`^k1_[A-Za-z0-9_-]{43}$`)
	identityReceiptShape    = regexp.MustCompile(`^amr1_[A-Za-z0-9_-]{43}$`)
)

type IdentityPeer struct {
	ServiceID string
	HubID     string
	Endpoint  string // the full URL of aicrew's introspection route
	TLSMode   string // ca_dns or spki_sha256
	TLSValue  string
	Disabled  bool
}

type PeerCredential struct {
	ID        string
	ServiceID string
	CreatedAt time.Time
	ExpiresAt time.Time
	Revoked   bool
}

// PeerIdentity is what an authenticated peer bearer proves: one registered
// service, through one credential, for identity.redeem only.
type PeerIdentity struct {
	ServiceID    string
	CredentialID string
}

type ProofRequest struct {
	PeerServiceID string
	HubID         string
	ChallengeID   string
}

type ProofReceipt struct {
	Receipt       string // the secret; returned once, never stored
	ID            string
	ExpiresAt     time.Time
	HubID         string
	PeerServiceID string
	ChallengeID   string
	UserID        string
	TokenID       string
}

type RedeemRequest struct {
	HubID       string
	ChallengeID string
	Receipt     string
	RequestKey  string // k1_ encoding of aicrew's request key
}

type Redemption struct {
	ID            string
	RequestKey    string
	Replayed      bool
	PeerServiceID string
	ChallengeID   string
	HubID         string
	UserID        string
	TokenID       string
	RedeemedAt    time.Time
}

func digestHex(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func validateTLSTrust(endpoint *url.URL, mode, value string) error {
	switch mode {
	case "ca_dns":
		if value != endpoint.Hostname() {
			return fmt.Errorf("ca_dns trust must name the endpoint host")
		}
	case "spki_sha256":
		pin, ok := strings.CutPrefix(value, "sha256-")
		if raw, err := base64.StdEncoding.DecodeString(pin); !ok || err != nil || len(raw) != sha256.Size {
			return fmt.Errorf("spki_sha256 trust must be sha256- followed by a base64 SHA-256")
		}
	default:
		return fmt.Errorf("TLS trust mode must be ca_dns or spki_sha256")
	}
	return nil
}

// RegisterIdentityPeer records the aicrew service allowed to redeem receipts
// on this hub. The pilot allows one active peer per hub. The endpoint and
// trust binding are what the introspection client calls and verifies.
func (s *Store) RegisterIdentityPeer(actor string, p IdentityPeer) error {
	if !identityIDPattern.MatchString(p.ServiceID) {
		return fmt.Errorf("%w: service ID", ErrInvalidRequest)
	}
	u, err := url.Parse(p.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" {
		return fmt.Errorf("%w: endpoint must be an https URL without credentials, query or fragment", ErrInvalidRequest)
	}
	if err := validateTLSTrust(u, p.TLSMode, p.TLSValue); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	hub, err := s.HubID()
	if err != nil {
		return err
	}
	if p.HubID != hub {
		return fmt.Errorf("%w: peer must be registered for this hub", ErrInvalidRequest)
	}
	return s.change(actor, "identity_peer.register", p.ServiceID, func(tx *sql.Tx) error {
		if err := requireNoOtherActivePeer(tx, p.ServiceID); err != nil {
			return err
		}
		_, err := tx.Exec("INSERT INTO identity_peers(service_id,hub_id,operation,endpoint,tls_mode,tls_value) VALUES(?,?,?,?,?,?)",
			p.ServiceID, p.HubID, peerOperationRedeem, u.String(), p.TLSMode, p.TLSValue)
		return err
	})
}

func requireNoOtherActivePeer(tx *sql.Tx, serviceID string) error {
	var n int
	if err := tx.QueryRow("SELECT count(*) FROM identity_peers WHERE disabled=0 AND service_id<>?", serviceID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("another active identity peer is registered on this hub")
	}
	return nil
}

// SetIdentityPeerDisabled disables a peer, which refuses all its credentials,
// or re-enables it under the one-active-peer rule.
func (s *Store) SetIdentityPeerDisabled(actor, serviceID string, disabled bool) error {
	return s.change(actor, fmt.Sprintf("identity_peer.disabled.%t", disabled), serviceID, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow("SELECT count(*) FROM identity_peers WHERE service_id=?", serviceID).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("unknown identity peer")
		}
		if !disabled {
			if err := requireNoOtherActivePeer(tx, serviceID); err != nil {
				return err
			}
		}
		_, err := tx.Exec("UPDATE identity_peers SET disabled=? WHERE service_id=?", disabled, serviceID)
		return err
	})
}

// IssuePeerCredential mints one peer bearer, shown only in this return value.
// A lost response is recovered by revoking the unconfirmed credential (its
// metadata is listed) and issuing another; the secret is never shown again.
func (s *Store) IssuePeerCredential(actor, serviceID string, expires time.Time) (PeerCredential, string, error) {
	now := s.now()
	if !expires.After(now) || expires.After(now.Add(peerCredentialMaxLife)) {
		return PeerCredential{}, "", fmt.Errorf("%w: expiry must be in the future and within 366 days", ErrInvalidRequest)
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return PeerCredential{}, "", err
	}
	secret := peerCredentialPrefix + hex.EncodeToString(random[:])
	c := PeerCredential{ID: uuidv7.New(), ServiceID: serviceID, CreatedAt: now.UTC().Truncate(time.Second), ExpiresAt: expires.UTC().Truncate(time.Second)}
	err := s.change(actor, "identity_peer.credential.issue", c.ID, func(tx *sql.Tx) error {
		var disabled bool
		if err := tx.QueryRow("SELECT disabled FROM identity_peers WHERE service_id=?", serviceID).Scan(&disabled); err != nil {
			return fmt.Errorf("unknown identity peer: %w", err)
		}
		if disabled {
			return fmt.Errorf("identity peer is disabled")
		}
		var active int
		if err := tx.QueryRow("SELECT count(*) FROM identity_peer_credentials WHERE service_id=? AND revoked=0 AND expires_at>?",
			serviceID, s.now().Unix()).Scan(&active); err != nil {
			return err
		}
		if active >= peerCredentialMaxActive {
			return ErrPeerCredentialLimit
		}
		_, err := tx.Exec("INSERT INTO identity_peer_credentials(id,service_id,digest,created_at,expires_at) VALUES(?,?,?,?,?)",
			c.ID, serviceID, digestHex(secret), c.CreatedAt.Unix(), c.ExpiresAt.Unix())
		return err
	})
	if err != nil {
		return PeerCredential{}, "", err
	}
	return c, secret, nil
}

// RevokePeerCredential affects only that credential of that peer and is
// idempotent.
func (s *Store) RevokePeerCredential(actor, serviceID, id string) error {
	return s.change(actor, "identity_peer.credential.revoke", id, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRow("SELECT count(*) FROM identity_peer_credentials WHERE id=? AND service_id=?", id, serviceID).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("unknown peer credential")
		}
		_, err := tx.Exec("UPDATE identity_peer_credentials SET revoked=1 WHERE id=?", id)
		return err
	})
}

// RecordIdentityPeerCheck audits one operator check of a peer's
// introspection. The outcome is a fixed word; the check's handle and the
// credential are never recorded.
func (s *Store) RecordIdentityPeerCheck(actor, serviceID, outcome string) error {
	return s.change(actor, "identity_peer.check."+outcome, serviceID, func(*sql.Tx) error { return nil })
}

// ListIdentityPeers returns the registered peers' non-secret records.
func (s *Store) ListIdentityPeers() ([]IdentityPeer, error) {
	rows, err := s.db.Query("SELECT service_id,hub_id,endpoint,tls_mode,tls_value,disabled FROM identity_peers ORDER BY service_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IdentityPeer
	for rows.Next() {
		var p IdentityPeer
		if err := rows.Scan(&p.ServiceID, &p.HubID, &p.Endpoint, &p.TLSMode, &p.TLSValue, &p.Disabled); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListPeerCredentials returns metadata only, for lost-response recovery and
// rotation; no digest or secret is exposed.
func (s *Store) ListPeerCredentials(serviceID string) ([]PeerCredential, error) {
	rows, err := s.db.Query("SELECT id,service_id,created_at,expires_at,revoked FROM identity_peer_credentials WHERE service_id=? ORDER BY id", serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeerCredential
	for rows.Next() {
		var c PeerCredential
		var created, expires int64
		if err := rows.Scan(&c.ID, &c.ServiceID, &created, &expires, &c.Revoked); err != nil {
			return nil, err
		}
		c.CreatedAt, c.ExpiresAt = time.Unix(created, 0).UTC(), time.Unix(expires, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) lookupPeer(ctx context.Context, secret string) (PeerIdentity, error) {
	if !strings.HasPrefix(secret, peerCredentialPrefix) {
		return PeerIdentity{}, ErrPeerUnauthenticated
	}
	var id PeerIdentity
	err := s.db.QueryRowContext(ctx, `SELECT p.service_id,c.id FROM identity_peer_credentials c
JOIN identity_peers p ON p.service_id=c.service_id
JOIN hub_identity h ON h.id=p.hub_id
WHERE c.digest=? AND c.revoked=0 AND c.expires_at>? AND p.disabled=0 AND p.operation=?`,
		digestHex(secret), s.now().Unix(), peerOperationRedeem).Scan(&id.ServiceID, &id.CredentialID)
	if errors.Is(err, sql.ErrNoRows) {
		return PeerIdentity{}, ErrPeerUnauthenticated
	}
	return id, err
}

// AuthenticatePeer checks a presented peer bearer. Expired, revoked and
// disabled-peer credentials are refused as if unknown.
func (s *Store) AuthenticatePeer(secret string) (PeerIdentity, error) {
	return s.lookupPeer(context.Background(), secret)
}

// AuthenticatePeerContext is AuthenticatePeer with the wait for the store
// bounded by ctx; on expiry it returns ctx.Err().
func (s *Store) AuthenticatePeerContext(ctx context.Context, secret string) (PeerIdentity, error) {
	return s.lookupPeer(ctx, secret)
}

func peerStillValid(tx *sql.Tx, p PeerIdentity, now time.Time) error {
	var n int
	err := tx.QueryRow(`SELECT count(*) FROM identity_peer_credentials c
JOIN identity_peers p ON p.service_id=c.service_id
JOIN hub_identity h ON h.id=p.hub_id
WHERE c.id=? AND c.service_id=? AND c.revoked=0 AND c.expires_at>? AND p.disabled=0 AND p.operation=?`,
		p.CredentialID, p.ServiceID, now.Unix(), peerOperationRedeem).Scan(&n)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrPeerUnauthenticated
	}
	return nil
}

// userTokenState reports whether an individual token is live and whether it
// is an installation (user-scoped, projectless) credential.
func userTokenState(tx *sql.Tx, userID, tokenID string, now time.Time) (live, userScoped bool, err error) {
	var scope, project string
	err = tx.QueryRow(`SELECT t.scope,t.project FROM tokens t JOIN users u ON u.id=t.user_id
WHERE t.id=? AND u.id=? AND u.disabled=0 AND t.revoked=0 AND t.expires_at>?`, tokenID, userID, now.Unix()).Scan(&scope, &project)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	return true, scope == string(ScopeUser) && project == "", nil
}

func pruneIdentityLedger(tx *sql.Tx, now time.Time) error {
	cutoff := now.Add(-redemptionRetention).UnixMilli()
	if _, err := tx.Exec("DELETE FROM identity_redemptions WHERE redeemed_ms<?", cutoff); err != nil {
		return err
	}
	// A receipt always expires after its redemption, so it outlives it here.
	_, err := tx.Exec("DELETE FROM identity_receipts WHERE expires_ms<?", cutoff)
	return err
}

// IssueProof mints a single-use receipt for the authenticated individual
// credential (userID, tokenID from authentication; both are rechecked here).
//
// The wait for the store is bounded by ctx: when it expires before the
// transaction starts, the call returns ctx.Err() and changes nothing.
func (s *Store) IssueProof(ctx context.Context, userID, tokenID string, req ProofRequest) (ProofReceipt, error) {
	if !identityIDPattern.MatchString(req.ChallengeID) || !identityIDPattern.MatchString(req.PeerServiceID) {
		return ProofReceipt{}, ErrInvalidRequest
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ProofReceipt{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProofReceipt{}, err
	}
	defer tx.Rollback()
	// Validation time is taken only once the transaction holds the store: a
	// wait in Begin must not let a credential expire unnoticed.
	now := s.now()
	r := ProofReceipt{
		Receipt: receiptPrefix + base64.RawURLEncoding.EncodeToString(random[:]), ID: uuidv7.New(),
		ExpiresAt: now.Add(receiptLifetime).UTC(), HubID: req.HubID, PeerServiceID: req.PeerServiceID,
		ChallengeID: req.ChallengeID, UserID: userID, TokenID: tokenID,
	}
	live, userScoped, err := userTokenState(tx, userID, tokenID, now)
	if err != nil {
		return ProofReceipt{}, err
	}
	if !live {
		return ProofReceipt{}, ErrDenied
	}
	if !userScoped {
		return ProofReceipt{}, ErrCredentialScope
	}
	var peers int
	if err := tx.QueryRow(`SELECT count(*) FROM identity_peers p JOIN hub_identity h ON h.id=p.hub_id
WHERE p.service_id=? AND p.hub_id=? AND p.disabled=0 AND p.operation=?`, req.PeerServiceID, req.HubID, peerOperationRedeem).Scan(&peers); err != nil {
		return ProofReceipt{}, err
	}
	if peers != 1 {
		return ProofReceipt{}, ErrPeerUnknown
	}
	var recent int
	if err := tx.QueryRow("SELECT count(*) FROM identity_receipts WHERE token_id=? AND created_ms>?",
		tokenID, now.Add(-time.Minute).UnixMilli()).Scan(&recent); err != nil {
		return ProofReceipt{}, err
	}
	if recent >= receiptsPerTokenPerMinute {
		return ProofReceipt{}, ErrRateLimited
	}
	if err := pruneIdentityLedger(tx, now); err != nil {
		return ProofReceipt{}, err
	}
	if _, err := tx.Exec("UPDATE identity_receipts SET state='superseded' WHERE token_id=? AND service_id=? AND challenge_id=? AND state='live'",
		tokenID, req.PeerServiceID, req.ChallengeID); err != nil {
		return ProofReceipt{}, err
	}
	if _, err := tx.Exec(`INSERT INTO identity_receipts(id,digest,service_id,hub_id,challenge_id,user_id,token_id,created_ms,expires_ms,state)
VALUES(?,?,?,?,?,?,?,?,?,'live')`, r.ID, digestHex(r.Receipt), r.PeerServiceID, r.HubID, r.ChallengeID, userID, tokenID,
		now.UnixMilli(), r.ExpiresAt.UnixMilli()); err != nil {
		return ProofReceipt{}, err
	}
	if err := audit(tx, "user:"+userID, "identity.proof.issue", r.ID); err != nil {
		return ProofReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProofReceipt{}, err
	}
	return r, nil
}

// RedeemProof consumes a receipt for the authenticated peer. It is single
// effect per (peer, request key): an identical retry returns the recorded
// result after rechecking the token, changed input under the key conflicts,
// and a receipt consumed under another key is proof_invalid. Every refusal
// is audited with its exact reason; the caller only sees the stable code.
//
// As in IssueProof, ctx bounds the wait for the store; an identical retry
// that cannot start in time returns ctx.Err() and changes nothing.
func (s *Store) RedeemProof(ctx context.Context, peer PeerIdentity, req RedeemRequest) (Redemption, error) {
	if !identityRequestKeyShape.MatchString(req.RequestKey) || !identityReceiptShape.MatchString(req.Receipt) ||
		!identityIDPattern.MatchString(req.ChallengeID) || req.HubID == "" {
		return Redemption{}, ErrInvalidRequest
	}
	receiptDigest := digestHex(req.Receipt)
	inputDigest := digestHex(req.HubID + "\x00" + req.ChallengeID + "\x00" + receiptDigest)
	actor := "peer:" + peer.ServiceID

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Redemption{}, err
	}
	defer tx.Rollback()
	// As in IssueProof: expiry is judged at the time the transaction starts.
	now := s.now()
	// A refusal commits only its audit record.
	refuse := func(reason string, cause error) (Redemption, error) {
		if err := audit(tx, actor, "identity.redeem.refused."+reason, req.ChallengeID); err != nil {
			return Redemption{}, err
		}
		if err := tx.Commit(); err != nil {
			return Redemption{}, err
		}
		return Redemption{}, cause
	}
	if err := peerStillValid(tx, peer, now); err != nil {
		return Redemption{}, err
	}
	if err := pruneIdentityLedger(tx, now); err != nil {
		return Redemption{}, err
	}

	var rec Redemption
	var recordedInput string
	var redeemedMs int64
	err = tx.QueryRow(`SELECT id,request_key,hub_id,challenge_id,user_id,token_id,redeemed_ms,input_digest
FROM identity_redemptions WHERE service_id=? AND request_key=?`, peer.ServiceID, req.RequestKey).
		Scan(&rec.ID, &rec.RequestKey, &rec.HubID, &rec.ChallengeID, &rec.UserID, &rec.TokenID, &redeemedMs, &recordedInput)
	switch {
	case err == nil:
		if recordedInput != inputDigest {
			return refuse("changed_input", ErrIdempotencyConflict)
		}
		live, userScoped, err := userTokenState(tx, rec.UserID, rec.TokenID, now)
		if err != nil {
			return Redemption{}, err
		}
		if !live || !userScoped {
			return refuse("token_inactive_on_replay", ErrCredentialInactive)
		}
		rec.Replayed, rec.PeerServiceID, rec.RedeemedAt = true, peer.ServiceID, time.UnixMilli(redeemedMs).UTC()
		return rec, tx.Commit()
	case !errors.Is(err, sql.ErrNoRows):
		return Redemption{}, err
	}

	var receiptID, service, hub, challenge, userID, tokenID, state string
	var expiresMs int64
	err = tx.QueryRow(`SELECT id,service_id,hub_id,challenge_id,user_id,token_id,expires_ms,state
FROM identity_receipts WHERE digest=?`, receiptDigest).Scan(&receiptID, &service, &hub, &challenge, &userID, &tokenID, &expiresMs, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return refuse("unknown", ErrProofInvalid)
	}
	if err != nil {
		return Redemption{}, err
	}
	switch {
	case service != peer.ServiceID:
		return refuse("wrong_peer", ErrProofInvalid)
	case challenge != req.ChallengeID:
		return refuse("wrong_challenge", ErrProofInvalid)
	case hub != req.HubID:
		return refuse("wrong_hub", ErrProofInvalid)
	case state == "redeemed":
		return refuse("already_redeemed", ErrProofInvalid)
	case state == "superseded":
		return refuse("superseded", ErrProofInvalid)
	case expiresMs <= now.UnixMilli():
		return refuse("expired", ErrProofInvalid)
	}
	live, userScoped, err := userTokenState(tx, userID, tokenID, now)
	if err != nil {
		return Redemption{}, err
	}
	if !live || !userScoped {
		return refuse("token_inactive", ErrCredentialInactive)
	}
	res, err := tx.Exec("UPDATE identity_receipts SET state='redeemed' WHERE id=? AND state='live'", receiptID)
	if err != nil {
		return Redemption{}, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return Redemption{}, fmt.Errorf("receipt state changed during redemption")
	}
	rec = Redemption{ID: uuidv7.New(), RequestKey: req.RequestKey, PeerServiceID: peer.ServiceID, ChallengeID: challenge,
		HubID: hub, UserID: userID, TokenID: tokenID, RedeemedAt: time.UnixMilli(now.UnixMilli()).UTC()}
	if _, err := tx.Exec(`INSERT INTO identity_redemptions(service_id,request_key,input_digest,id,receipt_id,hub_id,challenge_id,user_id,token_id,redeemed_ms)
VALUES(?,?,?,?,?,?,?,?,?,?)`, peer.ServiceID, req.RequestKey, inputDigest, rec.ID, receiptID, hub, challenge, userID, tokenID, now.UnixMilli()); err != nil {
		return Redemption{}, err
	}
	if err := audit(tx, actor, "identity.redeem", rec.ID); err != nil {
		return Redemption{}, err
	}
	return rec, tx.Commit()
}
