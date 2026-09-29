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
	// Back to schema 4: the column did not exist.
	if _, err := s.db.Exec("ALTER TABLE identity_peer_credentials DROP COLUMN operation; PRAGMA user_version=4;"); err != nil {
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
