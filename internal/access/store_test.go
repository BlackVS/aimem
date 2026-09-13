package access

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenExistingDoesNotAcquireWriteLock(t *testing.T) {
	root := t.TempDir()
	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	tx, err := first.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// A current schema can be opened/read while another connection owns the
	// reserved write lock. A migration transaction here would time out.
	second, err := OpenExisting(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := second.Authenticate("invalid"); !errors.Is(err, ErrDenied) {
		t.Fatalf("read under reserved writer: %v", err)
	}
}
func TestPermissionsAndRevocation(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser("admin", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	g, err := s.CreateGroup("admin", "Developers")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMember("admin", g.ID, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGrant("admin", "project-A", "group", g.ID, true); err != nil {
		t.Fatal(err)
	}
	tok, secret, err := s.Issue("admin", u.ID, "agent", "project-A", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "project-A"} {
		if _, err := s.Authorize(secret, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Authorize(secret, "project-B"); !errors.Is(err, ErrDenied) {
		t.Fatalf("cross-project: %v", err)
	}
	if err := s.SetGrant("admin", "project-A", "user", u.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMember("admin", g.ID, u.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authorize(secret, "project-A"); err != nil {
		t.Fatal("direct grant must survive group removal", err)
	}
	if err := s.SetGrant("admin", "project-A", "user", u.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authorize(secret, "project-A"); !errors.Is(err, ErrDenied) {
		t.Fatalf("removed access still writes: %v", err)
	}
	if _, err := s.Authorize(secret, ""); err != nil {
		t.Fatal("read should remain", err)
	}
	if err := s.SetUser("admin", u.ID, "Alice", true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(secret); !errors.Is(err, ErrDenied) {
		t.Fatalf("disabled user authenticated: %v", err)
	}
	if err := s.SetUser("admin", u.ID, "Alice", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke("admin", tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(secret); !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked token authenticated: %v", err)
	}
}
func TestTokenExpiryReadOnlyAndAuditAtomicity(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser("admin", "Reader")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Issue("admin", u.ID, "bad", "unassigned", time.Now().Add(time.Hour)); !errors.Is(err, ErrDenied) {
		t.Fatalf("unassigned issuance: %v", err)
	}
	tok, secret, err := s.Issue("admin", u.ID, "read", "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authorize(secret, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authorize(secret, "A"); !errors.Is(err, ErrDenied) {
		t.Fatal("read token wrote")
	}
	before, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser("", "rolled back"); err == nil {
		t.Fatal("missing audit actor accepted")
	}
	after, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Users) != len(after.Users) || len(before.Audit) != len(after.Audit) {
		t.Fatal("failed transaction changed data")
	}
	raw, _ := json.Marshal(after)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "digest") {
		t.Fatal("snapshot leaked credential material")
	}
	if _, err := s.db.Exec("UPDATE tokens SET expires_at=? WHERE id=?", time.Now().Unix()-1, tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(secret); !errors.Is(err, ErrDenied) {
		t.Fatal("expired credential accepted")
	}
	if _, _, err := s.Issue("admin", u.ID, "past", "", time.Now().Add(-time.Second)); err == nil {
		t.Fatal("past expiry accepted")
	}
}
func TestReopenAndFailClosed(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser("admin", "persistent")
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := s.Issue("admin", u.ID, "reader", "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Authenticate(secret); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("PRAGMA user_version=99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(root); err == nil {
		s.Close()
		t.Fatal("newer schema accepted")
	}
	badRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(badRoot, "access.db"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(badRoot); err == nil {
		s.Close()
		t.Fatal("corrupt access store replaced")
	}
}
