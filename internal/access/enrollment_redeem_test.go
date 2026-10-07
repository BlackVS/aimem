package access

import (
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aimem/internal/enrollment"
)

type redeemClient struct {
	key *ecdh.PrivateKey
	req EnrollmentRedeemRequest
}

func k1Of(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "k1_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

func newRedeemClient(t *testing.T, s *Store, subcode, rawKey string) redeemClient {
	t.Helper()
	key, err := enrollment.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	hub, err := s.HubID()
	if err != nil {
		t.Fatal(err)
	}
	return redeemClient{key: key, req: EnrollmentRedeemRequest{HubID: hub, Subcode: subcode, RequestKey: k1Of(rawKey),
		Label: "member-laptop", Suite: enrollment.Suite, PublicKey: enrollment.EncodePublicKey(key.PublicKey())}}
}

func (c redeemClient) open(t *testing.T, r EnrollmentRedeemResult) enrollment.Plaintext {
	t.Helper()
	p, err := enrollment.Open(r.Suite, c.key, enrollment.AAD(r.HubID, r.BundleID, c.req.RequestKey, r.UserID, r.TokenID), r.Enc, r.Ciphertext)
	if err != nil {
		t.Fatalf("open the delivery: %v", err)
	}
	return p
}

func auditActions(t *testing.T, s *Store, prefix string) []string {
	t.Helper()
	rows, err := s.db.Query("SELECT action FROM audit WHERE action LIKE ? ORDER BY id", prefix+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		rows.Scan(&a)
		out = append(out, a)
	}
	return out
}

// A first redemption creates one enabled user with no grant, group or
// profile and one user-scoped token, delivered only inside the ciphertext.
func TestRedeemEnrollmentCreatesUserAndToken(t *testing.T) {
	s := testStore(t)
	subcode := issueTestEnrollment(t, s, testBundle, time.Now().Add(time.Hour))
	c := newRedeemClient(t, s, subcode, "request-1")
	r, err := s.RedeemEnrollment(c.req)
	if err != nil {
		t.Fatal(err)
	}
	if r.Replayed || r.BundleID != testBundle || r.HubID != c.req.HubID || r.UserID == "" || r.TokenID == "" {
		t.Fatalf("answer: %+v", r)
	}
	p := c.open(t, r)
	if p.UserID != r.UserID || p.TokenID != r.TokenID || p.HubID != r.HubID || !strings.HasPrefix(p.Token, "aimem_user_") {
		t.Fatalf("plaintext: %+v", p)
	}
	id, err := s.Authenticate(p.Token)
	if err != nil || id.UserID != r.UserID || id.TokenID != r.TokenID || id.Scope != ScopeUser {
		t.Fatalf("the delivered token: %+v %v", id, err)
	}
	var name, label string
	var expires int64
	if err := s.db.QueryRow("SELECT u.name,t.label,t.expires_at FROM users u JOIN tokens t ON t.user_id=u.id WHERE t.id=?", r.TokenID).Scan(&name, &label, &expires); err != nil ||
		name != "member-laptop" || label != "member-laptop" {
		t.Fatalf("user %q token %q: %v", name, label, err)
	}
	if life := time.Until(time.Unix(expires, 0)); life < 364*24*time.Hour || life > 365*24*time.Hour {
		t.Fatalf("token life %v", life)
	}
	for _, q := range []string{"SELECT count(*) FROM grants WHERE subject=?", "SELECT count(*) FROM members WHERE user_id=?"} {
		var n int
		if err := s.db.QueryRow(q, r.UserID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d %v", q, n, err)
		}
	}
	// The bearer is stored only as its digest; the delivery column holds
	// the ciphertext alone.
	var delivery string
	if err := s.db.QueryRow("SELECT delivery FROM enrollments WHERE bundle_id=?", testBundle).Scan(&delivery); err != nil || strings.Contains(delivery, p.Token) {
		t.Fatalf("delivery column: %v", err)
	}
	if got := auditActions(t, s, "enrollment.redeem"); len(got) != 1 || got[0] != "enrollment.redeemed" {
		t.Fatalf("audit: %v", got)
	}
	var subject string
	s.db.QueryRow("SELECT subject FROM audit WHERE action='enrollment.redeemed'").Scan(&subject)
	if strings.Contains(subject, p.Token) || strings.Contains(subject, subcode) || !strings.Contains(subject, r.UserID) {
		t.Fatalf("audit subject: %q", subject)
	}
	list, _ := s.ListEnrollments(EnrollmentRedeemed)
	if len(list) != 1 || list[0].Redeemed == nil || list[0].Redeemed.TokenID != r.TokenID {
		t.Fatalf("ledger: %+v", list)
	}
}

func TestRedeemEnrollmentUsesTheIssuerUserName(t *testing.T) {
	s := testStore(t)
	_, subcode, err := s.IssueEnrollment("admin", EnrollmentRequest{BundleID: testBundle, Purpose: EnrollmentPurposeNewUser,
		ExpiresAt: time.Now().Add(time.Hour), UserName: "Member One"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.RedeemEnrollment(newRedeemClient(t, s, subcode, "k").req)
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if err := s.db.QueryRow("SELECT name FROM users WHERE id=?", r.UserID).Scan(&name); err != nil || name != "Member One" {
		t.Fatalf("name %q %v", name, err)
	}
}

// An identical retry replays the stored answer for an hour after the
// redemption, also past the subcode's expiry; after that hour it is refused.
func TestRedeemEnrollmentReplayAndRetention(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	subcode := issueTestEnrollment(t, s, testBundle, now.Add(10*time.Minute))
	c := newRedeemClient(t, s, subcode, "request-1")
	first, err := s.RedeemEnrollment(c.req)
	if err != nil {
		t.Fatal(err)
	}
	s.clock = func() time.Time { return now.Add(30 * time.Minute) } // past the subcode's expiry
	again, err := s.RedeemEnrollment(c.req)
	if err != nil || !again.Replayed || again.UserID != first.UserID || again.TokenID != first.TokenID ||
		again.Enc != first.Enc || again.Ciphertext != first.Ciphertext {
		t.Fatalf("replay past expiry: %+v %v", again, err)
	}
	if p := c.open(t, again); p.TokenID != first.TokenID {
		t.Fatalf("replayed plaintext: %+v", p)
	}
	s.clock = func() time.Time { return now.Add(EnrollmentDeliveryRetention + time.Minute) }
	if _, err := s.RedeemEnrollment(c.req); !errors.Is(err, ErrEnrollmentInvalid) {
		t.Fatalf("after the retention: %v", err)
	}
	if got := auditActions(t, s, "enrollment.redeem"); strings.Join(got, ",") != "enrollment.redeemed,enrollment.redeem.replayed,enrollment.redeem.refused.retention_expired" {
		t.Fatalf("audit: %v", got)
	}
}

// A replay rechecks that the token it delivers is still live.
func TestRedeemEnrollmentReplayRechecksTheCredential(t *testing.T) {
	for _, kill := range []string{"UPDATE tokens SET revoked=1 WHERE id=?", "UPDATE users SET disabled=1 WHERE id=(SELECT user_id FROM tokens WHERE id=?)"} {
		s := testStore(t)
		subcode := issueTestEnrollment(t, s, testBundle, time.Now().Add(time.Hour))
		c := newRedeemClient(t, s, subcode, "k")
		r, err := s.RedeemEnrollment(c.req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(kill, r.TokenID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.RedeemEnrollment(c.req); !errors.Is(err, ErrEnrollmentInvalid) {
			t.Fatalf("%s: %v", kill, err)
		}
		if got := auditActions(t, s, "enrollment.redeem.refused"); len(got) != 1 || got[0] != "enrollment.redeem.refused.credential_inactive" {
			t.Fatalf("audit: %v", got)
		}
	}
}

func TestRedeemEnrollmentConflicts(t *testing.T) {
	s := testStore(t)
	subcode := issueTestEnrollment(t, s, testBundle, time.Now().Add(time.Hour))
	c := newRedeemClient(t, s, subcode, "request-1")
	if _, err := s.RedeemEnrollment(c.req); err != nil {
		t.Fatal(err)
	}
	other := newRedeemClient(t, s, subcode, "request-2")
	relabel := c.req
	relabel.Label = "other-laptop"
	rekey := c.req
	rekey.PublicKey = other.req.PublicKey
	for name, req := range map[string]EnrollmentRedeemRequest{"another key": other.req, "another label": relabel, "another public key": rekey} {
		if r, err := s.RedeemEnrollment(req); !errors.Is(err, ErrEnrollmentConflict) || r.Ciphertext != "" {
			t.Errorf("%s: %+v %v", name, r, err)
		}
	}
}

// Every refusal of a subcode is the same enrollment_invalid; the audit keeps
// the reason.
func TestRedeemEnrollmentNonDisclosingRefusals(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	revoked := issueTestEnrollment(t, s, "01a10c90-0000-7000-8000-000000000001", now.Add(time.Hour))
	if _, err := s.RevokeEnrollment("admin", "01a10c90-0000-7000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	expired := issueTestEnrollment(t, s, "01a10c90-0000-7000-8000-000000000002", now.Add(time.Hour))
	live := issueTestEnrollment(t, s, "01a10c90-0000-7000-8000-000000000003", now.Add(3*time.Hour))
	s.clock = func() time.Time { return now.Add(2 * time.Hour) }
	wrongHub := newRedeemClient(t, s, live, "k").req
	wrongHub.HubID = "01a10c90-0000-7000-8000-00000000beef"
	for name, req := range map[string]EnrollmentRedeemRequest{
		"unknown":   newRedeemClient(t, s, "aes1_"+strings.Repeat("A", 43), "k").req,
		"revoked":   newRedeemClient(t, s, revoked, "k").req,
		"expired":   newRedeemClient(t, s, expired, "k").req,
		"wrong hub": wrongHub,
	} {
		if _, err := s.RedeemEnrollment(req); !errors.Is(err, ErrEnrollmentInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	got := strings.Join(auditActions(t, s, "enrollment.redeem.refused"), ",")
	for _, reason := range []string{"unknown", "revoked", "expired", "wrong_hub"} {
		if !strings.Contains(got, "enrollment.redeem.refused."+reason) {
			t.Errorf("audit lacks %s: %s", reason, got)
		}
	}
	var users int
	s.db.QueryRow("SELECT count(*) FROM users").Scan(&users)
	if users != 0 {
		t.Fatalf("a refusal created %d users", users)
	}
}

func TestRedeemEnrollmentValidatesInput(t *testing.T) {
	s := testStore(t)
	subcode := issueTestEnrollment(t, s, testBundle, time.Now().Add(time.Hour))
	base := newRedeemClient(t, s, subcode, "k").req
	for name, mut := range map[string]func(*EnrollmentRedeemRequest){
		"raw request key": func(r *EnrollmentRedeemRequest) { r.RequestKey = "request-1" },
		"label case":      func(r *EnrollmentRedeemRequest) { r.Label = "Laptop" },
		"label length":    func(r *EnrollmentRedeemRequest) { r.Label = strings.Repeat("a", 65) },
		"suite":           func(r *EnrollmentRedeemRequest) { r.Suite = "hpke-other" },
		"public key":      func(r *EnrollmentRedeemRequest) { r.PublicKey = "short" },
	} {
		req := base
		mut(&req)
		if _, err := s.RedeemEnrollment(req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if list, _ := s.ListEnrollments(EnrollmentIssued); len(list) != 1 {
		t.Fatal("an invalid request consumed the subcode")
	}
}

// Revocation and redemption are serialized: whichever commits first wins.
func TestRedeemAndRevokeOrder(t *testing.T) {
	s := testStore(t)
	first := issueTestEnrollment(t, s, "01a10c90-0000-7000-8000-000000000001", time.Now().Add(time.Hour))
	if _, err := s.RevokeEnrollment("admin", "01a10c90-0000-7000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RedeemEnrollment(newRedeemClient(t, s, first, "k").req); !errors.Is(err, ErrEnrollmentInvalid) {
		t.Fatalf("redeem after revoke: %v", err)
	}
	second := issueTestEnrollment(t, s, "01a10c90-0000-7000-8000-000000000002", time.Now().Add(time.Hour))
	r, err := s.RedeemEnrollment(newRedeemClient(t, s, second, "k").req)
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.RevokeEnrollment("admin", "01a10c90-0000-7000-8000-000000000002")
	if !errors.Is(err, ErrEnrollmentRedeemed) || e.Redeemed == nil || e.Redeemed.TokenID != r.TokenID {
		t.Fatalf("revoke after redeem: %+v %v", e, err)
	}
}

// Concurrent first redemptions with different keys: exactly one wins and
// exactly one user exists; every other request is a conflict.
func TestRedeemEnrollmentConcurrently(t *testing.T) {
	s := testStore(t)
	subcode := issueTestEnrollment(t, s, testBundle, time.Now().Add(time.Hour))
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := range 8 {
		c := newRedeemClient(t, s, subcode, "request-"+string(rune('a'+i)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.RedeemEnrollment(c.req)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case errors.Is(err, ErrEnrollmentConflict):
				conflicts++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	var users int
	if err := s.db.QueryRow("SELECT count(*) FROM users").Scan(&users); err != nil {
		t.Fatal(err)
	}
	if wins != 1 || conflicts != 7 || users != 1 {
		t.Fatalf("wins %d conflicts %d users %d", wins, conflicts, users)
	}
}

// settableClock is a store clock a test moves while a redemption waits.
type settableClock struct{ ns atomic.Int64 }

func (c *settableClock) set(t time.Time) { c.ns.Store(t.UnixNano()) }
func (c *settableClock) now() time.Time  { return time.Unix(0, c.ns.Load()) }

// redeemAfterWait starts a redemption while the store's one connection is
// held, moves the clock to after, then releases the connection: the
// redemption must be judged at the time it runs.
func redeemAfterWait(t *testing.T, s *Store, clock *settableClock, req EnrollmentRedeemRequest, after time.Time) error {
	t.Helper()
	hold, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	waits := s.db.Stats().WaitCount
	done := make(chan error, 1)
	go func() {
		_, err := s.RedeemEnrollment(req)
		done <- err
	}()
	// The store has one connection: a rise in its wait count means the
	// redemption is queued for it, so the clock moves only after that.
	deadline := time.Now().Add(10 * time.Second)
	for s.db.Stats().WaitCount == waits {
		if time.Now().After(deadline) {
			hold.Rollback()
			t.Fatal("the redemption never waited for the connection")
		}
		time.Sleep(time.Millisecond)
	}
	clock.set(after)
	hold.Rollback()
	return <-done
}

// A first redemption queued just before the subcode expires and run just
// after it is refused, and creates nothing.
func TestRedeemEnrollmentJudgedWhenItRunsAcrossExpiry(t *testing.T) {
	s := testStore(t)
	clock := &settableClock{}
	base := time.Now()
	clock.set(base)
	s.clock = clock.now
	subcode := issueTestEnrollment(t, s, testBundle, base.Add(time.Hour))
	clock.set(base.Add(time.Hour - time.Second))
	err := redeemAfterWait(t, s, clock, newRedeemClient(t, s, subcode, "k").req, base.Add(time.Hour+time.Second))
	if !errors.Is(err, ErrEnrollmentInvalid) {
		t.Fatalf("queued across expiry: %v", err)
	}
	var users int
	if err := s.db.QueryRow("SELECT count(*) FROM users").Scan(&users); err != nil || users != 0 {
		t.Fatalf("users %d %v", users, err)
	}
}

// A replay queued just before the retention ends and run just after it is
// refused.
func TestRedeemEnrollmentReplayJudgedWhenItRunsAcrossRetention(t *testing.T) {
	s := testStore(t)
	clock := &settableClock{}
	base := time.Now()
	clock.set(base)
	s.clock = clock.now
	subcode := issueTestEnrollment(t, s, testBundle, base.Add(time.Hour))
	c := newRedeemClient(t, s, subcode, "k")
	if _, err := s.RedeemEnrollment(c.req); err != nil {
		t.Fatal(err)
	}
	clock.set(base.Add(EnrollmentDeliveryRetention - time.Second))
	if err := redeemAfterWait(t, s, clock, c.req, base.Add(EnrollmentDeliveryRetention+time.Second)); !errors.Is(err, ErrEnrollmentInvalid) {
		t.Fatalf("replay queued across the retention: %v", err)
	}
}

// Past the retention a spent subcode is enrollment_invalid for any request,
// conflicting or not, so it cannot be told apart from other invalid ones.
func TestRedeemEnrollmentAfterRetentionDisclosesNoConflict(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	subcode := issueTestEnrollment(t, s, testBundle, now.Add(time.Hour))
	c := newRedeemClient(t, s, subcode, "request-1")
	if _, err := s.RedeemEnrollment(c.req); err != nil {
		t.Fatal(err)
	}
	s.clock = func() time.Time { return now.Add(EnrollmentDeliveryRetention + time.Minute) }
	relabel := c.req
	relabel.Label = "other-laptop"
	for name, req := range map[string]EnrollmentRedeemRequest{
		"identical":     c.req,
		"another key":   newRedeemClient(t, s, subcode, "request-2").req,
		"another label": relabel,
	} {
		if _, err := s.RedeemEnrollment(req); !errors.Is(err, ErrEnrollmentInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, a := range auditActions(t, s, "enrollment.redeem.refused") {
		if a != "enrollment.redeem.refused.retention_expired" {
			t.Errorf("audit reason %s", a)
		}
	}
}
