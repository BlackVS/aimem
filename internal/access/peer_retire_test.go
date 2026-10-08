package access

import (
	"context"
	"errors"
	"testing"
	"time"
)

const retireTeam = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"

// teamRegistrar authenticates a new team.register credential of service.
func (e *identityEnv) teamRegistrar(t *testing.T, service string) PeerIdentity {
	t.Helper()
	_, secret, err := e.s.IssuePeerCredential("admin", service, PeerOperationTeamRegister, e.now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	p, err := e.s.AuthenticatePeer(secret)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// retireEnv is an identity env whose peer holds a registered team with two
// project grants, a second credential, a redeemed proof and a live one.
func retireEnv(t *testing.T) (*identityEnv, ProofReceipt, string) {
	t.Helper()
	e := newIdentityEnv(t)
	reg, err := e.s.RegisterTeam("peer:aicrew-example", e.teamRegistrar(t, "aicrew-example"), retireTeam, "pilot")
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"alpha", "beta"} {
		if err := e.s.SetTeamGrant("admin", project, reg.Profile.ID, true); err != nil {
			t.Fatal(err)
		}
	}
	redeemed := e.proof(t, "01a0dbee-0000-7000-8000-00000000c101")
	key := fixtureKey("retire-key")
	if _, err := e.redeem(redeemed, key); err != nil {
		t.Fatal(err)
	}
	e.proof(t, "01a0dbee-0000-7000-8000-00000000c102")
	return e, redeemed, key
}

func TestRetireRefusesAnEnabledPeer(t *testing.T) {
	e, _, _ := retireEnv(t)
	if _, err := e.s.RetireIdentityPeer("admin", "aicrew-example"); !errors.Is(err, ErrPeerEnabled) {
		t.Fatalf("retiring an enabled peer: %v", err)
	}
	if _, err := e.s.RetireIdentityPeer("admin", "no-such-peer"); !errors.Is(err, ErrPeerUnknown) {
		t.Fatalf("retiring an unknown peer: %v", err)
	}
	// Nothing was removed: the peer still authenticates and holds its team.
	if _, err := e.s.AuthenticatePeer(e.peerCred); err != nil {
		t.Errorf("the refused retirement touched the peer: %v", err)
	}
	if ps, err := e.s.ListTeamProfiles("aicrew-example"); err != nil || len(ps) != 1 {
		t.Errorf("the refused retirement touched the team profiles: %v %v", ps, err)
	}
	var n int
	if err := e.s.db.QueryRow("SELECT count(*) FROM audit WHERE action='identity_peer.retire'").Scan(&n); err != nil || n != 0 {
		t.Errorf("a refused retirement was audited as done: %d %v", n, err)
	}
}

func TestRetireRemovesThePeerStateAndAuditsTheCounts(t *testing.T) {
	e, _, _ := retireEnv(t)
	if err := e.s.SetIdentityPeerDisabled("admin", "aicrew-example", true); err != nil {
		t.Fatal(err)
	}
	var auditBefore int
	if err := e.s.db.QueryRow("SELECT count(*) FROM audit").Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}
	got, err := e.s.RetireIdentityPeer("admin", "aicrew-example")
	if err != nil {
		t.Fatal(err)
	}
	want := PeerRetirement{Credentials: 2, TeamProfiles: 1, TeamGrants: 2, Receipts: 2, Redemptions: 1}
	if got != want {
		t.Fatalf("retired %+v, want %+v", got, want)
	}
	for _, table := range []string{"identity_peers", "identity_peer_credentials", "team_access_profiles", "identity_receipts", "identity_redemptions"} {
		var n int
		if err := e.s.db.QueryRow("SELECT count(*) FROM " + table + " WHERE service_id='aicrew-example'").Scan(&n); err != nil || n != 0 {
			t.Errorf("%s keeps %d rows of the retired peer (%v)", table, n, err)
		}
	}
	var grants int
	if err := e.s.db.QueryRow("SELECT count(*) FROM team_profile_grants").Scan(&grants); err != nil || grants != 0 {
		t.Errorf("team_profile_grants keeps %d rows (%v)", grants, err)
	}
	// One audit row is added and none is removed.
	var auditAfter int
	if err := e.s.db.QueryRow("SELECT count(*) FROM audit").Scan(&auditAfter); err != nil || auditAfter != auditBefore+1 {
		t.Errorf("audit rows %d -> %d, want one more (%v)", auditBefore, auditAfter, err)
	}
	var actor, subject string
	if err := e.s.db.QueryRow("SELECT actor,subject FROM audit WHERE action='identity_peer.retire'").Scan(&actor, &subject); err != nil {
		t.Fatal(err)
	}
	if actor != "admin" || subject != "service=aicrew-example credentials=2 team_profiles=1 team_grants=2 receipts=2 redemptions=1" {
		t.Errorf("audit %q %q", actor, subject)
	}
	if _, err := e.s.AuthenticatePeer(e.peerCred); err == nil {
		t.Error("a credential of the retired peer still authenticates")
	}
}

// After retirement the service ID and the team it held register again, and
// a replay of a redemption made under the retired peer reads as unknown.
func TestRetiredPeerNameAndTeamRegisterAgain(t *testing.T) {
	e, redeemed, key := retireEnv(t)
	if err := e.s.SetIdentityPeerDisabled("admin", "aicrew-example", true); err != nil {
		t.Fatal(err)
	}
	// The renamed peer cannot take the team while the old profile exists.
	e.registerPeer(t, "aicrew-renamed")
	renamed := e.teamRegistrar(t, "aicrew-renamed")
	if _, err := e.s.RegisterTeam("peer:aicrew-renamed", renamed, retireTeam, "pilot"); !errors.Is(err, ErrPeerForbidden) {
		t.Fatalf("team.register under another peer before retirement: %v", err)
	}
	if _, err := e.s.RetireIdentityPeer("admin", "aicrew-example"); err != nil {
		t.Fatal(err)
	}
	reg, err := e.s.RegisterTeam("peer:aicrew-renamed", renamed, retireTeam, "pilot")
	if err != nil || !reg.Created {
		t.Fatalf("team.register of the retired peer's team: %+v %v", reg, err)
	}

	// The retired name registers again (the renamed peer steps aside first:
	// one active peer per hub) and its replay finds nothing.
	if err := e.s.SetIdentityPeerDisabled("admin", "aicrew-renamed", true); err != nil {
		t.Fatal(err)
	}
	e.registerPeer(t, "aicrew-example")
	_, secret := e.issueCredential(t, "aicrew-example")
	again, err := e.s.AuthenticatePeer(secret)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.s.RedeemProof(context.Background(), again, RedeemRequest{HubID: redeemed.HubID, ChallengeID: redeemed.ChallengeID, Receipt: redeemed.Receipt, RequestKey: key})
	if !errors.Is(err, ErrProofInvalid) {
		t.Fatalf("replay of a redemption made under the retired peer: %v", err)
	}
	var action string
	if err := e.s.db.QueryRow("SELECT action FROM audit WHERE actor='peer:aicrew-example' ORDER BY id DESC LIMIT 1").Scan(&action); err != nil {
		t.Fatal(err)
	}
	if action != "identity.redeem.refused.unknown" {
		t.Errorf("the replay was refused as %q, want unknown", action)
	}
}

// A team.register that authenticated before its peer was retired, and
// reaches the store after, writes nothing: it cannot leave a profile behind
// that keeps the team from the replacement peer.
func TestTeamRegisterAuthenticatedBeforeRetirementWritesNothing(t *testing.T) {
	e := newIdentityEnv(t)
	inFlight := e.teamRegistrar(t, "aicrew-example")
	if err := e.s.SetIdentityPeerDisabled("admin", "aicrew-example", true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RetireIdentityPeer("admin", "aicrew-example"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RegisterTeam("peer:aicrew-example", inFlight, retireTeam, "pilot"); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Fatalf("team.register resumed after retirement: %v", err)
	}
	var n int
	if err := e.s.db.QueryRow("SELECT count(*) FROM team_access_profiles WHERE service_id='aicrew-example'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("the resumed team.register left %d profiles (%v)", n, err)
	}
	var refused int
	if err := e.s.db.QueryRow("SELECT count(*) FROM audit WHERE action='team.register.refused.peer_unauthenticated' AND actor='peer:aicrew-example'").Scan(&refused); err != nil || refused != 1 {
		t.Errorf("the refusal was audited %d times (%v)", refused, err)
	}
	e.registerPeer(t, "aicrew-renamed")
	if reg, err := e.s.RegisterTeam("peer:aicrew-renamed", e.teamRegistrar(t, "aicrew-renamed"), retireTeam, "pilot"); err != nil || !reg.Created {
		t.Fatalf("the replacement peer's team.register: %+v %v", reg, err)
	}
}

// The same check covers a credential revoked and a peer disabled while the
// request was in flight.
func TestTeamRegisterRechecksThePeerCredential(t *testing.T) {
	e := newIdentityEnv(t)
	revoked := e.teamRegistrar(t, "aicrew-example")
	if err := e.s.RevokePeerCredential("admin", "aicrew-example", revoked.CredentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RegisterTeam("peer:aicrew-example", revoked, retireTeam, "pilot"); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Errorf("team.register with a credential revoked in flight: %v", err)
	}
	live := e.teamRegistrar(t, "aicrew-example")
	if err := e.s.SetIdentityPeerDisabled("admin", "aicrew-example", true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.s.RegisterTeam("peer:aicrew-example", live, retireTeam, "pilot"); !errors.Is(err, ErrPeerUnauthenticated) {
		t.Errorf("team.register of a peer disabled in flight: %v", err)
	}
	if ps, err := e.s.ListTeamProfiles("aicrew-example"); err != nil || len(ps) != 0 {
		t.Errorf("refused registrations left profiles: %v %v", ps, err)
	}
}
