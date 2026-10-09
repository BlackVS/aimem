package access

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// A peer credential permits exactly one operation (task C6b, D6-3a): the
// limit of two active holds per peer and operation, an unknown operation is
// refused, the operation is audited and listed, and a reservation.read
// credential never redeems.
func TestPeerCredentialOperations(t *testing.T) {
	e := newIdentityEnv(t)
	if _, _, err := e.s.IssuePeerCredential("admin", "aicrew-example", "reservation.write", e.now.Add(time.Hour)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("an unknown operation: %v", err)
	}
	// Redemption already has one active credential from the env; reads
	// have their own two.
	read1, readSecret, err := e.s.IssuePeerCredential("admin", "aicrew-example", PeerOperationReservationRead, e.now.Add(time.Hour))
	if err != nil || read1.Operation != PeerOperationReservationRead {
		t.Fatalf("a read credential: %+v %v", read1, err)
	}
	e.secrets = append(e.secrets, readSecret)
	if _, _, err := e.s.IssuePeerCredential("admin", "aicrew-example", PeerOperationReservationRead, e.now.Add(time.Hour)); err != nil {
		t.Fatalf("a second read credential: %v", err)
	}
	if _, _, err := e.s.IssuePeerCredential("admin", "aicrew-example", PeerOperationReservationRead, e.now.Add(time.Hour)); !errors.Is(err, ErrPeerCredentialLimit) {
		t.Fatalf("a third active read credential: %v", err)
	}
	if _, _, err := e.s.IssuePeerCredential("admin", "aicrew-example", PeerOperationRedeem, e.now.Add(time.Hour)); err != nil {
		t.Fatalf("the reads took a redemption slot: %v", err)
	}
	p, err := e.s.AuthenticatePeer(readSecret)
	if err != nil || p.Operation != PeerOperationReservationRead || p.CredentialID != read1.ID {
		t.Fatalf("a read credential authenticates as %+v: %v", p, err)
	}
	if e.peer.Operation != PeerOperationRedeem {
		t.Fatalf("the redemption credential authenticates as %+v", e.peer)
	}
	// A read credential never redeems, whatever reaches the store.
	receipt := e.proof(t, "challenge-read")
	if _, err := e.s.RedeemProof(context.Background(), p, RedeemRequest{HubID: e.hub, ChallengeID: "challenge-read",
		Receipt: receipt.Receipt, RequestKey: "k1_" + strings.Repeat("R", 43)}); !errors.Is(err, ErrPeerForbidden) {
		t.Fatalf("a read credential redeemed: %v", err)
	}
	// Nor does a redemption credential's identity with the operation
	// swapped: the recheck binds the credential to its own operation.
	forged := e.peer
	forged.Operation = PeerOperationRedeem
	forged.CredentialID = read1.ID
	if _, err := e.s.RedeemProof(context.Background(), forged, RedeemRequest{HubID: e.hub, ChallengeID: "challenge-read",
		Receipt: receipt.Receipt, RequestKey: "k1_" + strings.Repeat("S", 43)}); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Fatalf("a read credential passed as a redemption one: %v", err)
	}
	list, err := e.s.ListPeerCredentials("aicrew-example")
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]int{}
	for _, c := range list {
		ops[c.Operation]++
	}
	if ops[PeerOperationRedeem] != 2 || ops[PeerOperationReservationRead] != 2 || len(ops) != 2 {
		t.Fatalf("listed operations: %v", ops)
	}
	rows, err := e.s.db.Query("SELECT action,subject FROM audit")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var issuedRead int
	for rows.Next() {
		var action, subject string
		if err := rows.Scan(&action, &subject); err != nil {
			t.Fatal(err)
		}
		if action == "identity_peer.credential.issue.reservation.read" {
			issuedRead++
		}
		for _, secret := range e.secrets {
			if strings.Contains(subject, secret) || strings.Contains(action, secret) {
				t.Fatal("the audit carries a secret")
			}
		}
	}
	if issuedRead != 2 {
		t.Fatalf("read credential issues audited: %d", issuedRead)
	}
}

// Schema 5 gives every existing credential identity.redeem, and it keeps
// authenticating.
func TestAccessSchema5KeepsCredentialsAsRedemption(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := s.HubID()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterIdentityPeer("admin", IdentityPeer{ServiceID: "aicrew-example", HubID: hub,
		Endpoint: "https://aicrew.example/v1/crew/introspect", TLSMode: "ca_dns", TLSValue: "aicrew.example"}); err != nil {
		t.Fatal(err)
	}
	c, secret, err := s.IssuePeerCredential("admin", "aicrew-example", PeerOperationRedeem, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Back to schema 4: the column did not exist, nor schema 6's team name
	// or schema 7's enrollment ledger.
	if _, err := s.db.Exec(`DROP TABLE enrollments; DROP INDEX team_access_profiles_name; ALTER TABLE team_access_profiles DROP COLUMN team_name;
ALTER TABLE identity_peer_credentials DROP COLUMN operation; PRAGMA user_version=4;`); err != nil {
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
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != accessSchema {
			t.Fatalf("schema version %d: %v", version, err)
		}
		p, err := s.AuthenticatePeer(secret)
		if err != nil || p.CredentialID != c.ID || p.Operation != PeerOperationRedeem {
			t.Fatalf("a migrated credential: %+v %v", p, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// Schema 6 widens the operation set by rebuilding the credential table:
// every schema-5 credential keeps its ID, digest, expiry and operation and
// still authenticates; team profiles keep their rows and grants and start
// without a name; the new operations can then be issued.
func TestAccessSchema6KeepsCredentialsAndProfiles(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := s.HubID()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterIdentityPeer("admin", IdentityPeer{ServiceID: "aicrew-example", HubID: hub,
		Endpoint: "https://aicrew.example/v1/crew/introspect", TLSMode: "ca_dns", TLSValue: "aicrew.example"}); err != nil {
		t.Fatal(err)
	}
	type issued struct{ id, secret, op string }
	var creds []issued
	for _, op := range []string{PeerOperationRedeem, PeerOperationReservationRead} {
		c, secret, err := s.IssuePeerCredential("admin", "aicrew-example", op, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		creds = append(creds, issued{c.ID, secret, op})
	}
	profile, err := s.CreateTeamProfile("admin", "aicrew-example", "team-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetTeamGrant("admin", "instance-1", profile.ID, true); err != nil {
		t.Fatal(err)
	}
	// Back to schema 5: the old CHECK, no team_name, no enrollment ledger.
	if _, err := s.db.Exec(`
DROP TABLE enrollments;
CREATE TABLE c5(
 id TEXT PRIMARY KEY,
 service_id TEXT NOT NULL REFERENCES identity_peers(service_id),
 digest TEXT NOT NULL UNIQUE,
 created_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL,
 revoked INTEGER NOT NULL DEFAULT 0 CHECK(revoked IN (0,1)),
 operation TEXT NOT NULL DEFAULT 'identity.redeem' CHECK(operation IN ('identity.redeem','reservation.read'))
);
INSERT INTO c5 SELECT id,service_id,digest,created_at,expires_at,revoked,operation FROM identity_peer_credentials;
DROP TABLE identity_peer_credentials;
ALTER TABLE c5 RENAME TO identity_peer_credentials;
DROP INDEX team_access_profiles_name;
ALTER TABLE team_access_profiles DROP COLUMN team_name;
PRAGMA user_version=5;`); err != nil {
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
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != accessSchema {
			t.Fatalf("schema version %d: %v", version, err)
		}
		for _, c := range creds {
			p, err := s.AuthenticatePeer(c.secret)
			if err != nil || p.CredentialID != c.id || p.Operation != c.op {
				t.Fatalf("a migrated %s credential: %+v %v", c.op, p, err)
			}
		}
		got, err := s.TeamProfileByKey("aicrew-example", "team-1")
		if err != nil || got.ID != profile.ID || got.TeamName != "" {
			t.Fatalf("migrated profile: %+v %v", got, err)
		}
		if inst, err := s.TeamGrantInstances(profile.ID); err != nil || len(inst) != 1 || inst[0] != "instance-1" {
			t.Fatalf("migrated grant: %v %v", inst, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if s, err = Open(root); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, op := range []string{PeerOperationTeamRegister, PeerOperationTeamRead} {
		if _, _, err := s.IssuePeerCredential("admin", "aicrew-example", op, time.Now().Add(time.Hour)); err != nil {
			t.Fatalf("issue %s after the migration: %v", op, err)
		}
	}
	if _, _, err := s.IssuePeerCredential("admin", "aicrew-example", "team.write", time.Now().Add(time.Hour)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("an unknown operation: %v", err)
	}
}

// Schema 8 widens the credential table for board.read and keeps every
// credential of the four earlier operations authenticating as it was.
func TestAccessSchema8KeepsCredentialsAndAllowsBoardRead(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	hub, err := s.HubID()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterIdentityPeer("admin", IdentityPeer{ServiceID: "aicrew-example", HubID: hub,
		Endpoint: "https://aicrew.example/v1/crew/introspect", TLSMode: "ca_dns", TLSValue: "aicrew.example"}); err != nil {
		t.Fatal(err)
	}
	type issued struct{ id, secret, op string }
	var creds []issued
	for _, op := range []string{PeerOperationRedeem, PeerOperationReservationRead, PeerOperationTeamRegister, PeerOperationTeamRead} {
		c, secret, err := s.IssuePeerCredential("admin", "aicrew-example", op, time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		creds = append(creds, issued{c.ID, secret, op})
	}
	// Back to schema 7: the CHECK without board.read.
	if _, err := s.db.Exec(`
CREATE TABLE c7(
 id TEXT PRIMARY KEY,
 service_id TEXT NOT NULL REFERENCES identity_peers(service_id),
 digest TEXT NOT NULL UNIQUE,
 created_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL,
 revoked INTEGER NOT NULL DEFAULT 0 CHECK(revoked IN (0,1)),
 operation TEXT NOT NULL DEFAULT 'identity.redeem'
  CHECK(operation IN ('identity.redeem','reservation.read','team.register','team.read'))
);
INSERT INTO c7 SELECT id,service_id,digest,created_at,expires_at,revoked,operation FROM identity_peer_credentials;
DROP TABLE identity_peer_credentials;
ALTER TABLE c7 RENAME TO identity_peer_credentials;
PRAGMA user_version=7;`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.IssuePeerCredential("admin", "aicrew-example", PeerOperationBoardRead, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("the schema-7 table took a board.read credential")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 { // migration, then an ordinary reopen
		if s, err = Open(root); err != nil {
			t.Fatal(err)
		}
		var version int
		if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != accessSchema || accessSchema != 8 {
			t.Fatalf("schema version %d: %v", version, err)
		}
		for _, c := range creds {
			p, err := s.AuthenticatePeer(c.secret)
			if err != nil || p.CredentialID != c.id || p.Operation != c.op {
				t.Fatalf("a migrated %s credential: %+v %v", c.op, p, err)
			}
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if s, err = Open(root); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, secret, err := s.IssuePeerCredential("admin", "aicrew-example", PeerOperationBoardRead, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("issue board.read after the migration: %v", err)
	}
	if p, err := s.AuthenticatePeer(secret); err != nil || p.CredentialID != c.ID || p.Operation != PeerOperationBoardRead {
		t.Fatalf("a board.read credential authenticates as %+v: %v", p, err)
	}
}
