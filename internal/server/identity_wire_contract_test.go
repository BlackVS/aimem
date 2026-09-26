package server

// These tests check the E2 identity design fixtures as a fake consumer. They
// do not implement proof issuance, redemption, introspection or peer
// registration; E3 and E4 own those gates.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func readIdentityFixture(t *testing.T, name string, out any) {
	t.Helper()
	p := filepath.Join("..", "..", "docs", "fixtures", "identity-v1", name)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
}

type identityExchange struct {
	Case      string `json:"case"`
	Operation string `json:"operation"`
	Caller    string `json:"caller"`
	HTTP      struct {
		Method  string            `json:"method"`
		Path    string            `json:"path"`
		Owner   string            `json:"owner"`
		Headers map[string]string `json:"headers"`
	} `json:"http"`
	MCPTool  string                     `json:"mcp_tool"`
	Request  map[string]json.RawMessage `json:"request"`
	Response struct {
		Status int                        `json:"status"`
		Body   map[string]json.RawMessage `json:"body"`
	} `json:"response"`
}

type identityRefusal struct {
	Case       string `json:"case"`
	Operation  string `json:"operation"`
	Code       string `json:"code"`
	HTTPStatus int    `json:"http_status"`
	Retryable  bool   `json:"retryable"`
	NextAction string `json:"next_action"`
}

type identityExamples struct {
	Version       int               `json:"version"`
	Contract      string            `json:"contract"`
	Status        string            `json:"status"`
	SampleSecrets map[string]string `json:"sample_secrets"`
	Bounds        map[string]int    `json:"bounds"`
	KeyEncoding   struct {
		Cases []struct {
			Case string `json:"case"`
			Raw  string `json:"raw_request_key"`
			Key  string `json:"idempotency_key"`
		} `json:"cases"`
	} `json:"request_key_encoding"`
	PeerRegistration struct {
		AuthorizedBy  string          `json:"authorized_by"`
		Audited       bool            `json:"audited"`
		Record        json.RawMessage `json:"record"`
		GrantsNothing []string        `json:"grants_nothing"`
	} `json:"peer_registration"`
	Exchanges   []identityExchange `json:"exchanges"`
	TeamRequest struct {
		Headers       map[string]string `json:"headers"`
		Authenticated struct {
			HubID   string `json:"hub_id"`
			UserID  string `json:"user_id"`
			TokenID string `json:"token_id"`
		} `json:"authenticated"`
		LinkedProfile struct {
			ServiceID string `json:"service_id"`
			TeamID    string `json:"team_id"`
		} `json:"linked_profile"`
		VerifierOrder []string        `json:"verifier_order"`
		AuditExample  json.RawMessage `json:"audit_example"`
	} `json:"team_request"`
	ConcurrentSessions []struct {
		Connection         string `json:"connection"`
		HandleAttached     bool   `json:"handle_attached"`
		IntrospectionCalls int    `json:"introspection_calls"`
		Grants             string `json:"grants"`
		SessionID          string `json:"session_id"`
	} `json:"concurrent_sessions"`
	RotationSequence []struct {
		Step                 string `json:"step"`
		Code                 string `json:"code"`
		IntrospectionCalls   *int   `json:"introspection_calls"`
		IntrospectedTokenID  string `json:"introspected_token_id"`
		AuthenticatedTokenID string `json:"authenticated_token_id"`
	} `json:"rotation_sequence"`
	RefusalEnvelope json.RawMessage   `json:"refusal_envelope_example"`
	Refusals        []identityRefusal `json:"refusals"`
}

func TestIdentityV1FixtureAndProposedSurface(t *testing.T) {
	var ex identityExamples
	readIdentityFixture(t, "examples.json", &ex)
	if ex.Version != 1 || ex.Contract != "identity.v1" || ex.Status != "proposal_only" {
		t.Fatalf("unversioned or live fixture: %d %q %q", ex.Version, ex.Contract, ex.Status)
	}
	var spec struct {
		OpenAPI       string                                `json:"openapi"`
		Version       string                                `json:"x-contract-version"`
		SecurityGate  string                                `json:"x-security-gate"`
		Parity        map[string]string                     `json:"x-mcp-parity"`
		ServiceOnly   []string                              `json:"x-service-only"`
		RefusalStatus map[string]int                        `json:"x-refusal-status"`
		PeerRoutes    map[string]json.RawMessage            `json:"x-peer-routes"`
		Paths         map[string]map[string]json.RawMessage `json:"paths"`
	}
	readIdentityFixture(t, "openapi-proposal.json", &spec)
	if spec.OpenAPI != "3.1.0" || spec.Version != "identity.v1" {
		t.Fatalf("unexpected proposed OpenAPI version: %q %q", spec.OpenAPI, spec.Version)
	}
	if !strings.Contains(spec.SecurityGate, "E3") || !strings.Contains(spec.SecurityGate, "E4") {
		t.Fatal("proposal must gate registration on E3/E4")
	}

	checkIdentityBounds(t, ex.Bounds)
	checkIdentityKeyEncoding(t, ex)
	checkIdentityExchanges(t, ex, spec.Parity, spec.ServiceOnly, spec.Paths, spec.PeerRoutes)
	checkIdentityRefusals(t, ex.Refusals, spec.RefusalStatus)
	checkIdentityNoSecrets(t, ex)

	if ex.PeerRegistration.AuthorizedBy != "hub_admin" || !ex.PeerRegistration.Audited {
		t.Error("peer registration must be an audited hub-admin operation")
	}
	for _, g := range []string{"project_access", "enrollment", "token_issuance", "agent_action"} {
		if !slices.Contains(ex.PeerRegistration.GrantsNothing, g) {
			t.Errorf("peer registration must not grant %s", g)
		}
	}
	var record struct {
		Operations []string `json:"operations"`
		TLSTrust   struct {
			Mode string `json:"mode"`
		} `json:"tls_trust"`
		Endpoint string `json:"introspection_endpoint"`
		Inbound  []struct {
			DigestOnly bool `json:"digest_only"`
		} `json:"inbound_credentials"`
	}
	if err := json.Unmarshal(ex.PeerRegistration.Record, &record); err != nil {
		t.Fatal(err)
	}
	if len(record.Operations) != 1 || record.Operations[0] != "identity.redeem" {
		t.Errorf("peer operations must be exactly identity.redeem, got %v", record.Operations)
	}
	if record.TLSTrust.Mode != "spki_sha256" && record.TLSTrust.Mode != "ca_dns" {
		t.Errorf("unsupported TLS trust mode %q", record.TLSTrust.Mode)
	}
	if !strings.HasPrefix(record.Endpoint, "https://") {
		t.Error("introspection endpoint must use TLS")
	}
	for _, c := range record.Inbound {
		if !c.DigestOnly {
			t.Error("inbound peer credentials must be stored as digests only")
		}
	}

	tr := ex.TeamRequest
	if tr.Headers["X-Aimem-Team-Context"] != "{aimem_handle}" || tr.Headers["Authorization"] != "Bearer {individual_bearer}" {
		t.Error("team request must carry both the individual bearer and the aimem-scoped handle")
	}
	wantOrder := []string{"authenticate_individual_bearer", "introspect_handle", "compare_exact_binding", "evaluate_profile_grants_only", "role_and_reservation_policy"}
	if strings.Join(tr.VerifierOrder, ",") != strings.Join(wantOrder, ",") {
		t.Errorf("verifier order %v", tr.VerifierOrder)
	}

	personal, teams := 0, map[string]bool{}
	for _, s := range ex.ConcurrentSessions {
		if !s.HandleAttached {
			personal++
			if s.IntrospectionCalls != 0 || s.Grants != "personal_only" {
				t.Errorf("%s: personal mode must not call aicrew or union grants", s.Connection)
			}
			continue
		}
		if s.IntrospectionCalls != 1 || s.Grants != "profile_only" || s.SessionID == "" {
			t.Errorf("%s: team mode must introspect once and use profile grants only", s.Connection)
		}
		if teams[s.SessionID] {
			t.Errorf("%s: two connections share one session", s.Connection)
		}
		teams[s.SessionID] = true
	}
	if personal == 0 || len(teams) < 2 {
		t.Error("concurrent sessions must cover one personal and two isolated team connections")
	}

	steps := map[string]bool{}
	for _, s := range ex.RotationSequence {
		steps[s.Step] = true
		switch s.Step {
		case "old_receipt_redeemed":
			if s.Code != "credential_inactive" {
				t.Error("an outstanding receipt for a revoked token must fail redemption")
			}
		case "request_with_old_token":
			if s.Code != "invalid_credential" || s.IntrospectionCalls == nil || *s.IntrospectionCalls != 0 {
				t.Error("a revoked token must fail before introspection")
			}
		case "request_with_new_token_old_session":
			if s.Code != "context_stale" || s.IntrospectedTokenID == s.AuthenticatedTokenID {
				t.Error("same user with a new token must be context_stale until re-proof")
			}
		}
	}
	for _, s := range []string{"d1_reissue_commits", "old_receipt_redeemed", "request_with_old_token", "request_with_new_token_old_session", "aicrew_reproof", "request_with_new_token_new_handle"} {
		if !steps[s] {
			t.Errorf("rotation sequence missing %s", s)
		}
	}

	var env map[string]json.RawMessage
	if err := json.Unmarshal(ex.RefusalEnvelope, &env); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"code", "message", "active_mode", "retryable", "next_action", "correlation_id"} {
		if _, ok := env[f]; !ok {
			t.Errorf("refusal envelope missing %s", f)
		}
	}

	// E3 alone registers the routes. A fixture must not accidentally become a
	// production surface through a copied route or embedded OpenAPI entry.
	s, _ := testServer(t)
	for _, route := range s.Routes() {
		if strings.Contains(route.Pattern, "/v1/identity/") || strings.Contains(route.Pattern, "/v1/crew/") {
			t.Errorf("E2 unexpectedly registered route %s", route.Pattern)
		}
	}
	var live struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPISpec, &live); err != nil {
		t.Fatal(err)
	}
	for path := range live.Paths {
		if strings.HasPrefix(path, "/v1/identity/") || strings.HasPrefix(path, "/v1/crew/") {
			t.Errorf("E2 unexpectedly changed live OpenAPI path %s", path)
		}
	}
}

func checkIdentityBounds(t *testing.T, b map[string]int) {
	t.Helper()
	maxima := map[string]int{
		"challenge_max_seconds":                  300,
		"receipt_max_seconds":                    60,
		"live_receipts_per_token_peer_challenge": 1,
		"introspection_attempts":                 1,
		"introspection_budget_ms":                2000,
		"handle_max_seconds":                     900,
		"handle_refresh_overlap_seconds":         60,
	}
	for k, max := range maxima {
		if v, ok := b[k]; !ok || v <= 0 || v > max {
			t.Errorf("bound %s = %d, want 1..%d", k, v, max)
		}
	}
	for _, k := range []string{"receipts_per_token_per_minute", "redemption_retention_seconds", "server_same_key_wait_seconds", "redemption_call_timeout_seconds", "aicrew_inflight_wait_seconds", "introspection_max_response_bytes"} {
		if b[k] <= 0 {
			t.Errorf("bound %s must be positive", k)
		}
	}
	// A lost redemption reply must stay recoverable for the whole challenge
	// window plus aicrew's in-flight wait.
	if b["redemption_retention_seconds"] < b["challenge_max_seconds"]+b["aicrew_inflight_wait_seconds"] {
		t.Error("redemption retention shorter than challenge window plus aicrew in-flight wait")
	}
	if b["server_same_key_wait_seconds"] >= b["redemption_call_timeout_seconds"] ||
		b["redemption_call_timeout_seconds"] >= b["aicrew_inflight_wait_seconds"] {
		t.Error("server same-key wait < redemption call timeout < aicrew in-flight wait must hold")
	}
	if b["receipt_max_seconds"] >= b["challenge_max_seconds"] {
		t.Error("receipt must expire before the challenge")
	}
}

// identityRequestKey is the v1 header encoding of an aicrew request key, whose
// domain (any valid UTF-8 up to aicrew's limits) cannot travel raw in a header.
func identityRequestKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "k1_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

var identityKeyPattern = regexp.MustCompile(`^k1_[A-Za-z0-9_-]{43}$`)

func checkIdentityKeyEncoding(t *testing.T, ex identityExamples) {
	t.Helper()
	seen, keys := map[string]bool{}, map[string]string{}
	for _, c := range ex.KeyEncoding.Cases {
		seen[c.Case] = true
		if got := identityRequestKey(c.Raw); got != c.Key {
			t.Errorf("%s: encoded key %q, want %q", c.Case, c.Key, got)
		}
		if !identityKeyPattern.MatchString(c.Key) {
			t.Errorf("%s: encoded key is not header-safe", c.Case)
		}
		if other, dup := keys[c.Key]; dup && other != c.Raw {
			t.Errorf("%s: two raw keys share one encoding", c.Case)
		}
		keys[c.Key] = c.Raw
	}
	for _, c := range []string{"ascii", "space", "non_ascii", "control_character", "max_length_key"} {
		if !seen[c] {
			t.Errorf("request key encoding missing %s case", c)
		}
	}
	for _, e := range ex.Exchanges {
		if e.Operation != "redeem" {
			continue
		}
		k := e.HTTP.Headers["Idempotency-Key"]
		if !identityKeyPattern.MatchString(k) || keys[k] == "" {
			t.Errorf("%s: Idempotency-Key must be the encoding of a fixture request key", e.Case)
		}
	}
}

func checkIdentityExchanges(t *testing.T, ex identityExamples, parity map[string]string, serviceOnly []string,
	paths map[string]map[string]json.RawMessage, peerRoutes map[string]json.RawMessage) {
	t.Helper()
	byCase := map[string]identityExchange{}
	for _, e := range ex.Exchanges {
		byCase[e.Case] = e
		if e.Response.Status != 200 {
			t.Errorf("%s: success example must be 200", e.Case)
		}
		if e.Operation == "introspect" {
			if e.HTTP.Owner != "aicrew" {
				t.Errorf("%s: introspection is an aicrew-owned route", e.Case)
			}
			if _, ok := peerRoutes[e.HTTP.Path]; !ok {
				t.Errorf("%s: introspection path %s missing from x-peer-routes", e.Case, e.HTTP.Path)
			}
			continue
		}
		if e.HTTP.Headers["X-Aimem-Identity-Version"] != "1" {
			t.Errorf("%s: missing identity version header", e.Case)
		}
		var op struct {
			Operation string                     `json:"x-operation"`
			MCPTool   string                     `json:"x-mcp-tool"`
			Security  []map[string][]string      `json:"security"`
			Responses map[string]json.RawMessage `json:"responses"`
		}
		found := false
		for path, methods := range paths {
			if identityPathMatches(path, e.HTTP.Path) {
				if raw, ok := methods[strings.ToLower(e.HTTP.Method)]; ok && json.Unmarshal(raw, &op) == nil {
					found = true
				}
			}
		}
		if !found || op.Operation != e.Operation {
			t.Errorf("%s: %s %s not in proposed OpenAPI", e.Case, e.HTTP.Method, e.HTTP.Path)
			continue
		}
		if len(op.Security) != 1 {
			t.Errorf("%s: proposed operation must name exactly one security scheme", e.Case)
		}
		if slices.Contains(serviceOnly, e.Operation) {
			if e.MCPTool != "" || op.MCPTool != "" || e.Caller != "peer" {
				t.Errorf("%s: service-only operation exposed to agents", e.Case)
			}
			if _, ok := op.Security[0]["PeerRedemptionBearer"]; !ok {
				t.Errorf("%s: redemption must use the peer credential", e.Case)
			}
			if e.HTTP.Headers["Idempotency-Key"] == "" {
				t.Errorf("%s: redemption needs an idempotency key", e.Case)
			}
		} else {
			if e.MCPTool == "" || e.MCPTool != parity[e.Operation] || op.MCPTool != e.MCPTool {
				t.Errorf("%s: missing HTTP/MCP parity", e.Case)
			}
			if _, ok := op.Security[0]["IndividualBearer"]; !ok {
				t.Errorf("%s: proof must use the individual credential", e.Case)
			}
		}
	}

	proof := byCase["proof_issue"]
	redeem := byCase["redemption"]
	replay := byCase["redemption_identical_replay"]
	active := byCase["introspection_active"]
	inactive := byCase["introspection_inactive"]
	for _, c := range []string{"proof_issue", "redemption", "redemption_identical_replay", "introspection_active", "introspection_inactive"} {
		if _, ok := byCase[c]; !ok {
			t.Fatalf("missing exchange %s", c)
		}
	}
	var binding map[string]string
	_ = json.Unmarshal(proof.Response.Body["binding"], &binding)
	for _, f := range []string{"hub_id", "peer_service_id", "challenge_id", "user_id", "token_id"} {
		if binding[f] == "" {
			t.Errorf("proof binding missing %s", f)
		}
	}
	if identityRaw(proof.Request["challenge_id"]) != binding["challenge_id"] || identityRaw(proof.Request["peer_service_id"]) != binding["peer_service_id"] {
		t.Error("proof binding does not match the requested peer and challenge")
	}
	if !regexp.MustCompile(`^"amr1_[A-Za-z0-9_-]{43}"$`).Match(ex.sampleJSON("receipt")) {
		t.Error("sample receipt does not match the v1 receipt format")
	}
	if !regexp.MustCompile(`^"acs1_[A-Za-z0-9_-]{43}"$`).Match(ex.sampleJSON("aimem_handle")) {
		t.Error("sample handle does not match the v1 handle format")
	}

	var identity map[string]string
	_ = json.Unmarshal(redeem.Response.Body["identity"], &identity)
	if len(identity) != 3 || identity["hub_id"] != binding["hub_id"] || identity["user_id"] != binding["user_id"] || identity["token_id"] != binding["token_id"] {
		t.Errorf("redemption identity must be exactly the proof binding's hub/user/token: %v", identity)
	}
	if identityRaw(redeem.Request["challenge_id"]) != binding["challenge_id"] || !strings.HasSuffix(redeem.HTTP.Path, "/"+binding["peer_service_id"]+"/redemptions") {
		t.Error("redemption must name the bound peer and challenge")
	}
	if identityRaw(redeem.Response.Body["request_key"]) != redeem.HTTP.Headers["Idempotency-Key"] || identityRaw(redeem.Response.Body["token_state"]) != "active" {
		t.Error("redemption must echo its request key and report active token state")
	}
	if string(redeem.Response.Body["replayed"]) != "false" || string(replay.Response.Body["replayed"]) != "true" {
		t.Error("replayed flag wrong on original or identical retry")
	}
	if replay.HTTP.Headers["Idempotency-Key"] != redeem.HTTP.Headers["Idempotency-Key"] ||
		identityRaw(replay.Response.Body["redemption_id"]) != identityRaw(redeem.Response.Body["redemption_id"]) {
		t.Error("identical replay must reuse the key and return the original redemption")
	}
	for _, f := range []string{"display_name", "grants", "membership", "role", "profile_id"} {
		if _, ok := redeem.Response.Body[f]; ok {
			t.Errorf("redemption discloses %s", f)
		}
	}

	for _, e := range []identityExchange{active, inactive} {
		for _, f := range []string{"user_id", "token_id", "team_id", "role", "identity", "expected"} {
			if _, ok := e.Request[f]; ok {
				t.Errorf("%s: aimem must not send %s for aicrew to echo", e.Case, f)
			}
		}
		if identityRaw(e.Request["nonce"]) == "" || identityRaw(e.Request["nonce"]) != identityRaw(e.Response.Body["nonce"]) {
			t.Errorf("%s: reply not bound to request nonce", e.Case)
		}
	}
	if len(inactive.Response.Body) != 2 || string(inactive.Response.Body["active"]) != "false" {
		t.Error("inactive introspection must return only nonce and active=false")
	}
	for _, f := range []string{"service_id", "hub_id", "identity", "agent_id", "team_id", "role", "session_id", "generation", "handle_expires_at"} {
		if _, ok := active.Response.Body[f]; !ok {
			t.Errorf("active introspection missing %s", f)
		}
	}
	for _, f := range []string{"grants", "profile_id", "allowed", "decision"} {
		if _, ok := active.Response.Body[f]; ok {
			t.Errorf("aicrew must not return a grant decision (%s)", f)
		}
	}
	if !regexp.MustCompile(`^"[1-9][0-9]*"$`).Match(active.Response.Body["generation"]) {
		t.Error("generation must be a positive decimal string")
	}
	var aid map[string]string
	_ = json.Unmarshal(active.Response.Body["identity"], &aid)
	tr := ex.TeamRequest
	if aid["user_id"] != tr.Authenticated.UserID || aid["token_id"] != tr.Authenticated.TokenID ||
		identityRaw(active.Response.Body["hub_id"]) != tr.Authenticated.HubID ||
		identityRaw(active.Response.Body["service_id"]) != tr.LinkedProfile.ServiceID || identityRaw(active.Response.Body["team_id"]) != tr.LinkedProfile.TeamID {
		t.Error("allowed team request must match authenticated identity and linked team key exactly")
	}
}

func checkIdentityRefusals(t *testing.T, refusals []identityRefusal, status map[string]int) {
	t.Helper()
	cases, codes := map[string]bool{}, map[string]bool{}
	for _, r := range refusals {
		if cases[r.Case] {
			t.Errorf("duplicate refusal case %s", r.Case)
		}
		cases[r.Case] = true
		codes[r.Code] = true
		if r.NextAction == "" || strings.Contains(strings.ToLower(r.NextAction), "personal") {
			t.Errorf("%s: unsafe or empty next action", r.Case)
		}
		want, ok := status[r.Code]
		if !ok || want != r.HTTPStatus {
			t.Errorf("%s: %s status %d disagrees with proposed OpenAPI (%d)", r.Case, r.Code, r.HTTPStatus, want)
		}
		if r.Retryable != (r.HTTPStatus == 429 || r.HTTPStatus == 503) {
			t.Errorf("%s: retryable must match 429/503", r.Case)
		}
	}
	for code := range status {
		if !codes[code] {
			t.Errorf("proposed refusal code %s lacks an example", code)
		}
	}
	for _, c := range []string{
		"proof_revoked_token", "proof_project_scoped_token", "proof_enrollment_subcode_as_bearer", "proof_unknown_peer_or_hub", "proof_rate_limited",
		"redeem_unregistered_credential", "redeem_path_service_mismatch", "redeem_expired_receipt", "redeem_superseded_receipt",
		"redeem_wrong_peer_receipt", "redeem_wrong_challenge", "redeem_second_key_same_receipt", "redeem_replay_after_retention",
		"redeem_changed_input_same_key", "redeem_same_key_in_flight", "redeem_token_revoked_after_issue", "redeem_replay_after_revocation",
		"team_revoked_bearer", "team_user_mismatch", "team_rotation_pending", "team_generation_advanced", "team_no_handle_for_team_tool",
		"team_handle_on_personal_connection", "team_introspection_timeout", "team_introspection_tls_mismatch", "team_introspection_nonce_mismatch",
		"team_profile_not_granted", "team_role_downgrade",
	} {
		if !cases[c] {
			t.Errorf("missing refusal case %s", c)
		}
	}
}

// checkIdentityNoSecrets proves every sample secret appears only in request
// positions, plus the receipt returned to its own requester.
func checkIdentityNoSecrets(t *testing.T, ex identityExamples) {
	t.Helper()
	var secrets []string
	for _, v := range ex.SampleSecrets {
		secrets = append(secrets, v)
	}
	scan := func(where string, v any) {
		b, _ := json.Marshal(v)
		s := string(b)
		for _, secret := range secrets {
			if strings.Contains(s, secret) {
				t.Errorf("%s contains a sample secret", where)
			}
		}
		for _, marker := range []string{"{individual_bearer}", "{redemption_credential}", "{introspection_credential}", "{aimem_handle}", "Bearer "} {
			if strings.Contains(s, marker) {
				t.Errorf("%s contains credential placeholder %s", where, marker)
			}
		}
	}
	for _, e := range ex.Exchanges {
		body := maps.Clone(e.Response.Body)
		if e.Case == "proof_issue" {
			if identityRaw(body["receipt"]) != "{receipt}" {
				t.Error("proof response must return the receipt only to its requester")
			}
			delete(body, "receipt")
		}
		if strings.Contains(identityJSON(body), "{receipt}") {
			t.Errorf("%s response echoes the receipt", e.Case)
		}
		scan(e.Case+" response", body)
	}
	scan("team audit", ex.TeamRequest.AuditExample)
	scan("peer registration", ex.PeerRegistration.Record)
	scan("refusal envelope", ex.RefusalEnvelope)
	scan("refusals", ex.Refusals)
	scan("rotation sequence", ex.RotationSequence)
	scan("concurrent sessions", ex.ConcurrentSessions)
}

func (ex identityExamples) sampleJSON(name string) []byte {
	b, _ := json.Marshal(ex.SampleSecrets[name])
	return b
}

func identityPathMatches(template, path string) bool {
	tp, pp := strings.Split(template, "/"), strings.Split(path, "/")
	if len(tp) != len(pp) {
		return false
	}
	for i := range tp {
		if !strings.HasPrefix(tp[i], "{") && tp[i] != pp[i] {
			return false
		}
	}
	return true
}

func identityRaw(m json.RawMessage) string {
	var s string
	if json.Unmarshal(m, &s) == nil {
		return s
	}
	return string(m)
}

func identityJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
