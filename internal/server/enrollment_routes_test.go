package server

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var ev1 = map[string]string{enrollmentVersionHeader: "1"}

const enrollBundle = "01a10c90-0000-7000-8000-000000000001"

func enrollBody(bundle string, expires time.Time) string {
	return `{"bundle_id":"` + bundle + `","purpose":"new_user","expires_at":"` + expires.UTC().Format(time.RFC3339) + `"}`
}

// The admin issues, lists and revokes a bundle over hub TLS. The subcode is
// shown once, with no-store; nothing else ever carries it.
func TestEnrollmentRoutesAdminLifecycle(t *testing.T) {
	g := newIdentityRig(t)
	expires := time.Now().Add(24 * time.Hour)
	r := g.call(t, g.tls, "POST", "/v1/identity/enrollments", g.env, ev1, enrollBody(enrollBundle, expires), false)
	if r.status != 201 || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("issue: %d %s %s", r.status, r.header.Get("Cache-Control"), r.body)
	}
	var issued struct {
		BundleID string `json:"bundle_id"`
		Purpose  string `json:"purpose"`
		HubID    string `json:"hub_id"`
		Subcode  string `json:"subcode"`
	}
	if err := json.Unmarshal(r.body, &issued); err != nil || issued.BundleID != enrollBundle || issued.Purpose != "new_user" ||
		issued.HubID != g.hub || !regexp.MustCompile(`^aes1_[A-Za-z0-9_-]{43}$`).MatchString(issued.Subcode) {
		t.Fatalf("issue answer: %+v %v", issued, err)
	}
	g.secrets = append(g.secrets, issued.Subcode)

	// A lost reply cannot be recovered: the same input is enrollment_exists,
	// other input idempotency_conflict, and neither shows a subcode.
	checkEnvelope(t, "repeat", g.call(t, g.tls, "POST", "/v1/identity/enrollments", g.env, ev1, enrollBody(enrollBundle, expires), true), 409, "enrollment_exists")
	checkEnvelope(t, "other input", g.call(t, g.tls, "POST", "/v1/identity/enrollments", g.env, ev1, enrollBody(enrollBundle, expires.Add(time.Minute)), true), 409, "idempotency_conflict")

	var list struct {
		Enrollments []struct {
			BundleID string          `json:"bundle_id"`
			State    string          `json:"state"`
			IssuedBy string          `json:"issued_by"`
			Redeemed json.RawMessage `json:"redeemed"`
		} `json:"enrollments"`
	}
	r = g.call(t, g.tls, "GET", "/v1/identity/enrollments?state=issued", g.env, ev1, "", true)
	if err := json.Unmarshal(r.body, &list); r.status != 200 || err != nil || len(list.Enrollments) != 1 ||
		list.Enrollments[0].State != "issued" || string(list.Enrollments[0].Redeemed) != "null" {
		t.Fatalf("list issued: %d %s", r.status, r.body)
	}

	r = g.call(t, g.tls, "POST", "/v1/identity/enrollments/"+enrollBundle+"/revocation", g.env, ev1, "", true)
	if r.status != 200 || !strings.Contains(string(r.body), `"state":"revoked"`) {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "POST", "/v1/identity/enrollments/"+enrollBundle+"/revocation", g.env, ev1, "", true); r.status != 200 {
		t.Fatalf("revoke again: %d %s", r.status, r.body)
	}
	checkEnvelope(t, "unknown bundle", g.call(t, g.tls, "POST", "/v1/identity/enrollments/01a10c90-0000-7000-8000-00000000ffff/revocation", g.env, ev1, "", true), 404, "not_found")

	// A redeemed bundle (as D1-b will record it) names what it issued.
	redeemed := "01a10c90-0000-7000-8000-000000000002"
	if r := g.call(t, g.tls, "POST", "/v1/identity/enrollments", g.env, ev1, enrollBody(redeemed, expires), false); r.status != 201 {
		t.Fatalf("issue second: %d %s", r.status, r.body)
	}
	markRedeemed(t, g, redeemed)
	r = g.call(t, g.tls, "POST", "/v1/identity/enrollments/"+redeemed+"/revocation", g.env, ev1, "", true)
	checkEnvelope(t, "revoke redeemed", r, 409, "enrollment_redeemed")
	var ref struct {
		Redeemed struct {
			UserID  string `json:"user_id"`
			TokenID string `json:"token_id"`
		} `json:"redeemed"`
	}
	if json.Unmarshal(r.body, &ref) != nil || ref.Redeemed.UserID != "u-1" || ref.Redeemed.TokenID != "t-1" {
		t.Fatalf("redeemed refusal: %s", r.body)
	}
	g.assertNoSecretLeak(t)
}

// markRedeemed records a redemption the way D1-b will, through a second
// connection to the hub's access store.
func markRedeemed(t *testing.T, g *identityRig, bundle string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(g.s.reg.Root(), "access.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE enrollments SET redeemed_at=?, redeemed_user_id='u-1', redeemed_token_id='t-1' WHERE bundle_id=?",
		time.Now().Unix(), bundle); err != nil {
		t.Fatal(err)
	}
}

// Only the hub admin over hub TLS, with the enrollment version, reaches the
// routes; every other caller gets enrollment.v1's envelope.
func TestEnrollmentRoutesRefuseOtherCallers(t *testing.T) {
	g := newIdentityRig(t)
	g.registerPeer(t, "aicrew-example")
	_, peer := g.issueCredential(t, "aicrew-example", time.Now().Add(time.Hour))
	body := enrollBody(enrollBundle, time.Now().Add(time.Hour))
	for _, path := range []struct{ method, path string }{
		{"POST", "/v1/identity/enrollments"},
		{"GET", "/v1/identity/enrollments"},
		{"POST", "/v1/identity/enrollments/" + enrollBundle + "/revocation"},
	} {
		name := path.method + " " + path.path
		checkEnvelope(t, name+" user token", g.call(t, g.tls, path.method, path.path, g.alice, ev1, body, true), 403, "credential_scope_forbidden")
		checkEnvelope(t, name+" project token", g.call(t, g.tls, path.method, path.path, g.project, ev1, body, true), 403, "credential_scope_forbidden")
		checkEnvelope(t, name+" peer credential", g.call(t, g.tls, path.method, path.path, peer, ev1, body, true), 403, "peer_forbidden")
		checkEnvelope(t, name+" plain HTTP", g.call(t, g.plain, path.method, path.path, g.env, ev1, body, true), 403, "tls_required")
		checkEnvelope(t, name+" no version", g.call(t, g.tls, path.method, path.path, g.env, nil, body, true), 400, "unsupported_version")
	}
	for name, b := range map[string]string{
		"reissue purpose": `{"bundle_id":"` + enrollBundle + `","purpose":"existing_user_reissue","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`,
		"beyond 72 hours": enrollBody(enrollBundle, time.Now().Add(73*time.Hour)),
		"unknown field":   `{"bundle_id":"` + enrollBundle + `","purpose":"new_user","expires_at":"2030-01-01T00:00:00Z","team":"x"}`,
		"not json":        `{`,
	} {
		checkEnvelope(t, name, g.call(t, g.tls, "POST", "/v1/identity/enrollments", g.env, ev1, b, true), 400, "invalid_request")
	}
	checkEnvelope(t, "unknown state", g.call(t, g.tls, "GET", "/v1/identity/enrollments?state=lost", g.env, ev1, "", true), 400, "invalid_request")
	// D1 serves no public redemption until the whole path is verified (D1-b).
	if r := g.call(t, g.tls, "POST", "/v1/identity/enrollments/redemptions", g.env, ev1, `{}`, true); r.status != 404 && r.status != 405 {
		t.Fatalf("redemption route served: %d %s", r.status, r.body)
	}
	g.assertNoSecretLeak(t)
}
