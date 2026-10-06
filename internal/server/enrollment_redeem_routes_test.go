package server

import (
	"crypto/ecdh"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aimem/internal/enrollment"
)

// redeemer is one client's side of a redemption over the wire.
type redeemer struct {
	key     *ecdh.PrivateKey
	k1      string
	hub     string
	subcode string
}

func newRedeemer(t *testing.T, hub, subcode, rawKey string) redeemer {
	t.Helper()
	key, err := enrollment.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(rawKey))
	return redeemer{key: key, k1: "k1_" + base64.RawURLEncoding.EncodeToString(sum[:]), hub: hub, subcode: subcode}
}

func (c redeemer) body(label string) string {
	return fmt.Sprintf(`{"hub_id":%q,"subcode":%q,"label":%q,"delivery":{"suite":%q,"public_key":%q}}`,
		c.hub, c.subcode, label, enrollment.Suite, enrollment.EncodePublicKey(c.key.PublicKey()))
}

func (c redeemer) headers() map[string]string {
	return map[string]string{enrollmentVersionHeader: "1", "Idempotency-Key": c.k1}
}

type redeemAnswer struct {
	BundleID string `json:"bundle_id"`
	Replayed bool   `json:"replayed"`
	HubID    string `json:"hub_id"`
	Identity struct {
		UserID  string `json:"user_id"`
		TokenID string `json:"token_id"`
	} `json:"identity"`
	Delivery struct {
		Suite      string `json:"suite"`
		Enc        string `json:"enc"`
		Ciphertext string `json:"ciphertext"`
	} `json:"delivery"`
}

func (c redeemer) open(t *testing.T, raw []byte) (redeemAnswer, enrollment.Plaintext) {
	t.Helper()
	var a redeemAnswer
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatal(err)
	}
	p, err := enrollment.Open(a.Delivery.Suite, c.key, enrollment.AAD(a.HubID, a.BundleID, c.k1, a.Identity.UserID, a.Identity.TokenID),
		a.Delivery.Enc, a.Delivery.Ciphertext)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return a, p
}

// issueOverTheWire issues a bundle as the admin and returns its subcode.
func issueOverTheWire(t *testing.T, g *identityRig, bundle string) string {
	t.Helper()
	r := g.call(t, g.tls, "POST", "/v1/identity/enrollments", g.env, ev1, enrollBody(bundle, time.Now().Add(time.Hour)), false)
	var issued struct {
		Subcode string `json:"subcode"`
	}
	if r.status != 201 || json.Unmarshal(r.body, &issued) != nil {
		t.Fatalf("issue: %d %s", r.status, r.body)
	}
	g.secrets = append(g.secrets, issued.Subcode)
	return issued.Subcode
}

// A redemption over hub TLS, with no bearer, delivers a token that only the
// client can open and that the hub then accepts as a user-scoped credential.
func TestEnrollmentRedeemOverTheWire(t *testing.T) {
	g := newIdentityRig(t)
	subcode := issueOverTheWire(t, g, enrollBundle)
	c := newRedeemer(t, g.hub, subcode, "request-1")
	r := g.call(t, g.tls, "POST", "/v1/identity/enrollments/redemptions", "", c.headers(), c.body("member-laptop"), true)
	if r.status != 200 || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("redeem: %d %s", r.status, r.body)
	}
	a, p := c.open(t, r.body)
	g.secrets = append(g.secrets, p.Token)
	if a.Replayed || a.BundleID != enrollBundle || p.UserID != a.Identity.UserID || p.HubID != g.hub {
		t.Fatalf("answer %+v plaintext ids %s %s", a, p.UserID, p.HubID)
	}
	id := g.call(t, g.tls, "GET", "/v1/access/identity", p.Token, nil, "", true)
	if id.status != 200 || !strings.Contains(string(id.body), a.Identity.TokenID) {
		t.Fatalf("the delivered token on the hub: %d %s", id.status, id.body)
	}
	// An identical retry replays; another key is a conflict.
	again := g.call(t, g.tls, "POST", "/v1/identity/enrollments/redemptions", "", c.headers(), c.body("member-laptop"), true)
	if _, b := c.open(t, again.body); again.status != 200 || !strings.Contains(string(again.body), `"replayed":true`) || b.TokenID != p.TokenID {
		t.Fatalf("replay: %d %s", again.status, again.body)
	}
	other := newRedeemer(t, g.hub, subcode, "request-2")
	checkEnvelope(t, "another key", g.call(t, g.tls, "POST", "/v1/identity/enrollments/redemptions", "", other.headers(), other.body("member-laptop"), true), 409, "enrollment_conflict")
	unknown := newRedeemer(t, g.hub, "aes1_"+strings.Repeat("A", 43), "k")
	checkEnvelope(t, "unknown subcode", g.call(t, g.tls, "POST", "/v1/identity/enrollments/redemptions", "", unknown.headers(), unknown.body("x"), true), 403, "enrollment_invalid")
	g.assertNoSecretLeak(t)
}

// The route evaluates no bearer: a peer credential, the admin's or garbage
// changes nothing, and the new identity is never the bearer's.
func TestEnrollmentRedeemIgnoresTheBearer(t *testing.T) {
	g := newIdentityRig(t)
	g.registerPeer(t, "aicrew-example")
	_, peer := g.issueCredential(t, "aicrew-example", time.Now().Add(time.Hour))
	for i, bearer := range []string{"garbage-bearer", peer, g.env, g.alice} {
		bundle := fmt.Sprintf("01a10c90-0000-7000-8000-00000000000%d", i+1)
		c := newRedeemer(t, g.hub, issueOverTheWire(t, g, bundle), "k")
		r := g.call(t, g.tls, "POST", "/v1/identity/enrollments/redemptions", bearer, c.headers(), c.body("member"), true)
		if r.status != 200 {
			t.Fatalf("with bearer %d: %d %s", i, r.status, r.body)
		}
		a, p := c.open(t, r.body)
		g.secrets = append(g.secrets, p.Token)
		if a.Identity.UserID == g.aliceID {
			t.Fatal("a redemption returned the bearer's identity")
		}
	}
	g.assertNoSecretLeak(t)
}

func TestEnrollmentRedeemRefusals(t *testing.T) {
	g := newIdentityRig(t)
	c := newRedeemer(t, g.hub, issueOverTheWire(t, g, enrollBundle), "k")
	checkEnvelope(t, "plain HTTP", g.call(t, g.plain, "POST", "/v1/identity/enrollments/redemptions", "", c.headers(), c.body("m"), true), 403, "tls_required")
	checkEnvelope(t, "no version", g.call(t, g.tls, "POST", "/v1/identity/enrollments/redemptions", "", map[string]string{"Idempotency-Key": c.k1}, c.body("m"), true), 400, "unsupported_version")
	noKey := map[string]string{enrollmentVersionHeader: "1"}
	checkEnvelope(t, "no request key", g.call(t, g.tls, "POST", "/v1/identity/enrollments/redemptions", "", noKey, c.body("m"), true), 400, "invalid_request")
	for name, body := range map[string]string{
		"unknown field": strings.TrimSuffix(c.body("m"), "}") + `,"extra":1}`,
		"oversized":     strings.TrimSuffix(c.body("m"), "}") + `,"label2":"` + strings.Repeat("x", enrollRedeemBodyMax) + `"}`,
		"not json":      "{",
		"bad label":     c.body("Not A Label"),
	} {
		checkEnvelope(t, name, g.call(t, g.tls, "POST", "/v1/identity/enrollments/redemptions", "", c.headers(), body, true), 400, "invalid_request")
	}
	// A valid object padded past the bound, or followed by stray data, is
	// refused before the ledger (the reader's size error and trailing
	// tokens are both caught by the end-of-body check).
	valid := c.body("m")
	for name, body := range map[string]string{
		"padded past 4096": valid + strings.Repeat(" ", enrollRedeemBodyMax+1-len(valid)),
		"trailing ]":       valid + "]",
		"trailing }":       valid + "}",
		"second object":    valid + "{}",
	} {
		checkEnvelope(t, name, g.call(t, g.tls, "POST", "/v1/identity/enrollments/redemptions", "", c.headers(), body, true), 400, "invalid_request")
	}
	// None of these spent the subcode.
	if r := g.call(t, g.tls, "POST", "/v1/identity/enrollments/redemptions", "", c.headers(), c.body("m"), true); r.status != 200 {
		t.Fatalf("after the refusals: %d %s", r.status, r.body)
	}
}

// redeemDirect calls the handler as one client address over a completed TLS
// handshake, so the per-address and per-subcode limits can be told apart.
func redeemDirect(s *Server, addr, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/identity/enrollments/redemptions", strings.NewReader(body))
	req.RemoteAddr = addr
	req.TLS = &tls.ConnectionState{HandshakeComplete: true}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.redeemEnrollment(rec, req)
	return rec
}

func TestEnrollmentRedeemRateLimits(t *testing.T) {
	g := newIdentityRig(t)
	unknown := newRedeemer(t, g.hub, "aes1_"+strings.Repeat("B", 43), "k")
	for i := range enrollment.RedemptionsPerAddress {
		if rec := redeemDirect(g.s, "192.0.2.1:4000", unknown.body("m"), unknown.headers()); rec.Code != 403 {
			t.Fatalf("request %d from one address: %d %s", i, rec.Code, rec.Body)
		}
	}
	rec := redeemDirect(g.s, "192.0.2.1:4001", unknown.body("m"), unknown.headers())
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), `"rate_limited"`) || !strings.Contains(rec.Body.String(), `"retryable":true`) {
		t.Fatalf("past the address limit: %d %s", rec.Code, rec.Body)
	}
	// One subcode from many addresses: its own limit.
	other := newRedeemer(t, g.hub, "aes1_"+strings.Repeat("C", 43), "k")
	for i := range enrollment.RedemptionsPerBundle {
		if rec := redeemDirect(g.s, fmt.Sprintf("198.51.100.%d:4000", i+1), other.body("m"), other.headers()); rec.Code != 403 {
			t.Fatalf("subcode request %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if rec := redeemDirect(g.s, "203.0.113.9:4000", other.body("m"), other.headers()); rec.Code != 429 {
		t.Fatalf("past the subcode limit: %d %s", rec.Code, rec.Body)
	}
}
