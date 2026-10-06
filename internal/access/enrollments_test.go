package access

import (
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const testBundle = "01a10c90-0000-7000-8000-000000000001"

func issueTestEnrollment(t *testing.T, s *Store, bundle string, expires time.Time) string {
	t.Helper()
	_, subcode, err := s.IssueEnrollment("admin", EnrollmentRequest{BundleID: bundle, Purpose: EnrollmentPurposeNewUser, ExpiresAt: expires})
	if err != nil {
		t.Fatal(err)
	}
	return subcode
}

// Schema 7 adds the enrollment ledger and nothing else: a populated schema-6
// store keeps its users, tokens and peer credentials across the migration.
func TestAccessSchema7AddsEnrollmentsAndKeepsRows(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser("admin", "pilot")
	if err != nil {
		t.Fatal(err)
	}
	tok, secret, err := s.IssueScoped("admin", u.ID, "agent", ScopeUser, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("DROP TABLE enrollments; PRAGMA user_version=6;"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 { // migration, then an ordinary reopen
		if s, err = Open(root); err != nil {
			t.Fatal(err)
		}
		var version int
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 7 || accessSchema != 7 {
			t.Fatalf("schema version %d: %v", version, err)
		}
		id, err := s.Authenticate(secret)
		if err != nil || id.TokenID != tok.ID || id.UserID != u.ID {
			t.Fatalf("a schema-6 token after the migration: %+v %v", id, err)
		}
		if list, err := s.ListEnrollments(""); err != nil || len(list) != 0 {
			t.Fatalf("a new ledger: %v %v", list, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// The subcode has the contract's shape, is returned once, and is stored only
// as its digest: no column of the ledger holds it.
func TestIssueEnrollmentStoresOnlyTheDigest(t *testing.T) {
	s := testStore(t)
	e, subcode, err := s.IssueEnrollment("admin", EnrollmentRequest{BundleID: testBundle, Purpose: EnrollmentPurposeNewUser,
		ExpiresAt: time.Now().Add(24 * time.Hour), UserName: "member one"})
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^aes1_[A-Za-z0-9_-]{43}$`).MatchString(subcode) {
		t.Fatalf("subcode shape: %q", subcode)
	}
	if e.State != EnrollmentIssued || e.IssuedBy != "admin" || e.UserName != "member one" {
		t.Fatalf("issued record: %+v", e)
	}
	rows, err := s.db.Query("SELECT * FROM enrollments")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
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
		for i, v := range vals {
			if strings.Contains(v.String, subcode) || strings.Contains(v.String, strings.TrimPrefix(subcode, "aes1_")) {
				t.Fatalf("column %s holds the subcode", cols[i])
			}
		}
		if vals[2].String != digestHex(subcode) {
			t.Fatalf("digest column: %q", vals[2].String)
		}
	}
	var audited string
	if err := s.db.QueryRow("SELECT subject FROM audit WHERE action='enrollment.issued'").Scan(&audited); err != nil ||
		!strings.Contains(audited, testBundle) || strings.Contains(audited, subcode) {
		t.Fatalf("issue audit: %q %v", audited, err)
	}
}

func TestIssueEnrollmentRules(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	for _, tc := range []struct {
		name string
		req  EnrollmentRequest
	}{
		{"reissue purpose", EnrollmentRequest{BundleID: testBundle, Purpose: "existing_user_reissue", ExpiresAt: now.Add(time.Hour)}},
		{"unknown purpose", EnrollmentRequest{BundleID: testBundle, Purpose: "admin", ExpiresAt: now.Add(time.Hour)}},
		{"past expiry", EnrollmentRequest{BundleID: testBundle, Purpose: EnrollmentPurposeNewUser, ExpiresAt: now.Add(-time.Minute)}},
		{"beyond 72 hours", EnrollmentRequest{BundleID: testBundle, Purpose: EnrollmentPurposeNewUser, ExpiresAt: now.Add(72*time.Hour + time.Minute)}},
		{"bundle not a UUID", EnrollmentRequest{BundleID: "bundle-1", Purpose: EnrollmentPurposeNewUser, ExpiresAt: now.Add(time.Hour)}},
		{"uppercase UUID", EnrollmentRequest{BundleID: strings.ToUpper(testBundle), Purpose: EnrollmentPurposeNewUser, ExpiresAt: now.Add(time.Hour)}},
		{"bad user name", EnrollmentRequest{BundleID: testBundle, Purpose: EnrollmentPurposeNewUser, ExpiresAt: now.Add(time.Hour), UserName: " padded"}},
	} {
		if _, _, err := s.IssueEnrollment("admin", tc.req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if _, _, err := s.IssueEnrollment("admin", EnrollmentRequest{BundleID: testBundle, Purpose: EnrollmentPurposeNewUser, ExpiresAt: now.Add(72 * time.Hour)}); err != nil {
		t.Fatalf("exactly 72 hours: %v", err)
	}
}

// A repeated bundle ID never reveals a subcode: the same input is
// enrollment_exists, other input idempotency_conflict.
func TestIssueEnrollmentRepeatedBundle(t *testing.T) {
	s := testStore(t)
	expires := time.Now().Add(time.Hour)
	issueTestEnrollment(t, s, testBundle, expires)
	if _, subcode, err := s.IssueEnrollment("admin", EnrollmentRequest{BundleID: testBundle, Purpose: EnrollmentPurposeNewUser, ExpiresAt: expires}); !errors.Is(err, ErrEnrollmentExists) || subcode != "" {
		t.Fatalf("same input: %q %v", subcode, err)
	}
	if _, subcode, err := s.IssueEnrollment("admin", EnrollmentRequest{BundleID: testBundle, Purpose: EnrollmentPurposeNewUser, ExpiresAt: expires.Add(time.Minute)}); !errors.Is(err, ErrIdempotencyConflict) || subcode != "" {
		t.Fatalf("other input: %q %v", subcode, err)
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM enrollments").Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows: %d %v", n, err)
	}
}

func TestRevokeEnrollmentStates(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	issueTestEnrollment(t, s, testBundle, now.Add(time.Hour))
	if _, err := s.RevokeEnrollment("admin", "01a10c90-0000-7000-8000-00000000ffff"); !errors.Is(err, ErrEnrollmentUnknown) {
		t.Fatalf("unknown bundle: %v", err)
	}
	e, err := s.RevokeEnrollment("admin", testBundle)
	if err != nil || e.State != EnrollmentRevoked {
		t.Fatalf("revoke issued: %+v %v", e, err)
	}
	if e, err := s.RevokeEnrollment("admin", testBundle); err != nil || e.State != EnrollmentRevoked {
		t.Fatalf("revoke revoked again: %+v %v", e, err)
	}

	expired := "01a10c90-0000-7000-8000-000000000002"
	issueTestEnrollment(t, s, expired, now.Add(time.Hour))
	s.clock = func() time.Time { return now.Add(2 * time.Hour) }
	if e, err := s.RevokeEnrollment("admin", expired); err != nil || e.State != EnrollmentExpired {
		t.Fatalf("revoke expired: %+v %v", e, err)
	}
	s.clock = nil

	// A redeemed bundle (as D1-b will record it) refuses, names what it
	// issued, and audits the refusal.
	redeemed := "01a10c90-0000-7000-8000-000000000003"
	issueTestEnrollment(t, s, redeemed, now.Add(time.Hour))
	if _, err := s.db.Exec("UPDATE enrollments SET redeemed_at=?, redeemed_user_id='u-1', redeemed_token_id='t-1' WHERE bundle_id=?", now.Unix(), redeemed); err != nil {
		t.Fatal(err)
	}
	e, err = s.RevokeEnrollment("admin", redeemed)
	if !errors.Is(err, ErrEnrollmentRedeemed) || e.Redeemed == nil || e.Redeemed.UserID != "u-1" || e.Redeemed.TokenID != "t-1" {
		t.Fatalf("revoke redeemed: %+v %v", e, err)
	}
	var actions []string
	rows, err := s.db.Query("SELECT action FROM audit WHERE action LIKE 'enrollment.revoke%' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var a string
		rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	if strings.Join(actions, ",") != "enrollment.revoked,enrollment.revoke.refused.redeemed" {
		t.Fatalf("revoke audit: %v", actions)
	}
}

func TestListEnrollmentsByState(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	issueTestEnrollment(t, s, "01a10c90-0000-7000-8000-000000000001", now.Add(time.Hour))
	issueTestEnrollment(t, s, "01a10c90-0000-7000-8000-000000000002", now.Add(3*time.Hour))
	if _, err := s.RevokeEnrollment("admin", "01a10c90-0000-7000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListEnrollments("")
	if err != nil || len(all) != 2 || all[0].BundleID != "01a10c90-0000-7000-8000-000000000002" {
		t.Fatalf("all, newest first: %+v %v", all, err)
	}
	for state, want := range map[string]int{EnrollmentIssued: 1, EnrollmentRevoked: 1, EnrollmentRedeemed: 0, EnrollmentExpired: 0} {
		if got, err := s.ListEnrollments(state); err != nil || len(got) != want {
			t.Errorf("state %s: %d %v", state, len(got), err)
		}
	}
	s.clock = func() time.Time { return now.Add(2 * time.Hour) }
	if got, err := s.ListEnrollments(EnrollmentExpired); err != nil || len(got) != 0 {
		t.Fatalf("expired at +2h: %+v %v", got, err) // the revoked one stays revoked
	}
	s.clock = func() time.Time { return now.Add(4 * time.Hour) }
	if got, err := s.ListEnrollments(EnrollmentExpired); err != nil || len(got) != 1 || got[0].BundleID != "01a10c90-0000-7000-8000-000000000002" {
		t.Fatalf("expired at +4h: %+v %v", got, err)
	}
	if _, err := s.ListEnrollments("lost"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unknown state: %v", err)
	}
}

// Concurrent revocations of one bundle are serialized: every one succeeds,
// and the state changes, and is audited, exactly once.
func TestRevokeEnrollmentConcurrently(t *testing.T) {
	s := testStore(t)
	issueTestEnrollment(t, s, testBundle, time.Now().Add(time.Hour))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := s.RevokeEnrollment("admin", testBundle)
			if err == nil && e.State != EnrollmentRevoked {
				err = errors.New("state " + e.State)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM audit WHERE action='enrollment.revoked'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("revoked audits: %d %v", n, err)
	}
}
