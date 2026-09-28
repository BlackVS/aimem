package server

// These tests check the C5w coordination.v1 and read-scope fixtures as a fake
// consumer. They implement nothing: C5b uses coordination.v1, C6 serves the
// read scope and the reservation CLI, and aicrew serves /v1/crew/coordination.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

func readCoordinationFixture(t *testing.T, name string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "coordination-v1", name))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

// Accessors for the generic fixture tree; a wrong shape fails the test.
func obj(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %v", what, v)
	}
	return m
}

func arr(t *testing.T, v any, what string) []any {
	t.Helper()
	a, ok := v.([]any)
	if !ok {
		t.Fatalf("%s is not an array: %v", what, v)
	}
	return a
}

func str(t *testing.T, v any, what string) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s is not a string: %v", what, v)
	}
	return s
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sha256Digest(prefix, s string) string {
	sum := sha256.Sum256([]byte(s))
	return prefix + base64.RawURLEncoding.EncodeToString(sum[:])
}

var (
	proofShape     = regexp.MustCompile(`^acp1_[A-Za-z0-9_-]{43}$`)
	proofDigest    = regexp.MustCompile(`^p1_[A-Za-z0-9_-]{43}$`)
	keyDigest      = regexp.MustCompile(`^k1_[A-Za-z0-9_-]{43}$`)
	nonceShape     = regexp.MustCompile(`^n-[0-9a-f]{32}$`)
	readCredShape  = regexp.MustCompile(`^aimem_peer_[0-9a-f]{64}$`)
	indexedSegment = regexp.MustCompile(`\[[^\]]*\]`)
)

// factKinds is the contract's table: operation, permitted acting roles and
// the references each fact must carry.
var factKinds = map[string]struct {
	op       string
	roles    []string
	requires []string
}{
	"offer":                     {"claim", []string{"coordinator"}, []string{"offer_ref", "intended_worker"}},
	"accepted_attempt":          {"transfer", []string{"worker"}, []string{"offer_ref", "attempt_ref"}},
	"never_accepted":            {"release", []string{"coordinator"}, []string{"offer_ref"}},
	"stopped":                   {"release", []string{"worker", "independent"}, []string{"attempt_ref"}},
	"accepted_for_finalization": {"finalize", []string{"worker", "independent", "coordinator"}, []string{"attempt_ref"}},
	"independent_claim":         {"claim", []string{"independent"}, []string{"attempt_ref"}},
}

func TestCoordinationV1Facts(t *testing.T) {
	ex := readCoordinationFixture(t, "examples.json")
	spec := readCoordinationFixture(t, "openapi-proposal.json")
	if ex["version"] != float64(1) || ex["contract"] != "coordination.v1" || ex["status"] != "contract" {
		t.Fatalf("unversioned fixture: %v %v %v", ex["version"], ex["contract"], ex["status"])
	}
	secrets := obj(t, ex["sample_secrets"], "sample_secrets")
	bounds := obj(t, ex["bounds"], "bounds")
	want := map[string]float64{"proof_max_seconds": 900, "coordination_attempts": 1, "coordination_budget_ms": 2000,
		"coordination_max_response_bytes": 16384, "answer_max_age_at_commit_seconds": 5, "read_none_final_after_seconds": 10,
		"read_requests_per_credential_per_minute": 60, "read_credential_max_days": 366, "read_credentials_active_per_peer": 2}
	if len(bounds) != len(want) {
		t.Errorf("bounds: %v", keysOf(bounds))
	}
	for k, v := range want {
		if bounds[k] != v {
			t.Errorf("bound %s = %v, want %v", k, bounds[k], v)
		}
	}
	// The fixture's fact table is the contract's.
	kinds := obj(t, ex["fact_kinds"], "fact_kinds")
	if len(kinds) != len(factKinds) {
		t.Fatalf("fact kinds: %v", keysOf(kinds))
	}
	for kind, rule := range factKinds {
		k := obj(t, kinds[kind], kind)
		var roles, requires []string
		for _, r := range arr(t, k["roles"], kind+".roles") {
			roles = append(roles, str(t, r, "role"))
		}
		for _, r := range arr(t, k["requires"], kind+".requires") {
			requires = append(requires, str(t, r, "requires"))
		}
		if k["operation"] != rule.op || !reflect.DeepEqual(roles, rule.roles) || !reflect.DeepEqual(requires, rule.requires) {
			t.Errorf("%s: %v %v %v", kind, k["operation"], roles, requires)
		}
	}
	if !reflect.DeepEqual(ex["no_fact_operations"], []any{"update"}) {
		t.Errorf("only a holder's update needs no fact: %v", ex["no_fact_operations"])
	}
	factSchema := obj(t, obj(t, obj(t, spec["components"], "components")["schemas"], "schemas")["Fact"], "Fact")
	allowed := obj(t, factSchema["properties"], "Fact.properties")
	seen := map[string]bool{}
	for _, e := range arr(t, ex["exchanges"], "exchanges") {
		e := obj(t, e, "exchange")
		kind := str(t, e["case"], "case")
		rule, ok := factKinds[kind]
		if !ok || seen[kind] {
			t.Fatalf("unknown or repeated fact case %q", kind)
		}
		seen[kind] = true
		http := obj(t, e["http"], kind+".http")
		headers := obj(t, http["headers"], kind+".headers")
		if http["method"] != "POST" || http["path"] != "/v1/crew/coordination" || http["owner"] != "aicrew" ||
			headers["Authorization"] != "Bearer {coordination_credential}" || headers["X-Aimem-Coordination-Version"] != "1" {
			t.Errorf("%s: request line or headers: %v %v", kind, http, headers)
		}
		req := obj(t, e["request"], kind+".request")
		if !reflect.DeepEqual(keysOf(req), []string{"hub_id", "nonce", "proof", "version"}) || req["version"] != float64(1) {
			t.Errorf("%s: aimem sends exactly version, hub, nonce and proof: %v", kind, keysOf(req))
		}
		nonce := str(t, req["nonce"], "nonce")
		placeholder := str(t, req["proof"], "proof")
		if placeholder != "{proof_"+kind+"}" || !nonceShape.MatchString(nonce) {
			t.Errorf("%s: proof or nonce: %s %s", kind, placeholder, nonce)
		}
		proof := str(t, secrets["proof_"+kind], "sample proof")
		if !proofShape.MatchString(proof) {
			t.Errorf("%s: sample proof shape %q", kind, proof)
		}
		body := obj(t, obj(t, e["response"], "response")["body"], kind+".body")
		if body["nonce"] != nonce || body["active"] != true || body["service_id"] != "aicrew-example" || body["hub_id"] != "hub-example" {
			t.Errorf("%s: active reply envelope: %v", kind, body)
		}
		fact := obj(t, body["fact"], kind+".fact")
		for name := range fact {
			if _, ok := allowed[name]; !ok {
				t.Errorf("%s: fact field %s is not in the schema", kind, name)
			}
		}
		for _, r := range arr(t, factSchema["required"], "Fact.required") {
			if _, ok := fact[str(t, r, "required")]; !ok {
				t.Errorf("%s: fact lacks %v", kind, r)
			}
		}
		for _, r := range rule.requires {
			if _, ok := fact[r]; !ok {
				t.Errorf("%s: fact lacks %s", kind, r)
			}
		}
		for _, r := range []string{"offer_ref", "attempt_ref", "intended_worker"} {
			if _, ok := fact[r]; ok && !contains(rule.requires, r) {
				t.Errorf("%s: fact carries %s it does not need", kind, r)
			}
		}
		member := obj(t, fact["member"], kind+".member")
		if fact["kind"] != kind || fact["operation"] != rule.op || !contains(rule.roles, str(t, member["role"], "role")) {
			t.Errorf("%s: kind, operation or role: %v %v %v", kind, fact["kind"], fact["operation"], member["role"])
		}
		if _, err := time.Parse(time.RFC3339, str(t, fact["expires_at"], "expires_at")); err != nil {
			t.Errorf("%s: expires_at: %v", kind, err)
		}
		// The member's own request is what aimem binds the fact to.
		mr := obj(t, e["member_request"], kind+".member_request")
		mh := obj(t, obj(t, mr["http"], "member http")["headers"], "member headers")
		key := str(t, mh["Idempotency-Key"], "request key")
		if fact["request_key_digest"] != sha256Digest("k1_", key) || !keyDigest.MatchString(str(t, fact["request_key_digest"], "digest")) {
			t.Errorf("%s: request key digest does not bind the member's key", kind)
		}
		if !strings.Contains(str(t, obj(t, mr["http"], "member http")["path"], "path"), "/tasks/"+str(t, fact["task_id"], "task")+"/reservation/"+rule.op) {
			t.Errorf("%s: fact task or operation differs from the member's request", kind)
		}
		if !reflect.DeepEqual(member, mr["verified_context"]) {
			t.Errorf("%s: fact member differs from the caller's verified context", kind)
		}
		if mr["coordination_proof"] != req["proof"] {
			t.Errorf("%s: aimem asks about another proof than the member sent", kind)
		}
		checks := obj(t, mr["checks"], kind+".checks")
		if checks["operation"] != rule.op {
			t.Errorf("%s: checks operation", kind)
		}
		holderRef := func() any {
			if h, ok := checks["holder"].(map[string]any); ok {
				if h["mode"] != "external" {
					t.Errorf("%s: a team holder is external", kind)
				}
				return h["work_ref"]
			}
			return nil
		}
		switch kind {
		case "offer":
			ok = holderRef() == fact["offer_ref"]
		case "accepted_attempt":
			ok = checks["current_work_ref"] == fact["offer_ref"] && holderRef() == fact["attempt_ref"]
		case "never_accepted":
			ok = checks["current_work_ref"] == fact["offer_ref"]
		case "stopped":
			ok = checks["current_work_ref"] == fact["attempt_ref"]
		case "accepted_for_finalization":
			ok = checks["current_work_ref"] == fact["attempt_ref"] && checks["terminal_evidence"] == true
		case "independent_claim":
			ok = holderRef() == fact["attempt_ref"]
		}
		if !ok {
			t.Errorf("%s: references do not bind the hold and the request", kind)
		}
	}
	if len(seen) != len(factKinds) {
		t.Errorf("fact exchanges cover %d of %d kinds", len(seen), len(factKinds))
	}
	// The transfer's member is the worker the offer named, which aimem
	// recorded on the hold at the offer claim.
	// Both the user and the agent must match: another agent of the same user
	// is not the intended worker.
	var offerWorker, transferMember [2]any
	for _, e := range arr(t, ex["exchanges"], "exchanges") {
		e := obj(t, e, "exchange")
		fact := obj(t, obj(t, obj(t, e["response"], "r")["body"], "b")["fact"], "fact")
		switch e["case"] {
		case "offer":
			w := obj(t, fact["intended_worker"], "intended_worker")
			offerWorker = [2]any{w["user_id"], w["agent_id"]}
		case "accepted_attempt":
			m := obj(t, fact["member"], "member")
			transferMember = [2]any{m["user_id"], m["agent_id"]}
		}
	}
	if offerWorker[0] == nil || offerWorker[1] == nil || offerWorker != transferMember {
		t.Errorf("the transfer's member %v is not the offer's intended worker %v", transferMember, offerWorker)
	}
	// Inactive replies carry only the nonce: no reason.
	for _, in := range arr(t, ex["inactive"], "inactive") {
		in := obj(t, in, "inactive")
		body := obj(t, obj(t, in["response"], "response")["body"], "inactive body")
		nonce := obj(t, in["request"], "request")["nonce"]
		if !reflect.DeepEqual(keysOf(body), []string{"active", "nonce"}) || body["active"] != false || body["nonce"] != nonce {
			t.Errorf("inactive %v discloses more than the nonce: %v", in["state"], body)
		}
	}
	// Peer-level failures are unavailable; fact-level ones are rejected.
	outcomes := map[string]string{}
	for _, a := range arr(t, ex["acceptance"], "acceptance") {
		a := obj(t, a, "acceptance")
		outcomes[str(t, a["case"], "case")] = str(t, a["outcome"], "outcome")
	}
	for c, want := range map[string]string{"inactive_reply": "coordination_rejected", "request_key_digest_differs": "coordination_rejected",
		"member_generation_differs": "coordination_rejected", "member_session_differs": "coordination_rejected",
		"other_task": "coordination_rejected", "operation_differs": "coordination_rejected", "fact_expired_by_hub_clock": "coordination_rejected",
		"nonce_mismatch": "context_unavailable", "tls_identity_mismatch": "context_unavailable", "unreachable_or_timeout": "context_unavailable",
		"reply_over_size_ceiling": "context_unavailable", "unsupported_version_refusal": "context_unavailable",
		"transfer_by_another_user_than_intended": "coordination_rejected", "transfer_by_another_agent_of_the_intended_user": "coordination_rejected"} {
		if outcomes[c] != want {
			t.Errorf("acceptance %s = %q, want %q", c, outcomes[c], want)
		}
	}
	for c, o := range outcomes {
		if o != "coordination_rejected" && o != "context_unavailable" {
			t.Errorf("acceptance %s has outcome %q", c, o)
		}
	}
	for _, a := range arr(t, ex["acceptance"], "acceptance") {
		a := obj(t, a, "acceptance")
		if a["case"] != "transfer_by_another_agent_of_the_intended_user" {
			continue
		}
		w, m := obj(t, a["offer_intended_worker"], "intended"), obj(t, a["transfer_member"], "member")
		if w["user_id"] != m["user_id"] || w["agent_id"] == m["agent_id"] {
			t.Error("the same-user, different-agent case must differ in the agent alone")
		}
	}
	// Version and size.
	cv := obj(t, ex["coordination_version"], "coordination_version")
	for _, c := range arr(t, cv["cases"], "version cases") {
		c := obj(t, c, "version case")
		accept := c["header"] == "1" && c["body"] == float64(1)
		if c["accepted"] != accept {
			t.Errorf("version case %v: accepted %v", c["case"], c["accepted"])
		}
	}
	if r := obj(t, cv["rejection"], "rejection"); r["aicrew_status"] != float64(400) || r["evaluated"] != false || r["aimem_outcome"] != "context_unavailable" || r["applied"] != false {
		t.Errorf("version rejection: %v", r)
	}
	for _, c := range arr(t, obj(t, ex["response_size"], "size")["cases"], "size cases") {
		c := obj(t, c, "size case")
		if c["accepted"] != (c["body_bytes"].(float64) <= 16384) {
			t.Errorf("size case %v", c)
		}
	}
	// Digests are the contract's encodings.
	dg := obj(t, ex["digests"], "digests")
	for _, c := range arr(t, obj(t, dg["request_key"], "k1")["cases"], "k1 cases") {
		c := obj(t, c, "k1 case")
		if c["digest"] != sha256Digest("k1_", str(t, c["raw"], "raw")) {
			t.Errorf("k1 digest of %v", c["raw"])
		}
	}
	for _, c := range arr(t, obj(t, dg["proof"], "p1")["cases"], "p1 cases") {
		c := obj(t, c, "p1 case")
		name := strings.Trim(str(t, c["proof"], "proof"), "{}")
		if d := str(t, c["digest"], "digest"); d != sha256Digest("p1_", str(t, secrets[name], name)) || !proofDigest.MatchString(d) {
			t.Errorf("p1 digest of %s", name)
		}
	}
	// A replay never asks aicrew again; a retry of an uncommitted request does.
	for _, r := range arr(t, ex["replay"], "replay") {
		r := obj(t, r, "replay")
		checks := arr(t, r["rechecks"], "rechecks")
		switch r["case"] {
		case "identical_replay_after_proof_settled":
			if r["coordination_calls"] != float64(0) || containsAny(checks, "coordination_fact") || r["replayed"] != true {
				t.Errorf("a replay must recheck only aimem-owned authority: %v", r)
			}
			for _, need := range []string{"token", "grant", "profile", "acting_member"} {
				if !containsAny(checks, need) {
					t.Errorf("a replay skips %s", need)
				}
			}
		case "coordinator_finalize_replay", "coordinator_finalize_replay_other_session":
			// The reviewing coordinator replays its own finalize, from the
			// session recorded on the receipt, and never asks aicrew again.
			acting := obj(t, r["acting_member"], "acting_member")
			same := acting["session_id"] == r["caller_session_id"]
			if r["coordination_calls"] != float64(0) || containsAny(checks, "coordination_fact") || !containsAny(checks, "acting_member") ||
				acting["role"] != "coordinator" || (r["result"] == "recorded_outcome") != same || r["replayed"] != same {
				t.Errorf("coordinator finalize replay: %v", r)
			}
		case "retry_after_not_committed":
			if r["coordination_calls"] != float64(1) || !containsAny(checks, "coordination_fact") || r["replayed"] != false {
				t.Errorf("a retry that did not commit verifies the fact again: %v", r)
			}
		default:
			t.Errorf("unknown replay case %v", r["case"])
		}
	}
}

func TestCoordinationV1ReadScopeAndCLI(t *testing.T) {
	ex := readCoordinationFixture(t, "examples.json")
	spec := readCoordinationFixture(t, "openapi-proposal.json")
	secrets := obj(t, ex["sample_secrets"], "sample_secrets")
	rs := obj(t, ex["read_scope"], "read_scope")
	cred := obj(t, rs["credential"], "credential")
	if cred["operation"] != "reservation.read" || cred["digest_only"] != true || cred["max_days"] != float64(366) || cred["active_per_peer"] != float64(2) {
		t.Errorf("read credential: %v", cred)
	}
	if !readCredShape.MatchString(str(t, secrets["read_credential"], "read credential")) {
		t.Error("read credential shape")
	}
	for _, g := range []string{"mutation", "task_content", "other_holders", "proof_redemption", "introspection"} {
		if !containsAny(arr(t, cred["grants_nothing"], "grants_nothing"), g) {
			t.Errorf("read credential must grant no %s", g)
		}
	}
	var rawKeys []string
	for _, c := range arr(t, obj(t, obj(t, ex["digests"], "digests")["request_key"], "k1")["cases"], "k1") {
		rawKeys = append(rawKeys, str(t, obj(t, c, "k1")["raw"], "raw"))
	}
	schemas := obj(t, obj(t, spec["components"], "components")["schemas"], "schemas")
	receiptFields := obj(t, obj(t, obj(t, schemas["ReceiptRead"], "ReceiptRead")["properties"], "p")["receipt"], "receipt")["required"]
	closedBy := map[string]bool{}
	for _, e := range arr(t, rs["exchanges"], "read exchanges") {
		e := obj(t, e, "read exchange")
		c := str(t, e["case"], "case")
		http := obj(t, e["http"], c)
		headers := obj(t, http["headers"], c)
		path := str(t, http["path"], c)
		if http["method"] != "GET" || headers["Authorization"] != "Bearer {read_credential}" || headers["X-Aimem-Reservation-Version"] != "1" ||
			!strings.HasPrefix(path, "/v1/identity/peers/aicrew-example/") {
			t.Errorf("%s: read request line or headers", c)
		}
		body := obj(t, obj(t, e["response"], c)["body"], c)
		raw, _ := json.Marshal(body)
		for _, k := range rawKeys {
			if strings.Contains(string(raw), k) {
				t.Errorf("%s: a read answer carries a raw request key", c)
			}
		}
		switch c {
		case "receipt_committed":
			if !proofDigest.MatchString(path[strings.LastIndex(path, "/")+1:]) {
				t.Errorf("%s: receipts are read by proof digest", c)
			}
			receipt := obj(t, body["receipt"], c)
			var want []string
			for _, f := range arr(t, receiptFields, "receipt fields") {
				want = append(want, str(t, f, "field"))
			}
			sort.Strings(want)
			if body["state"] != "committed" || !reflect.DeepEqual(keysOf(receipt), want) || receipt["verified_mode"] != "team" {
				t.Errorf("%s: receipt fields %v", c, keysOf(receipt))
			}
			// The committed receipt belongs to the proof in the path.
			if path[strings.LastIndex(path, "/")+1:] != sha256Digest("p1_", str(t, secrets["proof_accepted_attempt"], "proof")) ||
				receipt["request_key_digest"] != sha256Digest("k1_", "accept:attempt-example-7:transfer") {
				t.Errorf("%s: receipt does not bind its proof and key", c)
			}
		case "update_receipt_by_key":
			// A holder's update carries no proof: aicrew reads its receipt by
			// task, operation and key digest, on a hold its proof established.
			receipt := obj(t, body["receipt"], c)
			digest := path[strings.LastIndex(path, "/")+1:]
			if body["state"] != "committed" || receipt["operation"] != "update" || receipt["request_key_digest"] != digest ||
				!keyDigest.MatchString(digest) || !strings.Contains(path, "/reservations/"+str(t, receipt["task_id"], "task")+"/receipts/update/") {
				t.Errorf("%s: receipt by key does not bind its task, operation and key", c)
			}
		case "closed_holder_release", "closed_holder_finalize", "closed_recovery_release", "closed_recovery_cancel", "closed_while_another_holds_the_task":
			// Positive closure evidence for this service's own reservation: the
			// closing fence advanced, and nothing about the recovery admin, the
			// attestation, the reason or any current holder is disclosed.
			if !reflect.DeepEqual(keysOf(body), []string{"closed_at", "closed_by", "closing_fence", "reservation_id", "state", "task_revision"}) || body["state"] != "closed" {
				t.Errorf("%s: closed field set %v", c, keysOf(body))
			}
			by := str(t, body["closed_by"], "closed_by")
			if !contains([]string{"holder_release", "holder_finalize", "recovery_release", "recovery_cancel"}, by) {
				t.Errorf("%s: closed_by %q", c, by)
			}
			closedBy[by] = true
			ctx := obj(t, e["context"], c+".context")
			if atoi(t, str(t, body["closing_fence"], "closing_fence")) <= atoi(t, str(t, ctx["last_active_fence"], "last fence")) {
				t.Errorf("%s: the closing fence did not advance past the active fence", c)
			}
			if _, err := time.Parse(time.RFC3339, str(t, body["closed_at"], "closed_at")); err != nil {
				t.Errorf("%s: closed_at: %v", c, err)
			}
			for _, k := range []string{"recovery_admin", "attestation", "attestation_id", "reason", "current_holder", "current_holder_ref"} {
				if v, ok := ctx[k].(string); ok && strings.Contains(string(raw), v) {
					t.Errorf("%s: closed answer discloses %s", c, k)
				}
			}
		case "receipt_none", "hold_outside_scope":
			if !reflect.DeepEqual(keysOf(body), []string{"state"}) || body["state"] != "none" {
				t.Errorf("%s: none carries nothing else: %v", c, body)
			}
		case "hold_under_own_proof":
			if body["state"] != "held" || body["holder_mode"] != "external" || !reflect.DeepEqual(keysOf(body),
				[]string{"fence", "holder_mode", "own_work_ref", "reservation_id", "state", "task_revision"}) {
				t.Errorf("%s: hold fields %v", c, keysOf(body))
			}
		default:
			t.Errorf("unknown read case %s", c)
		}
	}
	for _, by := range []string{"holder_release", "holder_finalize", "recovery_release", "recovery_cancel"} {
		if !closedBy[by] {
			t.Errorf("no closed example for %s", by)
		}
	}
	// Aicrew closes an attempt only on its exact reservation ID with the fence
	// advanced; anything else keeps it open.
	cu := obj(t, rs["closure_use"], "closure_use")
	actions := map[string]bool{}
	for _, c := range arr(t, cu["cases"], "closure cases") {
		c := obj(t, c, "closure case")
		att, ans := obj(t, c["attempt"], "attempt"), obj(t, c["answer"], "answer")
		want := "keep_open"
		if ans["state"] == "closed" && ans["reservation_id"] == att["reservation_id"] &&
			atoi(t, str(t, ans["closing_fence"], "closing_fence")) > atoi(t, str(t, att["fence"], "fence")) {
			want = "close"
		}
		if c["action"] != want {
			t.Errorf("closure use %v: action %v, want %s", c["case"], c["action"], want)
		}
		actions[want] = true
	}
	if !actions["close"] || !actions["keep_open"] {
		t.Error("closure use needs both a close and a keep-open case")
	}
	// The proposal's HoldRead carries the three states and the closure fields.
	hold := obj(t, obj(t, schemas["HoldRead"], "HoldRead")["properties"], "HoldRead.properties")
	if !reflect.DeepEqual(obj(t, hold["state"], "state")["enum"], []any{"held", "closed", "none"}) ||
		!reflect.DeepEqual(obj(t, hold["closed_by"], "closed_by")["enum"], []any{"holder_release", "holder_finalize", "recovery_release", "recovery_cancel"}) {
		t.Errorf("HoldRead states or closed_by values: %v %v", hold["state"], hold["closed_by"])
	}
	for _, f := range []string{"closing_fence", "closed_at"} {
		if _, ok := hold[f]; !ok {
			t.Errorf("HoldRead lacks %s", f)
		}
	}
	nf := obj(t, rs["none_finality"], "none_finality")
	if pb := obj(t, nf["proof_backed"], "proof_backed"); pb["before_final"] != "wait" || pb["after_final"] != "not_committed" {
		t.Errorf("proof-backed none finality: %v", pb)
	}
	// A holder's update has no proof: a none stays unresolved until the hold
	// is seen past the request and the receipt is read after that, or the
	// same key replays.
	du := obj(t, obj(t, nf["proofless_update"], "proofless_update")["delayed_update"], "delayed_update")
	req := obj(t, du["request"], "request")
	if req["operation"] != "update" || !keyDigest.MatchString(str(t, req["request_key_digest"], "digest")) {
		t.Errorf("delayed update request: %v", req)
	}
	fence, rev := atoi(t, str(t, req["fence"], "fence")), int(req["expected_revision"].(float64))
	outcomesSeen := map[string]bool{}
	for _, c := range arr(t, du["cases"], "delayed cases") {
		c := obj(t, c, "delayed case")
		want := "unresolved"
		if ev, ok := c["evidence"].(map[string]any); ok && ev["source"] == "same_key_replay" {
			want = str(t, ev["result"], "replay result")
		} else if c["receipt"] == "committed" {
			want = "committed"
		} else if ok && c["receipt_read_after_evidence"] == true {
			past := false
			if f, ok := ev["fence"].(string); ok {
				past = atoi(t, f) > fence
			}
			if r, ok := ev["task_revision"].(float64); ok {
				past = past || int(r) > rev
			}
			if past {
				want = "not_committed"
			}
		}
		if c["outcome"] != want {
			t.Errorf("delayed update %v: outcome %v, want %s", c["case"], c["outcome"], want)
		}
		outcomesSeen[want] = true
	}
	for _, o := range []string{"unresolved", "not_committed", "committed"} {
		if !outcomesSeen[o] {
			t.Errorf("no delayed-update case ends %s", o)
		}
	}
	// The begin response is the one response that carries a proof: aicrew
	// delivering a new proof to the member that asked for the step.
	begin := obj(t, ex["begin_exchange"], "begin_exchange")
	bb := obj(t, obj(t, begin["response"], "begin response")["body"], "begin body")
	if begin["owner"] != "aicrew" || begin["caller"] != "member_client" ||
		obj(t, obj(t, begin["http"], "begin http")["headers"], "begin headers")["Authorization"] != "Bearer {aicrew_session_token}" ||
		!strings.HasPrefix(str(t, bb["coordination_proof"], "begin proof"), "{proof_") {
		t.Errorf("begin exchange: %v", begin)
	}
	status := map[string]float64{"unsupported_version": 400, "peer_unauthenticated": 401, "peer_forbidden": 403, "tls_required": 403,
		"rate_limited": 429, "request_in_progress": 503}
	for _, r := range arr(t, rs["refusals"], "read refusals") {
		r := obj(t, r, "refusal")
		code := str(t, r["code"], "code")
		if status[code] == 0 || r["http_status"] != status[code] || r["retryable"] != (code == "rate_limited" || code == "request_in_progress") {
			t.Errorf("read refusal %v", r)
		}
	}
	// The proposal's paths and owners.
	paths := obj(t, spec["paths"], "paths")
	for p, want := range map[string][2]string{
		"/v1/crew/coordination": {"aicrew", "crew.coordination"},
		"/v1/identity/peers/{service_id}/reservation-receipts/{proof_digest}":                              {"aimem", "reservation.read"},
		"/v1/identity/peers/{service_id}/reservations/{task_id}":                                           {"aimem", "reservation.read"},
		"/v1/identity/peers/{service_id}/reservations/{task_id}/receipts/{operation}/{request_key_digest}": {"aimem", "reservation.read"},
	} {
		entry := obj(t, paths[p], p)
		var op map[string]any
		for _, m := range []string{"get", "post"} {
			if v, ok := entry[m]; ok {
				op = obj(t, v, p+"."+m)
			}
		}
		if op == nil || op["x-owner"] != want[0] || op["x-operation"] != want[1] {
			t.Errorf("%s: owner or operation", p)
		}
	}
	if len(paths) != 4 {
		t.Errorf("proposal paths: %v", keysOf(paths))
	}
	// The reservation CLI (C6): every mutation and the receipt, secrets on stdin.
	cli := obj(t, ex["cli"], "cli")
	for _, c := range []string{"claim", "transfer", "update", "release", "finalize", "receipt", "status"} {
		if !containsAny(arr(t, cli["commands"], "commands"), "aimem reservation "+c) {
			t.Errorf("CLI lacks %s", c)
		}
	}
	if !containsAny(arr(t, cli["stdin_only"], "stdin_only"), "coordination_proof") {
		t.Error("the proof must travel on standard input")
	}
	for _, a := range arr(t, cli["arguments"], "arguments") {
		if strings.Contains(str(t, a, "argument"), "proof") {
			t.Errorf("CLI argument %v can carry the proof", a)
		}
	}
	if !reflect.DeepEqual(keysOf(obj(t, cli["exit_codes"], "exit codes")), []string{"0", "2", "3", "4", "5"}) {
		t.Errorf("CLI exit codes: %v", cli["exit_codes"])
	}
}

// secretViolation reports whether value, at the normalized path, carries a
// sample secret the contract does not permit there.
func secretViolation(secrets map[string]any, locations map[string]any, path, value string) string {
	norm := indexedSegment.ReplaceAllString(path, "[]")
	for name, raw := range secrets {
		secret, _ := raw.(string)
		if !strings.Contains(value, "{"+name+"}") && (secret == "" || !strings.Contains(value, secret)) {
			continue
		}
		category := name
		if strings.HasPrefix(name, "proof_") {
			category = "proof"
		}
		permitted := false
		if list, ok := locations[category].([]any); ok {
			for _, p := range list {
				permitted = permitted || p == norm
			}
		}
		if !permitted {
			return name + " at " + norm
		}
	}
	return ""
}

func walkStrings(v any, path string, fn func(path, value string)) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			p := k
			if path != "" {
				p = path + "." + k
			}
			walkStrings(child, p, fn)
		}
	case []any:
		for _, child := range x {
			walkStrings(child, path+"[]", fn)
		}
	case string:
		fn(path, x)
	}
}

func TestCoordinationV1SecretsStayWherePermitted(t *testing.T) {
	ex := readCoordinationFixture(t, "examples.json")
	secrets := obj(t, ex["sample_secrets"], "sample_secrets")
	locations := obj(t, ex["secret_locations"], "secret_locations")
	scanned := 0
	for top, v := range ex {
		if top == "sample_secrets" || top == "secret_locations" || top == "secret_location_rejections" {
			continue
		}
		walkStrings(v, top, func(path, value string) {
			scanned++
			if bad := secretViolation(secrets, locations, path, value); bad != "" {
				t.Errorf("secret outside its permitted locations: %s", bad)
			}
		})
	}
	if scanned < 100 {
		t.Fatalf("the secret walk saw only %d strings", scanned)
	}
	// Every listed misplacement is caught by the same check.
	for _, r := range arr(t, ex["secret_location_rejections"], "rejections") {
		r := obj(t, r, "rejection")
		if secretViolation(secrets, locations, str(t, r["path"], "path"), str(t, r["value"], "value")) == "" {
			t.Errorf("misplacement %v was not caught", r["case"])
		}
	}
	// No proof, credential or handle ever appears in an answer aimem or aicrew gives.
	for _, e := range arr(t, obj(t, ex["read_scope"], "read_scope")["exchanges"], "read") {
		b, _ := json.Marshal(obj(t, e, "read")["response"])
		for name, s := range secrets {
			if strings.Contains(string(b), s.(string)) || strings.Contains(string(b), "{"+name+"}") {
				t.Errorf("read answer carries %s", name)
			}
		}
	}
	for _, e := range arr(t, ex["exchanges"], "exchanges") {
		b, _ := json.Marshal(obj(t, e, "exchange")["response"])
		if strings.Contains(string(b), "acp1_") || strings.Contains(string(b), "{proof_") {
			t.Error("a coordination reply echoes the proof")
		}
	}
}

// The hub serves none of this yet, and the reservation contracts carry the
// operator's D4 correction.
func TestCoordinationV1IsNotServedAndContractsAgree(t *testing.T) {
	s, _ := testServer(t)
	for _, route := range s.Routes() {
		if strings.HasPrefix(route.Pattern, recoveryNamespace) {
			continue // C5c's admin-only recovery routes, not the read scope
		}
		for _, frag := range []string{"/v1/crew/", "reservation-receipts", "/reservations/"} {
			if strings.Contains(route.Pattern, frag) {
				t.Errorf("C5w registered route %s", route.Pattern)
			}
		}
	}
	var live struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPISpec, &live); err != nil {
		t.Fatal(err)
	}
	for path := range live.Paths {
		if strings.HasPrefix(path, recoveryNamespace) {
			continue
		}
		if strings.Contains(path, "/v1/crew/") || strings.Contains(path, "reservation") {
			t.Errorf("C5w changed live OpenAPI path %s", path)
		}
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "docs", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	coordination := read("DESIGN-AIFORGE-COORDINATION-WIRE.md")
	for kind := range factKinds {
		if !strings.Contains(coordination, "`"+kind+"`") {
			t.Errorf("the coordination contract does not define %s", kind)
		}
	}
	for _, must := range []string{"coordination_rejected", "reservation.read", "`acp1_`", "`p1_`", "X-Aimem-Coordination-Version", "standard input",
		"All three are HTTP-only", "own_work_ref", "acting member", "begin response", "stays **unresolved**", "**Only after that**",
		"### Closure evidence", "`recovery_cancel`", "`closing_fence`", "never describes that other holder"} {
		if !strings.Contains(coordination, must) {
			t.Errorf("the coordination contract lacks %s", must)
		}
	}
	wire := read("DESIGN-AIFORGE-RESERVATION-WIRE.md")
	for _, gone := range []string{"bounded aicrew service", "bounded service transition", "Bounded aicrew transition"} {
		if strings.Contains(wire, gone) {
			t.Errorf("the reservation wire still says %q", gone)
		}
	}
	if strings.Contains(coordination, "Both are HTTP-only") || strings.Contains(wire, "holder binding") {
		t.Error("the contracts keep wording the operator corrected")
	}
	for _, must := range []string{"acting member's own verified connection", "coordination_rejected", "Replay rule", "aimem reservation", "acting member"} {
		if !strings.Contains(wire, must) {
			t.Errorf("the reservation wire lacks %q", must)
		}
	}
	for _, doc := range []string{"DESIGN-AIFORGE-RESERVATIONS.md", "DESIGN-AIFORGE-CONTEXT.md"} {
		text := read(doc)
		for _, gone := range []string{"bounded aicrew service call", "submit a bounded reservation request", "reservation integration operations"} {
			if strings.Contains(text, gone) {
				t.Errorf("%s still says %q", doc, gone)
			}
		}
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("not a decimal: %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func containsAny(list []any, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
