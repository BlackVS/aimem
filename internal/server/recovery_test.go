package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"aimem/internal/introspect/introspecttest"
	"aimem/internal/store"
)

// introspecttestProof is a fresh, well-formed coordination proof.
func introspecttestProof() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "acp1_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

const recoveryStatement = "worker machine decommissioned; the attempt is abandoned"

type recoveryRig struct {
	*reservationRig
	task store.Task
	hold store.TaskReservationOutcome
}

// newRecoveryRig holds one alpha task for Alice's verified team context
// (service aicrew-example, team-1, worker), as a coordination-backed claim
// will (C5b).
func newRecoveryRig(t *testing.T) *recoveryRig {
	t.Helper()
	g := &recoveryRig{reservationRig: newReservationRig(t)}
	ctx := g.team(t, g.aliceIdentity, nil)
	g.task = g.readyTask(t, g.alpha)
	g.hold = g.seedTeamHold(t, ctx, g.task)
	return g
}

func (g *recoveryRig) body(state string, evidence map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"reservation_id": g.hold.Reservation.ID, "fence": strconv.FormatInt(g.hold.Reservation.Fence, 10),
		"expected_revision": g.hold.Task.Revision, "reason": "the holding worker is gone",
		"content":  map[string]any{"title": g.task.Title, "state": state},
		"evidence": evidence,
	})
	return string(b)
}

func attestationEvidence() map[string]any {
	return map[string]any{"kind": "attestation", "attestation_id": "ticket-4711", "statement": recoveryStatement}
}

func (g *recoveryRig) recover(t *testing.T, op, bearer, key, body string) identityResp {
	t.Helper()
	return g.call(t, g.tls, "POST", "/v1/admin/reservations/"+g.task.ID+"/recovery/"+op, bearer, map[string]string{"Idempotency-Key": key}, body, true)
}

func recoveryCode(t *testing.T, r identityResp) string {
	t.Helper()
	var env struct {
		Code string `json:"code"`
	}
	json.Unmarshal(r.body, &env)
	return env.Code
}

func (g *recoveryRig) auditText(t *testing.T) string {
	t.Helper()
	b, _ := json.Marshal(g.audit(t))
	return string(b)
}

// auditCount counts the access audit records with this exact action.
func (g *recoveryRig) auditCount(t *testing.T, action string) int {
	t.Helper()
	n := 0
	for _, e := range g.audit(t) {
		if e.Action == action {
			n++
		}
	}
	return n
}

// recoveryAudits counts every reservation.recovery.* record.
func (g *recoveryRig) recoveryAudits(t *testing.T) int {
	t.Helper()
	n := 0
	for _, e := range g.audit(t) {
		if strings.HasPrefix(e.Action, "reservation.recovery.") {
			n++
		}
	}
	return n
}

// Every recovery request past the admin gate leaves exactly one
// reservation.recovery.* record: a mutation, its replay, each read, and each
// refusal, the reader's included. None carries the attestation's text.
func TestRecoveryAuditsEveryRequestOnce(t *testing.T) {
	g := newRecoveryRig(t)
	body := g.body("READY", attestationEvidence())
	missing := "01a0ffff-ffff-7000-8000-000000000000"
	read := func(task, suffix string) func() identityResp {
		return func() identityResp {
			return g.call(t, g.tls, "GET", "/v1/admin/reservations/"+task+"/recovery"+suffix, g.env, nil, "", true)
		}
	}
	seed := store.RequestKeyDigest("seed-" + g.task.ID)
	for _, c := range []struct {
		name, action string
		status       int
		do           func() identityResp
	}{
		{"release", "reservation.recovery.release", 200, func() identityResp { return g.recover(t, "release", g.env, "audit-1", body) }},
		{"its replay", "reservation.recovery.replay.release", 200, func() identityResp { return g.recover(t, "release", g.env, "audit-1", body) }},
		{"a refused mutation", "reservation.recovery.refused.invalid_request", 400, func() identityResp { return g.recover(t, "release", g.env, "audit-2", "{") }},
		{"status", "reservation.recovery.read", 200, read(g.task.ID, "")},
		{"status of a missing task", "reservation.recovery.refused.task_unavailable", 404, read(missing, "")},
		{"receipts", "reservation.recovery.read", 200, read(g.task.ID, "/receipts/claim/"+seed)},
		{"receipts of an unknown operation", "reservation.recovery.refused.invalid_request", 400, read(g.task.ID, "/receipts/steal/"+seed)},
		{"receipts under a malformed digest", "reservation.recovery.refused.invalid_request", 400, read(g.task.ID, "/receipts/claim/k1_short")},
		{"receipts of a missing task", "reservation.recovery.refused.task_unavailable", 404, read(missing, "/receipts/claim/"+seed)},
	} {
		total, same := g.recoveryAudits(t), g.auditCount(t, c.action)
		if r := c.do(); r.status != c.status {
			t.Fatalf("%s: %d %s", c.name, r.status, r.body)
		}
		if got := g.recoveryAudits(t) - total; got != 1 {
			t.Fatalf("%s left %d recovery audit records, want 1", c.name, got)
		}
		if got := g.auditCount(t, c.action) - same; got != 1 {
			t.Fatalf("%s: no %s record", c.name, c.action)
		}
	}
	if audit := g.auditText(t); strings.Contains(audit, recoveryStatement) || strings.Contains(audit, "k1_short") {
		t.Fatalf("the audit quotes the statement or an unvalidated digest: %s", audit)
	}
}

func TestRecoveryIsAdminOnlyOverHubTLS(t *testing.T) {
	g := newRecoveryRig(t)
	body := g.body("READY", attestationEvidence())
	if r := g.recover(t, "release", g.alice, "k1", body); r.status != http.StatusForbidden {
		t.Fatalf("a user token recovered: %d %s", r.status, r.body)
	}
	r := g.call(t, g.plain, "POST", "/v1/admin/reservations/"+g.task.ID+"/recovery/release", g.env, map[string]string{"Idempotency-Key": "k2"}, body, true)
	if r.status != http.StatusForbidden || !strings.Contains(string(r.body), "tls_required") {
		t.Fatalf("plain HTTP: %d %s", r.status, r.body)
	}
	for _, path := range []string{"/recovery", "/recovery/receipts/claim/k1_" + strings.Repeat("A", 43)} {
		if r := g.call(t, g.tls, "GET", "/v1/admin/reservations/"+g.task.ID+path, g.alice, nil, "", true); r.status != http.StatusForbidden {
			t.Fatalf("a user token read %s: %d", path, r.status)
		}
	}
	g.held(t, g.task.ID, g.hold.Reservation)
}

func TestRecoveryReleaseOnAttestation(t *testing.T) {
	g := newRecoveryRig(t)
	body := g.body("READY", attestationEvidence())
	r := g.recover(t, "release", g.env, "rec-1", body)
	if r.status != http.StatusOK {
		t.Fatalf("recovery release: %d %s", r.status, r.body)
	}
	var out recoveryResponse
	json.Unmarshal(r.body, &out)
	if out.Operation != store.RecoveryRelease || out.TaskState != "READY" || out.Principal != "recovery/env" ||
		out.Affected != g.hold.Reservation.Binding || out.EvidenceKind != "attestation" || out.EvidenceRef != "ticket-4711" ||
		out.ClosingFence != strconv.FormatInt(g.hold.Reservation.Fence+1, 10) {
		t.Fatalf("recovery response: %+v", out)
	}
	if strings.Contains(string(r.body), recoveryStatement) {
		t.Fatal("the response quotes the attestation")
	}
	audit := g.auditText(t)
	if !strings.Contains(audit, "reservation.recovery.release") || !strings.Contains(audit, "affected_user="+g.aliceID) ||
		strings.Contains(audit, recoveryStatement) {
		t.Fatalf("audit: %s", audit)
	}
	// aicrew's read scope now reads its reservation as closed by recovery.
	if got, err := g.alpha.ServiceHoldStatus(g.task.ID, "aicrew-example"); err != nil || got.State != "closed" || got.ClosedBy != "recovery_release" ||
		got.ReservationID != g.hold.Reservation.ID {
		t.Fatalf("closure evidence: %+v %v", got, err)
	}
	// A replay returns the same; a changed body under the key conflicts.
	if again := g.recover(t, "release", g.env, "rec-1", body); again.status != http.StatusOK {
		t.Fatalf("replay: %d %s", again.status, again.body)
	}
	changed := g.body("BLOCKED", attestationEvidence())
	if r := g.recover(t, "release", g.env, "rec-1", changed); r.status != http.StatusConflict || recoveryCode(t, r) != "idempotency_conflict" {
		t.Fatalf("changed replay: %d %s", r.status, r.body)
	}
}

func TestRecoveryRefusesDoneAndBadRequests(t *testing.T) {
	g := newRecoveryRig(t)
	for name, c := range map[string]struct {
		op, key, body string
		status        int
		code          string
	}{
		"cancel to DONE":  {"cancel", "k-done", g.body("DONE", attestationEvidence()), 400, "invalid_request"},
		"release to DONE": {"release", "k-done2", g.body("DONE", attestationEvidence()), 400, "invalid_request"},
		"no key":          {"release", "", g.body("READY", attestationEvidence()), 400, "invalid_request"},
		"no evidence":     {"release", "k-ne", g.body("READY", map[string]any{"kind": "vibes"}), 400, "invalid_request"},
		"short statement": {"release", "k-ss", g.body("READY", map[string]any{"kind": "attestation", "attestation_id": "t", "statement": "short"}), 400, "invalid_request"},
		"mixed evidence":  {"release", "k-mx", g.body("READY", map[string]any{"kind": "attestation", "attestation_id": "t", "statement": recoveryStatement, "proof": "acp1_x"}), 400, "invalid_request"},
		"unknown field":   {"release", "k-uf", strings.Replace(g.body("READY", attestationEvidence()), `"reason"`, `"actor_id":"someone","reason"`, 1), 400, "invalid_request"},
	} {
		r := g.recover(t, c.op, g.env, c.key, c.body)
		if r.status != c.status || recoveryCode(t, r) != c.code {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
	stale := strings.Replace(g.body("READY", attestationEvidence()), `"fence":"`+strconv.FormatInt(g.hold.Reservation.Fence, 10)+`"`,
		`"fence":"`+strconv.FormatInt(g.hold.Reservation.Fence+5, 10)+`"`, 1)
	if r := g.recover(t, "release", g.env, "k-stale", stale); r.status != http.StatusConflict || recoveryCode(t, r) != "stale_fence" {
		t.Fatalf("stale fence: %d %s", r.status, r.body)
	}
	g.held(t, g.task.ID, g.hold.Reservation)
	// Cancel finalizes CANCELLED and aicrew reads recovery_cancel.
	if r := g.recover(t, "cancel", g.env, "k-cancel", g.body("CANCELLED", attestationEvidence())); r.status != http.StatusOK {
		t.Fatalf("cancel: %d %s", r.status, r.body)
	}
	if got, _ := g.alpha.ServiceHoldStatus(g.task.ID, "aicrew-example"); got.ClosedBy != "recovery_cancel" {
		t.Fatalf("cancel closure: %+v", got)
	}
}

// Stop evidence is a coordination.v1 stopped fact for this hold, whose
// member is the holder recorded on the hold.
func TestRecoveryOnStopEvidence(t *testing.T) {
	g := newRecoveryRig(t)
	proof, err := introspecttestProof()
	if err != nil {
		t.Fatal(err)
	}
	key := "stop-rec-1"
	answer := func(edit func(fact map[string]any)) {
		g.fake.SetCoordination(func(w http.ResponseWriter, got introspecttest.Request) {
			fact := map[string]any{
				"kind": "stopped", "operation": "release", "task_id": g.task.ID,
				"request_key_digest": store.RequestKeyDigest(key),
				"member": map[string]any{"user_id": g.aliceID, "agent_id": "agent-1", "team_id": "team-1", "role": "worker",
					"session_id": "sess-9", "generation": "7"},
				"attempt_ref": g.hold.Reservation.Holder.Ref,
				"expires_at":  "2999-01-01T00:00:00Z",
			}
			if edit != nil {
				edit(fact)
			}
			introspecttest.WriteJSON(w, introspecttest.FactReply(got.Nonce, "aicrew-example", g.hub, fact))
		})
	}
	stopBody := g.body("READY", map[string]any{"kind": "stop_evidence", "proof": proof})
	for name, edit := range map[string]func(map[string]any){
		"another attempt": func(f map[string]any) { f["attempt_ref"] = "aicrew-attempt-other" },
		"another member":  func(f map[string]any) { f["member"].(map[string]any)["user_id"] = g.bobID },
		"another key":     func(f map[string]any) { f["request_key_digest"] = store.RequestKeyDigest("other-key") },
		"another task":    func(f map[string]any) { f["task_id"] = g.betaTk },
		"not a stop": func(f map[string]any) {
			f["kind"], f["operation"] = "never_accepted", "release"
			delete(f, "attempt_ref")
			f["offer_ref"] = "o"
		},
	} {
		answer(edit)
		if r := g.recover(t, "release", g.env, key, stopBody); r.status != http.StatusForbidden || recoveryCode(t, r) != "coordination_rejected" {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
	}
	g.fake.SetCoordination(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, introspecttest.InactiveReply(got.Nonce))
	})
	if r := g.recover(t, "release", g.env, key, stopBody); r.status != http.StatusForbidden || recoveryCode(t, r) != "coordination_rejected" {
		t.Fatalf("inactive: %d %s", r.status, r.body)
	}
	g.held(t, g.task.ID, g.hold.Reservation)
	// The matching stop: the session and generation may have moved on.
	answer(nil)
	r := g.recover(t, "release", g.env, key, stopBody)
	if r.status != http.StatusOK {
		t.Fatalf("stop evidence: %d %s", r.status, r.body)
	}
	var out recoveryResponse
	json.Unmarshal(r.body, &out)
	if out.EvidenceKind != "stop_evidence" || out.EvidenceRef != proofP1Digest(proof) {
		t.Fatalf("stop response: %+v", out)
	}
	if strings.Contains(string(r.body), proof) || strings.Contains(g.auditText(t), proof) {
		t.Fatal("the proof reached a response or the audit")
	}
	if last := g.fake.Last(); last.Path != introspecttest.CoordinationPath || last.Body.Proof != proof {
		t.Fatalf("aicrew was not asked about the proof: %+v", last)
	}
	// aicrew settles the proof; the same request replays from its receipt
	// without asking aicrew again (the replay rule).
	g.fake.SetCoordination(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, introspecttest.InactiveReply(got.Nonce))
	})
	calls := g.fake.Calls()
	total, replays := g.recoveryAudits(t), g.auditCount(t, "reservation.recovery.replay.release")
	again := g.recover(t, "release", g.env, key, stopBody)
	var replay recoveryResponse
	json.Unmarshal(again.body, &replay)
	if again.status != http.StatusOK || replay != out || g.fake.Calls() != calls {
		t.Fatalf("stop-evidence replay: %d %s (aicrew calls %d -> %d)", again.status, again.body, calls, g.fake.Calls())
	}
	if g.recoveryAudits(t)-total != 1 || g.auditCount(t, "reservation.recovery.replay.release")-replays != 1 {
		t.Fatal("the stop-evidence replay is not audited exactly once")
	}
	if strings.Contains(g.auditText(t), proof) {
		t.Fatal("the replay audit quotes the proof")
	}
}

// A registry admin whose credential is gone by commit cannot commit: not
// when it is removed, not when it is named "env" like the host's env admin,
// and not when another credential takes its name.
func TestRecoveryRechecksTheAdminBeforeCommit(t *testing.T) {
	g := newRecoveryRig(t)
	root := g.s.reg.Root()
	t.Cleanup(func() { beforeReservationRecheck = nil })
	for _, c := range []struct {
		name     string
		replaced func(name string) []TokenEntry
	}{
		{"ops-admin", func(string) []TokenEntry { return nil }},
		{"env", func(string) []TokenEntry { return nil }},
		{"ops-admin", func(name string) []TokenEntry {
			_, other, _ := NewTokenSecret()
			return []TokenEntry{{Name: name, Role: "admin", SHA256: other}}
		}},
	} {
		secret, digest, err := NewTokenSecret()
		if err != nil {
			t.Fatal(err)
		}
		if err := SaveTokens(root, []TokenEntry{{Name: c.name, Role: "admin", SHA256: digest}}); err != nil {
			t.Fatal(err)
		}
		beforeReservationRecheck = func() { SaveTokens(root, c.replaced(c.name)) }
		r := g.recover(t, "release", secret, "admin-gone-"+c.name+strconv.Itoa(len(c.replaced(c.name))), g.body("READY", attestationEvidence()))
		beforeReservationRecheck = nil
		if r.status != http.StatusUnauthorized || recoveryCode(t, r) != "invalid_credential" {
			t.Fatalf("admin %q whose credential is gone committed: %d %s", c.name, r.status, r.body)
		}
		g.held(t, g.task.ID, g.hold.Reservation)
	}
	// The registry admin whose credential stays registered does commit.
	secret, digest, _ := NewTokenSecret()
	if err := SaveTokens(root, []TokenEntry{{Name: "ops-admin", Role: "admin", SHA256: digest}}); err != nil {
		t.Fatal(err)
	}
	if r := g.recover(t, "release", secret, "admin-kept", g.body("READY", attestationEvidence())); r.status != http.StatusOK {
		t.Fatalf("a registered admin was refused: %d %s", r.status, r.body)
	}
}

func TestRecoveryReader(t *testing.T) {
	g := newRecoveryRig(t)
	r := g.call(t, g.tls, "GET", "/v1/admin/reservations/"+g.task.ID+"/recovery", g.env, nil, "", true)
	var hold recoveryHold
	json.Unmarshal(r.body, &hold)
	if r.status != http.StatusOK || hold.State != "held" || hold.ReservationID != g.hold.Reservation.ID || hold.Binding != g.hold.Reservation.Binding {
		t.Fatalf("recovery status: %d %s", r.status, r.body)
	}
	path := "/v1/admin/reservations/" + g.task.ID + "/recovery/receipts/claim/" + store.RequestKeyDigest("seed-"+g.task.ID)
	r = g.call(t, g.tls, "GET", path, g.env, nil, "", true)
	var receipts struct {
		Receipts []store.RecoveryReceipt `json:"receipts"`
	}
	json.Unmarshal(r.body, &receipts)
	if r.status != http.StatusOK || len(receipts.Receipts) != 1 || receipts.Receipts[0].Principal != "user/"+g.aliceID {
		t.Fatalf("recovery receipts: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "GET", "/v1/admin/reservations/"+g.task.ID+"/recovery/receipts/steal/"+store.RequestKeyDigest("x"), g.env, nil, "", true); r.status != 400 {
		t.Fatalf("unknown operation: %d", r.status)
	}
	audit := g.auditText(t)
	if !strings.Contains(audit, "task="+g.task.ID+" status") || !strings.Contains(audit, "receipts operation=claim") ||
		!strings.Contains(audit, "reservation.recovery.read") {
		t.Fatalf("reads are not audited: %s", audit)
	}
	g.held(t, g.task.ID, g.hold.Reservation)
}
