package access

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// dropIdentitySchema4 lets migration tests rebuild an older schema from a
// current store.
const dropIdentitySchema4 = `DROP TABLE identity_redemptions;
DROP TABLE identity_receipts;
DROP TABLE identity_peer_credentials;
DROP TABLE identity_peers;
`

type identityFixture struct {
	Bounds      map[string]int `json:"bounds"`
	KeyEncoding struct {
		Cases []struct {
			Case string `json:"case"`
			Raw  string `json:"raw_request_key"`
			Key  string `json:"idempotency_key"`
		} `json:"cases"`
	} `json:"request_key_encoding"`
}

func readIdentityFixture(t *testing.T) identityFixture {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "identity-v1", "examples.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f identityFixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func fixtureKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "k1_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

type identityEnv struct {
	s        *Store
	mu       sync.Mutex // guards now; the clock is read from ledger goroutines
	now      time.Time
	hub      string
	user     string
	token    string
	bearer   string
	peer     PeerIdentity
	peerCred string
	secrets  []string
}

func (e *identityEnv) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

func (e *identityEnv) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func newIdentityEnv(t *testing.T) *identityEnv {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := &identityEnv{s: s, now: time.Now().UTC().Truncate(time.Second)}
	s.clock = e.clock
	if e.hub, err = s.HubID(); err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser("admin", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	tok, secret, err := s.IssueScoped("admin", u.ID, "agent", ScopeUser, "", e.clock().Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	e.user, e.token, e.bearer = u.ID, tok.ID, secret
	e.registerPeer(t, "aicrew-example")
	_, e.peerCred = e.issueCredential(t, "aicrew-example")
	if e.peer, err = s.AuthenticatePeer(e.peerCred); err != nil {
		t.Fatal(err)
	}
	e.secrets = append(e.secrets, e.bearer, e.peerCred)
	return e
}

func (e *identityEnv) registerPeer(t *testing.T, service string) {
	t.Helper()
	if err := e.s.RegisterIdentityPeer("admin", IdentityPeer{ServiceID: service, HubID: e.hub,
		Endpoint: "https://aicrew.example/v1/crew/introspect", TLSMode: "ca_dns", TLSValue: "aicrew.example"}); err != nil {
		t.Fatal(err)
	}
}

func (e *identityEnv) issueCredential(t *testing.T, service string) (PeerCredential, string) {
	t.Helper()
	c, secret, err := e.s.IssuePeerCredential("admin", service, e.now.Add(30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	e.secrets = append(e.secrets, secret)
	return c, secret
}

func (e *identityEnv) proof(t *testing.T, challenge string) ProofReceipt {
	t.Helper()
	r, err := e.s.IssueProof(context.Background(), e.user, e.token, ProofRequest{PeerServiceID: e.peer.ServiceID, HubID: e.hub, ChallengeID: challenge})
	if err != nil {
		t.Fatal(err)
	}
	e.secrets = append(e.secrets, r.Receipt)
	return r
}

func (e *identityEnv) redeem(r ProofReceipt, key string) (Redemption, error) {
	return e.s.RedeemProof(context.Background(), e.peer, RedeemRequest{HubID: r.HubID, ChallengeID: r.ChallengeID, Receipt: r.Receipt, RequestKey: key})
}

func TestIdentityLedgerBoundsMatchContract(t *testing.T) {
	b := readIdentityFixture(t).Bounds
	for name, got := range map[string]int{
		"receipt_max_seconds":                    int(receiptLifetime / time.Second),
		"receipts_per_token_per_minute":          receiptsPerTokenPerMinute,
		"redemption_retention_seconds":           int(redemptionRetention / time.Second),
		"live_receipts_per_token_peer_challenge": 1,
	} {
		if b[name] != got {
			t.Errorf("%s: implementation %d, contract %d", name, got, b[name])
		}
	}
}

func TestSchema4MigrationPreservesSchema3(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser("admin", "Agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrant("admin", "project-instance", "user", u.ID, true); err != nil {
		t.Fatal(err)
	}
	tok, secret, err := s.IssueScoped("admin", u.ID, "agent", ScopeUser, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := s.createTeamProfile("admin", "aicrew-example", "team-example")
	if err != nil {
		t.Fatal(err)
	}
	hub, err := s.HubID()
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild a populated schema 3 database; everything in it must survive.
	if _, err := s.db.Exec(dropIdentitySchema4 + "PRAGMA user_version=3;"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 { // migration, then an ordinary reopen
		s, err = Open(root)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 4 {
			t.Fatalf("schema version %d: %v", version, err)
		}
		if got, err := s.HubID(); err != nil || got != hub {
			t.Fatalf("hub ID %q, want %q: %v", got, hub, err)
		}
		id, err := s.Authenticate(secret)
		if err != nil || id.UserID != u.ID || id.TokenID != tok.ID {
			t.Fatalf("migrated actor %+v: %v", id, err)
		}
		if ok, err := s.CanWriteToken(u.ID, tok.ID, "project-instance"); err != nil || !ok {
			t.Fatalf("standalone grant lost: %v %v", ok, err)
		}
		if p, err := s.teamProfileByKey("aicrew-example", "team-example"); err != nil || p.ID != profile.ID {
			t.Fatalf("team profile lost: %+v %v", p, err)
		}
		for _, table := range []string{"identity_peers", "identity_peer_credentials", "identity_receipts", "identity_redemptions"} {
			var n int
			if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
				t.Fatalf("%s: %d rows: %v", table, n, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestIdentityPeerRegistration(t *testing.T) {
	e := newIdentityEnv(t)
	good := IdentityPeer{ServiceID: "aicrew-2", HubID: e.hub, Endpoint: "https://aicrew.example/v1/crew/introspect", TLSMode: "ca_dns", TLSValue: "aicrew.example"}
	for name, mutate := range map[string]func(*IdentityPeer){
		"other hub":          func(p *IdentityPeer) { p.HubID = "hub-elsewhere" },
		"plain http":         func(p *IdentityPeer) { p.Endpoint = "http://aicrew.example/v1/crew/introspect" },
		"userinfo":           func(p *IdentityPeer) { p.Endpoint = "https://u:p@aicrew.example/x" },
		"ca_dns other host":  func(p *IdentityPeer) { p.TLSValue = "other.example" },
		"bad pin":            func(p *IdentityPeer) { p.TLSMode, p.TLSValue = "spki_sha256", "sha256-short" },
		"unknown trust mode": func(p *IdentityPeer) { p.TLSMode = "tofu" },
		"bad service ID":     func(p *IdentityPeer) { p.ServiceID = "a/b" },
	} {
		p := good
		mutate(&p)
		if err := e.s.RegisterIdentityPeer("admin", p); err == nil {
			t.Errorf("%s: registration accepted", name)
		}
	}
	if err := e.s.RegisterIdentityPeer("admin", good); err == nil {
		t.Error("second active peer accepted on one hub")
	}
	if err := e.s.SetIdentityPeerDisabled("admin", "aicrew-example", true); err != nil {
		t.Fatal(err)
	}
	pin := "sha256-" + base64.StdEncoding.EncodeToString(make([]byte, 32))
	good.TLSMode, good.TLSValue = "spki_sha256", pin
	if err := e.s.RegisterIdentityPeer("admin", good); err != nil {
		t.Fatalf("replacement peer after disable: %v", err)
	}
	if err := e.s.SetIdentityPeerDisabled("admin", "aicrew-example", false); err == nil {
		t.Error("re-enabling a second active peer accepted")
	}
	if _, err := e.s.AuthenticatePeer(e.peerCred); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Errorf("disabled peer's credential authenticated: %v", err)
	}
}

func TestPeerCredentialLifecycle(t *testing.T) {
	e := newIdentityEnv(t)
	if !regexp.MustCompile(`^aimem_peer_[0-9a-f]{64}$`).MatchString(e.peerCred) {
		t.Fatalf("peer credential format %q", e.peerCred[:len(peerCredentialPrefix)])
	}
	if _, _, err := e.s.IssuePeerCredential("admin", "aicrew-example", e.now.Add(367*24*time.Hour)); err == nil {
		t.Error("credential beyond 366 days accepted")
	}
	if _, _, err := e.s.IssuePeerCredential("admin", "aicrew-example", e.now); err == nil {
		t.Error("already-expired credential accepted")
	}
	// Rotation: a second active credential overlaps; a third is refused.
	second, secondSecret := e.issueCredential(t, "aicrew-example")
	if _, _, err := e.s.IssuePeerCredential("admin", "aicrew-example", e.now.Add(time.Hour)); !errors.Is(err, ErrPeerCredentialLimit) {
		t.Fatalf("third active credential: %v", err)
	}
	if p, err := e.s.AuthenticatePeer(secondSecret); err != nil || p.ServiceID != "aicrew-example" || p.CredentialID != second.ID {
		t.Fatalf("rotated credential: %+v %v", p, err)
	}
	// Lost-response recovery: metadata lists the unconfirmed credential,
	// revoking it frees the slot, and revocation is idempotent.
	list, err := e.s.ListPeerCredentials("aicrew-example")
	if err != nil || len(list) != 2 {
		t.Fatalf("credential list %+v: %v", list, err)
	}
	for range 2 {
		if err := e.s.RevokePeerCredential("admin", "aicrew-example", second.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.s.AuthenticatePeer(secondSecret); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Errorf("revoked credential authenticated: %v", err)
	}
	if _, err := e.s.AuthenticatePeer(e.peerCred); err != nil {
		t.Errorf("revoking one credential affected another: %v", err)
	}
	e.issueCredential(t, "aicrew-example")
	// Expiry: refused as unknown and no longer counted toward the limit.
	e.advance(31 * 24 * time.Hour)
	if _, err := e.s.AuthenticatePeer(e.peerCred); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Errorf("expired credential authenticated: %v", err)
	}
	e.issueCredential(t, "aicrew-example")
	e.issueCredential(t, "aicrew-example")
	if _, err := e.s.AuthenticatePeer("aimem_user_" + strings.Repeat("0", 64)); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Error("user-shaped bearer authenticated as a peer")
	}
}

func TestProofIssuance(t *testing.T) {
	e := newIdentityEnv(t)
	r := e.proof(t, "01a0dbee-0000-7000-8000-00000000c001")
	if !identityReceiptShape.MatchString(r.Receipt) || r.UserID != e.user || r.TokenID != e.token || r.HubID != e.hub ||
		r.PeerServiceID != "aicrew-example" || !r.ExpiresAt.Equal(e.now.Add(receiptLifetime)) {
		t.Fatalf("receipt binding %+v", r)
	}
	for name, tc := range map[string]struct {
		req  ProofRequest
		want error
	}{
		"other hub":       {ProofRequest{"aicrew-example", "hub-elsewhere", "c"}, ErrPeerUnknown},
		"unknown peer":    {ProofRequest{"aicrew-other", e.hub, "c"}, ErrPeerUnknown},
		"bad challenge":   {ProofRequest{"aicrew-example", e.hub, "has space"}, ErrInvalidRequest},
		"empty challenge": {ProofRequest{"aicrew-example", e.hub, ""}, ErrInvalidRequest},
	} {
		if _, err := e.s.IssueProof(context.Background(), e.user, e.token, tc.req); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	u, err := e.s.CreateUser("admin", "Other")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.SetGrant("admin", "project-a", "user", u.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, sc := range []struct {
		scope   TokenScope
		project string
	}{{ScopeProject, "project-a"}, {ScopeReadOnly, ""}} {
		tok, _, err := e.s.IssueScoped("admin", u.ID, "t", sc.scope, sc.project, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.s.IssueProof(context.Background(), u.ID, tok.ID, ProofRequest{"aicrew-example", e.hub, "c"}); !errors.Is(err, ErrCredentialScope) {
			t.Errorf("%s token: %v", sc.scope, err)
		}
	}
	if _, err := e.s.IssueProof(context.Background(), u.ID, e.token, ProofRequest{"aicrew-example", e.hub, "c"}); !errors.Is(err, ErrDenied) {
		t.Errorf("token presented under another user: %v", err)
	}
	if err := e.s.SetIdentityPeerDisabled("admin", "aicrew-example", true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.IssueProof(context.Background(), e.user, e.token, ProofRequest{"aicrew-example", e.hub, "c"}); !errors.Is(err, ErrPeerUnknown) {
		t.Errorf("disabled peer: %v", err)
	}
	if err := e.s.SetIdentityPeerDisabled("admin", "aicrew-example", false); err != nil {
		t.Fatal(err)
	}
	if err := e.s.Revoke("admin", e.token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.IssueProof(context.Background(), e.user, e.token, ProofRequest{"aicrew-example", e.hub, "c"}); !errors.Is(err, ErrDenied) {
		t.Errorf("revoked token: %v", err)
	}
}

func TestProofRateLimitAndSupersession(t *testing.T) {
	e := newIdentityEnv(t)
	first := e.proof(t, "challenge-1")
	second := e.proof(t, "challenge-1")
	if _, err := e.redeem(first, fixtureKey("a")); !errors.Is(err, ErrProofInvalid) {
		t.Errorf("superseded receipt redeemed: %v", err)
	}
	for i := 2; i < receiptsPerTokenPerMinute; i++ {
		e.proof(t, "challenge-other")
	}
	if _, err := e.s.IssueProof(context.Background(), e.user, e.token, ProofRequest{"aicrew-example", e.hub, "challenge-2"}); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("receipt beyond the per-minute limit: %v", err)
	}
	if _, err := e.redeem(second, fixtureKey("b")); err != nil {
		t.Errorf("live receipt after rate limit: %v", err)
	}
	e.advance(time.Minute + time.Millisecond)
	e.proof(t, "challenge-2")
}

func TestRedemption(t *testing.T) {
	e := newIdentityEnv(t)
	vectors := readIdentityFixture(t).KeyEncoding.Cases
	if len(vectors) == 0 {
		t.Fatal("no request key vectors")
	}
	for _, v := range vectors {
		if fixtureKey(v.Raw) != v.Key {
			t.Fatalf("%s: fixture key encoding disagrees", v.Case)
		}
	}
	key := vectors[0].Key
	r := e.proof(t, "01a0dbee-0000-7000-8000-00000000c001")
	got, err := e.redeem(r, key)
	if err != nil {
		t.Fatal(err)
	}
	if got.Replayed || got.UserID != e.user || got.TokenID != e.token || got.HubID != e.hub ||
		got.ChallengeID != r.ChallengeID || got.PeerServiceID != "aicrew-example" || got.RequestKey != key {
		t.Fatalf("redemption %+v", got)
	}
	again, err := e.redeem(r, key)
	if err != nil || !again.Replayed || again.ID != got.ID || !again.RedeemedAt.Equal(got.RedeemedAt) {
		t.Fatalf("identical replay %+v: %v", again, err)
	}
	changed := r
	changed.ChallengeID = "other-challenge"
	if _, err := e.redeem(changed, key); !errors.Is(err, ErrIdempotencyConflict) {
		t.Errorf("changed input under the same key: %v", err)
	}
	if _, err := e.redeem(r, vectors[1].Key); !errors.Is(err, ErrProofInvalid) {
		t.Errorf("second key for a redeemed receipt: %v", err)
	}
	for _, bad := range []string{"redeem:x:complete 1", "k1_short", vectors[1].Raw} {
		if _, err := e.redeem(e.proof(t, "c-"+string(rune('a'+len(bad)%26))), bad); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("raw or malformed key %q accepted: %v", bad, err)
		}
	}

	fresh := func(challenge string) ProofReceipt { return e.proof(t, challenge) }
	wrongChallenge := fresh("c-wrong-challenge")
	wrongChallenge.ChallengeID = "c-other"
	wrongHub := fresh("c-wrong-hub")
	wrongHub.HubID = "hub-elsewhere"
	expired := fresh("c-expired")
	for name, rr := range map[string]ProofReceipt{"wrong challenge": wrongChallenge, "wrong hub": wrongHub} {
		if _, err := e.redeem(rr, fixtureKey(name)); !errors.Is(err, ErrProofInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	unknown := fresh("c-unknown")
	unknown.Receipt = receiptPrefix + strings.Repeat("A", 43)
	if _, err := e.redeem(unknown, fixtureKey("unknown")); !errors.Is(err, ErrProofInvalid) {
		t.Errorf("unknown receipt: %v", err)
	}
	e.advance(receiptLifetime)
	if _, err := e.redeem(expired, fixtureKey("expired")); !errors.Is(err, ErrProofInvalid) {
		t.Errorf("expired receipt: %v", err)
	}

	// Exact refusal reasons stay in the audit; the caller sees only the code.
	var reasons []string
	rows, err := e.s.db.Query("SELECT action FROM audit WHERE action LIKE 'identity.redeem.refused.%' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			t.Fatal(err)
		}
		reasons = append(reasons, strings.TrimPrefix(a, "identity.redeem.refused."))
	}
	rows.Close()
	for _, want := range []string{"changed_input", "already_redeemed", "wrong_challenge", "wrong_hub", "unknown", "expired"} {
		if !strings.Contains(strings.Join(reasons, ","), want) {
			t.Errorf("refusal reason %s not audited (got %v)", want, reasons)
		}
	}
}

func TestRedemptionRevocationAndRetention(t *testing.T) {
	e := newIdentityEnv(t)
	redeemed := e.proof(t, "c-redeemed")
	if _, err := e.redeem(redeemed, fixtureKey("k1")); err != nil {
		t.Fatal(err)
	}
	pending := e.proof(t, "c-pending")
	if err := e.s.Revoke("admin", e.token); err != nil {
		t.Fatal(err)
	}
	if _, err := e.redeem(pending, fixtureKey("k2")); !errors.Is(err, ErrCredentialInactive) {
		t.Errorf("receipt of a revoked token: %v", err)
	}
	if _, err := e.redeem(redeemed, fixtureKey("k1")); !errors.Is(err, ErrCredentialInactive) {
		t.Errorf("replay after revocation: %v", err)
	}

	e2 := newIdentityEnv(t)
	r := e2.proof(t, "c-retained")
	if _, err := e2.redeem(r, fixtureKey("k1")); err != nil {
		t.Fatal(err)
	}
	e2.advance(redemptionRetention - time.Second)
	if got, err := e2.redeem(r, fixtureKey("k1")); err != nil || !got.Replayed {
		t.Fatalf("replay inside retention: %+v %v", got, err)
	}
	e2.advance(receiptLifetime + 2*time.Second)
	if _, err := e2.redeem(r, fixtureKey("k1")); !errors.Is(err, ErrProofInvalid) {
		t.Errorf("replay after retention: %v", err)
	}
	for _, table := range []string{"identity_receipts", "identity_redemptions"} {
		var n int
		if err := e2.s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s not pruned: %d %v", table, n, err)
		}
	}

	e3 := newIdentityEnv(t)
	r3 := e3.proof(t, "c-peer")
	stale := e3.peer
	list, err := e3.s.ListPeerCredentials("aicrew-example")
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	if err := e3.s.RevokePeerCredential("admin", "aicrew-example", list[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e3.s.RedeemProof(context.Background(), stale, RedeemRequest{e3.hub, r3.ChallengeID, r3.Receipt, fixtureKey("k")}); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Errorf("redemption with a credential revoked after authentication: %v", err)
	}
}

func TestRedemptionWrongPeer(t *testing.T) {
	e := newIdentityEnv(t)
	r := e.proof(t, "c-for-first-peer")
	if err := e.s.SetIdentityPeerDisabled("admin", "aicrew-example", true); err != nil {
		t.Fatal(err)
	}
	e.registerPeer(t, "aicrew-second")
	_, secret := e.issueCredential(t, "aicrew-second")
	other, err := e.s.AuthenticatePeer(secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RedeemProof(context.Background(), other, RedeemRequest{r.HubID, r.ChallengeID, r.Receipt, fixtureKey("k")}); !errors.Is(err, ErrProofInvalid) {
		t.Errorf("receipt redeemed by a different peer: %v", err)
	}
}

func TestConcurrentRedemptionHasOneWinner(t *testing.T) {
	e := newIdentityEnv(t)
	const n = 8
	r := e.proof(t, "c-race-keys")
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = e.redeem(r, fixtureKey(string(rune('a'+i))))
		}(i)
	}
	wg.Wait()
	wins := 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrProofInvalid):
			t.Errorf("racing key: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d winners across different keys", wins)
	}

	same := e.proof(t, "c-race-same-key")
	type out struct {
		red Redemption
		err error
	}
	outs := make([]out, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i].red, outs[i].err = e.redeem(same, fixtureKey("same"))
		}(i)
	}
	wg.Wait()
	originals, id := 0, ""
	for _, o := range outs {
		if o.err != nil {
			t.Fatalf("same-key retry: %v", o.err)
		}
		if !o.red.Replayed {
			originals++
		}
		if id == "" {
			id = o.red.ID
		} else if o.red.ID != id {
			t.Fatal("same-key retries returned different redemptions")
		}
	}
	if originals != 1 {
		t.Fatalf("%d original redemptions for one key", originals)
	}
}

// TestIdentityLedgerStoresNoSecret dumps every text value in the access
// database after a full lifecycle and requires that no bearer, receipt or
// peer secret appears; only digests are stored.
func TestIdentityLedgerStoresNoSecret(t *testing.T) {
	e := newIdentityEnv(t)
	r := e.proof(t, "c-secret")
	if _, err := e.redeem(r, fixtureKey("k")); err != nil {
		t.Fatal(err)
	}
	e.redeem(e.proof(t, "c-refused"), fixtureKey("k")) // audited refusal
	_, _ = e.s.IssueProof(context.Background(), e.user, e.token, ProofRequest{"aicrew-example", e.hub, "c-x"})
	tables, err := e.s.db.Query("SELECT name FROM sqlite_master WHERE type='table'")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var n string
		if err := tables.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	tables.Close()
	for _, table := range names {
		rows, err := e.s.db.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]sql.NullString, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				for _, secret := range e.secrets {
					if v.Valid && strings.Contains(v.String, secret) {
						t.Errorf("table %s stores a secret", table)
					}
				}
			}
		}
		rows.Close()
	}
}

// expiresWhileBlocked runs op while the store's only connection is held, waits
// until op is queued for it, advances the clock by d, then releases the
// connection. An expiry check must use the time at which op's transaction
// starts, not the time op was called.
func expiresWhileBlocked[T any](t *testing.T, e *identityEnv, d time.Duration, op func() (T, error)) error {
	t.Helper()
	held, err := e.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	waits := e.s.db.Stats().WaitCount
	done := make(chan error, 1)
	go func() {
		_, err := op()
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for e.s.db.Stats().WaitCount == waits {
		if time.Now().After(deadline) {
			t.Fatal("operation never queued for the store connection")
		}
		time.Sleep(time.Millisecond)
	}
	e.advance(d)
	if err := held.Rollback(); err != nil {
		t.Fatal(err)
	}
	return <-done
}

func TestExpiryIsJudgedWhenTheTransactionStarts(t *testing.T) {
	// Receipt: 1 s before expiry when called, expired once it gets the store.
	e := newIdentityEnv(t)
	r := e.proof(t, "c-receipt")
	e.advance(receiptLifetime - time.Second)
	if err := expiresWhileBlocked(t, e, 2*time.Second, func() (Redemption, error) { return e.redeem(r, fixtureKey("k")) }); !errors.Is(err, ErrProofInvalid) {
		t.Errorf("receipt that expired while waiting was redeemed: %v", err)
	}

	// Peer credential: a one-hour credential replaces the default one.
	e = newIdentityEnv(t)
	short, secret, err := e.s.IssuePeerCredential("admin", "aicrew-example", e.clock().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range mustList(t, e) {
		if c.ID != short.ID {
			if err := e.s.RevokePeerCredential("admin", "aicrew-example", c.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if e.peer, err = e.s.AuthenticatePeer(secret); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Hour - receiptLifetime/2)
	r = e.proof(t, "c-peer")
	if err := expiresWhileBlocked(t, e, receiptLifetime/2, func() (Redemption, error) { return e.redeem(r, fixtureKey("k")) }); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Errorf("peer credential that expired while waiting was accepted: %v", err)
	}

	// Individual token (24 hours), for redemption, replay and issuance.
	e = newIdentityEnv(t)
	e.advance(24*time.Hour - receiptLifetime/2)
	r = e.proof(t, "c-token")
	if err := expiresWhileBlocked(t, e, receiptLifetime/2, func() (Redemption, error) { return e.redeem(r, fixtureKey("k")) }); !errors.Is(err, ErrCredentialInactive) {
		t.Errorf("receipt of a token that expired while waiting was redeemed: %v", err)
	}
	e = newIdentityEnv(t)
	e.advance(24*time.Hour - receiptLifetime/2)
	r = e.proof(t, "c-replay")
	if _, err := e.redeem(r, fixtureKey("k")); err != nil {
		t.Fatal(err)
	}
	if err := expiresWhileBlocked(t, e, receiptLifetime/2, func() (Redemption, error) { return e.redeem(r, fixtureKey("k")) }); !errors.Is(err, ErrCredentialInactive) {
		t.Errorf("replay for a token that expired while waiting succeeded: %v", err)
	}
	e = newIdentityEnv(t)
	e.advance(24*time.Hour - time.Second)
	issue := func() (ProofReceipt, error) {
		return e.s.IssueProof(context.Background(), e.user, e.token, ProofRequest{e.peer.ServiceID, e.hub, "c-issue"})
	}
	if err := expiresWhileBlocked(t, e, 2*time.Second, issue); !errors.Is(err, ErrDenied) {
		t.Errorf("token that expired while waiting got a receipt: %v", err)
	}
}

func mustList(t *testing.T, e *identityEnv) []PeerCredential {
	t.Helper()
	list, err := e.s.ListPeerCredentials("aicrew-example")
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// TestStoreWaitIsBoundedByContext: while another transaction holds the store,
// a proof or redemption gives up when its context expires, changing nothing,
// and succeeds once the store is free. The routes use this for the 5 s bound.
func TestStoreWaitIsBoundedByContext(t *testing.T) {
	e := newIdentityEnv(t)
	r := e.proof(t, "c-wait")
	held, err := e.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.s.RedeemProof(ctx, e.peer, RedeemRequest{r.HubID, r.ChallengeID, r.Receipt, fixtureKey("k")}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("redemption waiting on a held store: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if _, err := e.s.IssueProof(ctx2, e.user, e.token, ProofRequest{e.peer.ServiceID, e.hub, "c-wait-2"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("issuance waiting on a held store: %v", err)
	}
	if err := held.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got, err := e.redeem(r, fixtureKey("k")); err != nil || got.Replayed {
		t.Fatalf("receipt after the timed-out attempt: %+v %v", got, err)
	}
}

// TestAuthenticationWaitIsBoundedByContext: the gate's authentication queries
// give up on their deadline while another transaction holds the store.
func TestAuthenticationWaitIsBoundedByContext(t *testing.T) {
	e := newIdentityEnv(t)
	held, err := e.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := e.s.AuthenticateContext(ctx, e.bearer); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("user authentication waiting on a held store: %v", err)
	}
	if _, err := e.s.AuthenticatePeerContext(ctx, e.peerCred); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("peer authentication waiting on a held store: %v", err)
	}
	if err := held.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.AuthenticateContext(context.Background(), e.bearer); err != nil {
		t.Errorf("authentication after release: %v", err)
	}
}
