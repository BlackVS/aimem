package server

// Aicrew's read scope (task C6b): a fake aicrew consumer reads the three
// routes over the hub's real TLS gate with a reservation.read credential,
// after real coordination-backed transitions, and checks every answer
// against the coordination-v1 read-scope fixtures.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/store"
	"aimem/internal/uuidv7"
)

type readRig struct {
	*reservationRig
	read    string   // the reservation.read bearer
	proofs  []string // every proof sent, for the leak scan
	keys    []string // every raw request key sent, for the leak scan
	bodies  []string // every read-scope answer
	lastKey string   // the request key of the latest step
}

func newReadRig(t *testing.T) *readRig {
	t.Helper()
	g := &readRig{reservationRig: newReservationRig(t)}
	g.selectProcess(t, testPin)
	_, g.read = g.issueCredentialFor(t, "aicrew-example", access.PeerOperationReservationRead, time.Now().Add(time.Hour))
	return g
}

// get reads one read-scope route with the rig's read credential.
func (g *readRig) get(t *testing.T, path string) identityResp {
	t.Helper()
	r := g.call(t, g.tls, "GET", path, g.read, readV1, "", true)
	g.bodies = append(g.bodies, string(r.body))
	return r
}

// answer reads a route that must answer 200 and decodes its body.
func (g *readRig) answer(t *testing.T, path string) map[string]any {
	t.Helper()
	r := g.get(t, path)
	if r.status != 200 || r.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("GET %s: %d %s", path, r.status, r.body)
	}
	var out map[string]any
	if err := json.Unmarshal(r.body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func byProof(proof string) string {
	return "/v1/identity/peers/aicrew-example/reservation-receipts/" + proofP1Digest(proof)
}

func byKey(taskID, op, key string) string {
	return "/v1/identity/peers/aicrew-example/reservations/" + taskID + "/receipts/" + op + "/" + store.RequestKeyDigest(key)
}

func holdOf(taskID string) string { return "/v1/identity/peers/aicrew-example/reservations/" + taskID }

// step runs one coordinated transition of the independent member on a fact
// of kind for its attempt, and returns the outcome and the proof it sent.
func (g *readRig) step(t *testing.T, ctx context.Context, op store.ReservationOperation, in store.TaskReservationInput, kind, attempt string) (store.TaskReservationOutcome, string) {
	t.Helper()
	key := "rk-" + string(op) + "-" + uuidv7.New()
	f := fact(ctx, kind, string(op), in.TaskID, key)
	f["attempt_ref"] = attempt
	if kind == "independent_claim" {
		f["process"] = pinOf(testPin)
	}
	g.answerFact(f)
	proof := testProof(t)
	g.proofs, g.keys = append(g.proofs, proof), append(g.keys, key)
	out, err := g.s.reserve(ctx, op, in, key, proof)
	if err != nil {
		t.Fatalf("%s on %s: %v", op, kind, err)
	}
	g.lastKey = key
	return out, proof
}

// claim is an independent claim of task under this service's proof.
func (g *readRig) claim(t *testing.T, ctx context.Context, taskID, attempt string) (store.TaskReservationOutcome, string) {
	t.Helper()
	task, err := g.alpha.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	return g.step(t, ctx, store.ReservationClaim, externalClaim(task, attempt), "independent_claim", attempt)
}

// fixtureBodies are the read-scope exchanges' response bodies by case.
func fixtureBodies(t *testing.T) map[string]map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "coordination-v1", "examples.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		ReadScope struct {
			Exchanges []struct {
				Case     string `json:"case"`
				Response struct {
					Body map[string]any `json:"body"`
				} `json:"response"`
			} `json:"exchanges"`
		} `json:"read_scope"`
	}
	if err := json.Unmarshal(b, &ex); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, e := range ex.ReadScope.Exchanges {
		out[e.Case] = e.Response.Body
	}
	return out
}

// sameShape requires got to carry exactly the fixture's fields, and the
// receipt, when there is one, exactly the fixture receipt's.
func sameShape(t *testing.T, what string, got, want map[string]any) {
	t.Helper()
	if !reflect.DeepEqual(keysOf(got), keysOf(want)) {
		t.Fatalf("%s fields %v, fixture %v", what, keysOf(got), keysOf(want))
	}
	if w, ok := want["receipt"].(map[string]any); ok {
		g, _ := got["receipt"].(map[string]any)
		if !reflect.DeepEqual(keysOf(g), keysOf(w)) {
			t.Fatalf("%s receipt fields %v, fixture %v", what, keysOf(g), keysOf(w))
		}
	}
}

// receiptOf checks a committed read-scope receipt against the outcome it
// reports, and returns it.
func (g *readRig) receiptOf(t *testing.T, what string, got map[string]any, op store.ReservationOperation, key, reservationID string, out store.TaskReservationOutcome) map[string]any {
	t.Helper()
	rc, _ := got["receipt"].(map[string]any)
	if got["state"] != "committed" || rc == nil {
		t.Fatalf("%s: %v", what, got)
	}
	want := map[string]any{
		"id": store.ReservationReceiptID(g.aliceID, op, out.Task.ID, key), "operation": string(op), "task_id": out.Task.ID,
		"request_key_digest": store.RequestKeyDigest(key), "reservation_id": reservationID,
		"fence": strconv.FormatInt(out.Reservation.Fence, 10), "task_revision": float64(out.Task.Revision),
		"member_user_id": g.aliceID, "verified_mode": "team",
	}
	for k, v := range want {
		if rc[k] != v {
			t.Fatalf("%s: %s is %v, want %v (%v)", what, k, rc[k], v, rc)
		}
	}
	if at, err := time.Parse(time.RFC3339, rc["committed_at"].(string)); err != nil || time.Since(at) > time.Hour {
		t.Fatalf("%s: committed_at %v", what, rc["committed_at"])
	}
	return rc
}

func (g *readRig) wantNone(t *testing.T, what, path string) {
	t.Helper()
	if got := g.answer(t, path); !reflect.DeepEqual(got, map[string]any{"state": "none"}) {
		t.Fatalf("%s: %v, want exactly {state: none}", what, got)
	}
}

// The consumer's whole view of one reservation's life under its proofs: the
// claim, the holder's proofless update, the stop release, closure evidence,
// durability, and a newer reservation replacing it.
func TestReadScopeFollowsItsReservation(t *testing.T) {
	g := newReadRig(t)
	fx := fixtureBodies(t)
	indep := g.member(t, "independent", "agent-indep", "sess-i")
	task := g.readyTask(t, g.alpha)

	claimed, claimProof := g.claim(t, indep, task.ID, "attempt-i")
	claimKey := g.lastKey
	r := claimed.Reservation.ID
	got := g.answer(t, byProof(claimProof))
	sameShape(t, "receipt by proof", got, fx["receipt_committed"])
	first := g.receiptOf(t, "claim by proof", got, store.ReservationClaim, claimKey, r, claimed)
	sameShape(t, "claim by key", g.answer(t, byKey(task.ID, "claim", claimKey)), fx["receipt_committed"])
	hold := g.answer(t, holdOf(task.ID))
	sameShape(t, "held", hold, fx["hold_under_own_proof"])
	if hold["state"] != "held" || hold["reservation_id"] != r || hold["fence"] != "1" || hold["holder_mode"] != "external" ||
		hold["own_work_ref"] != "attempt-i" || hold["task_revision"] != float64(claimed.Task.Revision) {
		t.Fatalf("held: %v", hold)
	}
	// A replay of the claim adds no receipt: the same one answers.
	if _, err := g.s.reserve(indep, store.ReservationClaim, externalClaim(task, "attempt-i"), claimKey, claimProof); err != nil {
		t.Fatal(err)
	}
	if again := g.answer(t, byProof(claimProof)); !reflect.DeepEqual(again["receipt"], first) {
		t.Fatalf("the replay changed the receipt: %v", again)
	}

	// The holder's update carries no proof; aicrew confirms it by key.
	updKey := "rk-update-" + uuidv7.New()
	g.keys = append(g.keys, updKey)
	updated, err := g.s.reserve(indep, store.ReservationUpdate, nextOf(task, claimed, "IN_PROGRESS"), updKey, "")
	if err != nil {
		t.Fatal(err)
	}
	got = g.answer(t, byKey(task.ID, "update", updKey))
	sameShape(t, "update by key", got, fx["update_receipt_by_key"])
	g.receiptOf(t, "update by key", got, store.ReservationUpdate, updKey, r, updated)
	g.wantNone(t, "an update key never sent", byKey(task.ID, "update", "rk-never-sent"))
	g.wantNone(t, "an operation that is not a member step", byKey(task.ID, "steal", updKey))
	g.wantNone(t, "a malformed key digest", "/v1/identity/peers/aicrew-example/reservations/"+task.ID+"/receipts/update/k1_short")

	// A delayed update behind the hold: once the fence moved past it, it
	// can never commit, and its none is final.
	moved, err := g.s.reserve(indep, store.ReservationUpdate, nextOf(task, updated, "BLOCKED"), "rk-block-"+uuidv7.New(), "")
	if err != nil {
		t.Fatal(err)
	}
	if h := g.answer(t, holdOf(task.ID)); h["fence"] != "3" {
		t.Fatalf("the hold did not move: %v", h)
	}
	lateKey := "rk-late-" + uuidv7.New()
	g.keys = append(g.keys, lateKey)
	_, err = g.s.reserve(indep, store.ReservationUpdate, nextOf(task, updated, "IN_PROGRESS"), lateKey, "")
	if code := refusalCode(err); code != "revision_conflict" && code != "stale_fence" {
		t.Fatalf("a delayed update behind the hold: %v", err)
	}
	g.wantNone(t, "the delayed update", byKey(task.ID, "update", lateKey))

	// The holder's stop release: a receipt by proof for the release, naming
	// the reservation it closed, and closure evidence.
	released, stopProof := g.step(t, indep, store.ReservationRelease, nextOf(task, moved, "READY"), "stopped", "attempt-i")
	g.receiptOf(t, "release by proof", g.answer(t, byProof(stopProof)), store.ReservationRelease, g.lastKey, r, released)
	closed := g.answer(t, holdOf(task.ID))
	sameShape(t, "closed", closed, fx["closed_holder_release"])
	if closed["state"] != "closed" || closed["reservation_id"] != r || closed["closed_by"] != "holder_release" ||
		closed["closing_fence"] != strconv.FormatInt(released.Reservation.Fence, 10) || closed["task_revision"] != float64(released.Task.Revision) {
		t.Fatalf("closed: %v", closed)
	}
	// Aicrew's closure rule: the same reservation, and a fence past the one
	// it last confirmed.
	if cf, _ := strconv.ParseInt(closed["closing_fence"].(string), 10, 64); cf <= moved.Reservation.Fence {
		t.Fatalf("closing fence %d did not advance past %d", cf, moved.Reservation.Fence)
	}
	if at, err := time.Parse(time.RFC3339, closed["closed_at"].(string)); err != nil || time.Since(at) > time.Hour {
		t.Fatalf("closed_at %v", closed["closed_at"])
	}
	// Durable: it reads the same until this service holds the task again.
	if again := g.answer(t, holdOf(task.ID)); !reflect.DeepEqual(again, closed) {
		t.Fatalf("closure evidence changed: %v then %v", closed, again)
	}
	newer, _ := g.claim(t, indep, task.ID, "attempt-j")
	if h := g.answer(t, holdOf(task.ID)); h["state"] != "held" || h["reservation_id"] != newer.Reservation.ID || h["reservation_id"] == r {
		t.Fatalf("a newer reservation: %v", h)
	}
	g.scanForLeaks(t)
}

// Every closed_by value, closure while another member holds the task, and
// the records outside this service's scope, which read none.
func TestReadScopeClosureEvidenceAndScope(t *testing.T) {
	g := newReadRig(t)
	fx := fixtureBodies(t)
	indep := g.member(t, "independent", "agent-indep", "sess-i")
	closedBy := func(t *testing.T, taskID string) map[string]any {
		t.Helper()
		h := g.answer(t, holdOf(taskID))
		if h["state"] != "closed" {
			t.Fatalf("not closed: %v", h)
		}
		return h
	}

	// holder_finalize: the holder finalizes on an accepted-for-finalization
	// fact, with terminal evidence.
	fin := g.readyTask(t, g.alpha)
	held, _ := g.claim(t, indep, fin.ID, "attempt-f")
	done := nextOf(fin, held, "DONE")
	done.TerminalEvidence = []string{"review-head-example", "human-merge-example", "post-merge-ci-example"}
	finished, _ := g.step(t, indep, store.ReservationFinalize, done, "accepted_for_finalization", "attempt-f")
	h := closedBy(t, fin.ID)
	sameShape(t, "closed by finalize", h, fx["closed_holder_finalize"])
	if h["closed_by"] != "holder_finalize" || h["reservation_id"] != held.Reservation.ID ||
		h["closing_fence"] != strconv.FormatInt(finished.Reservation.Fence, 10) {
		t.Fatalf("holder finalize: %v", h)
	}

	// recovery_release and recovery_cancel, through the admin recovery
	// route on an attestation; the answer never names the admin, the
	// attestation or the reason.
	recover := func(t *testing.T, op, state string) (store.Task, store.TaskReservationOutcome) {
		t.Helper()
		task := g.readyTask(t, g.alpha)
		held, _ := g.claim(t, indep, task.ID, "attempt-"+op)
		body, _ := json.Marshal(map[string]any{
			"reservation_id": held.Reservation.ID, "fence": strconv.FormatInt(held.Reservation.Fence, 10),
			"expected_revision": held.Task.Revision, "reason": "the holding worker is gone",
			"content": map[string]any{"title": task.Title, "state": state}, "evidence": attestationEvidence(),
		})
		r := g.call(t, g.tls, "POST", "/v1/admin/reservations/"+task.ID+"/recovery/"+op, g.env,
			map[string]string{"Idempotency-Key": "rec-" + op}, string(body), true)
		if r.status != 200 {
			t.Fatalf("recovery %s: %d %s", op, r.status, r.body)
		}
		return task, held
	}
	relTask, relHeld := recover(t, "release", "READY")
	relClosed := closedBy(t, relTask.ID)
	sameShape(t, "closed by recovery release", relClosed, fx["closed_recovery_release"])
	if relClosed["closed_by"] != "recovery_release" || relClosed["reservation_id"] != relHeld.Reservation.ID ||
		relClosed["closing_fence"] != strconv.FormatInt(relHeld.Reservation.Fence+1, 10) {
		t.Fatalf("recovery release: %v", relClosed)
	}
	canTask, canHeld := recover(t, "cancel", "CANCELLED")
	h = closedBy(t, canTask.ID)
	sameShape(t, "closed by recovery cancel", h, fx["closed_recovery_cancel"])
	if h["closed_by"] != "recovery_cancel" || h["reservation_id"] != canHeld.Reservation.ID {
		t.Fatalf("recovery cancel: %v", h)
	}

	// Another member now holds the released task, in personal mode: the
	// answer is still this service's closed reservation, and says nothing
	// about the new holder.
	bob := g.personal(g.bobIdentity)
	current, err := g.alpha.GetTask(relTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	bobKey := "rk-bob-" + uuidv7.New()
	g.keys = append(g.keys, bobKey)
	bobHold, err := g.s.reserve(bob, store.ReservationClaim, claimOf(current), bobKey, "")
	if err != nil {
		t.Fatal(err)
	}
	h2 := closedBy(t, relTask.ID)
	sameShape(t, "closed while another holds", h2, fx["closed_while_another_holds_the_task"])
	if !reflect.DeepEqual(h2, relClosed) {
		t.Fatalf("closed while another holds: %v, before %v", h2, relClosed)
	}
	for _, name := range []string{g.bobIdentity.UserID, bobHold.Reservation.ID, "own-work", "personal", "standalone"} {
		if strings.Contains(g.bodies[len(g.bodies)-1], name) {
			t.Fatalf("the closure answer discloses the new holder (%s): %s", name, g.bodies[len(g.bodies)-1])
		}
	}
	// Bob's personal receipt is outside the scope, by key too.
	g.wantNone(t, "another member's personal claim", byKey(relTask.ID, "claim", bobKey))

	// A task this service never held, a missing task, an unknown proof and
	// a malformed digest all read none.
	outside := g.readyTask(t, g.alpha)
	if _, err := g.s.reserve(bob, store.ReservationClaim, claimOf(outside), "rk-outside-"+uuidv7.New(), ""); err != nil {
		t.Fatal(err)
	}
	g.wantNone(t, "a hold outside the scope", holdOf(outside.ID))
	sameShape(t, "none", g.answer(t, holdOf(outside.ID)), fx["hold_outside_scope"])
	missing := uuidv7.New()
	g.wantNone(t, "a missing task", holdOf(missing))
	g.wantNone(t, "a missing task's receipt", byKey(missing, "claim", "rk-x"))
	g.wantNone(t, "an unknown proof", byProof(testProof(t)))
	sameShape(t, "receipt none", g.answer(t, byProof(testProof(t))), fx["receipt_none"])
	g.wantNone(t, "a malformed proof digest", "/v1/identity/peers/aicrew-example/reservation-receipts/p1_short")

	for _, body := range g.bodies {
		for _, private := range []string{"ticket-4711", recoveryStatement, "the holding worker is gone", "recovery/env", "attestation"} {
			if strings.Contains(body, private) {
				t.Fatalf("a read-scope answer names the recovery (%q): %s", private, body)
			}
		}
	}
	g.scanForLeaks(t)
}

// scanForLeaks: no read-scope answer carries a proof, a raw request key or
// a bearer, and the log carries no proof or bearer.
func (g *readRig) scanForLeaks(t *testing.T) {
	t.Helper()
	logs := g.logs.String()
	for _, proof := range g.proofs {
		if strings.Contains(logs, proof) {
			t.Fatal("the log carries a proof")
		}
		for _, body := range g.bodies {
			if strings.Contains(body, proof) {
				t.Fatalf("a read answer carries a proof: %s", body)
			}
		}
	}
	for _, key := range g.keys {
		for _, body := range g.bodies {
			if strings.Contains(body, key) {
				t.Fatalf("a read answer carries a raw request key: %s", body)
			}
		}
	}
	g.assertNoSecretLeak(t)
}

// Every refusal the fixtures list, with its status, code and retryable flag,
// plus a revoked credential and a disabled peer. A refusal never carries a
// secret.
func TestReadScopeRefusals(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "fixtures", "coordination-v1", "examples.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		ReadScope struct {
			Refusals []struct {
				Case      string `json:"case"`
				Code      string `json:"code"`
				Status    int    `json:"http_status"`
				Retryable bool   `json:"retryable"`
			} `json:"refusals"`
		} `json:"read_scope"`
	}
	if err := json.Unmarshal(b, &ex); err != nil {
		t.Fatal(err)
	}
	g := newReadRig(t)
	clock := time.Date(2026, 9, 29, 4, 0, 0, 0, time.UTC)
	g.s.readLimit = &readScopeLimiter{now: func() time.Time { return clock }}
	task := g.readyTask(t, g.alpha)
	path := holdOf(task.ID)
	_, redeem := g.issueCredential(t, "aicrew-example", time.Now().Add(time.Hour))
	cases := map[string]func() identityResp{
		"missing_version": func() identityResp { return g.call(t, g.tls, "GET", path, g.read, nil, "", true) },
		"unknown_credential": func() identityResp {
			return g.call(t, g.tls, "GET", path, "aimem_peer_"+strings.Repeat("0", 64), readV1, "", true)
		},
		"redemption_credential_used": func() identityResp { return g.call(t, g.tls, "GET", path, redeem, readV1, "", true) },
		"other_service_path": func() identityResp {
			return g.call(t, g.tls, "GET", "/v1/identity/peers/aicrew-other/reservations/"+task.ID, g.read, readV1, "", true)
		},
		"plain_http": func() identityResp { return g.call(t, g.plain, "GET", path, g.read, readV1, "", true) },
		"rate_limit": func() identityResp {
			// The refusals above count nothing; a minute allows 60 reads.
			for i := 0; i < readScopePerMinute; i++ {
				if r := g.get(t, path); r.status != 200 {
					t.Fatalf("read %d within the bound: %d %s", i+1, r.status, r.body)
				}
			}
			return g.get(t, path)
		},
		"store_busy": func() identityResp {
			// A project that cannot be read: the proof lookup cannot vouch
			// for none, so it refuses, retryably. (A fresh window, whatever
			// order the fixture lists its cases in.)
			clock = clock.Add(time.Minute)
			dir := filepath.Join(g.s.reg.Root(), "projects", "broken")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "journal.db"), []byte("not a database, not at all, not a database"), 0o600); err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			if r := g.get(t, holdOf(uuidv7.New())); r.status != 503 || r.code() != "request_in_progress" {
				t.Fatalf("a hold read past an unreadable project: %d %s", r.status, r.body)
			}
			return g.get(t, byProof(testProof(t)))
		},
	}
	for _, want := range ex.ReadScope.Refusals {
		do, ok := cases[want.Case]
		if !ok {
			t.Fatalf("no test drives the fixture's refusal %q", want.Case)
		}
		r := do()
		var env struct {
			Code          string `json:"code"`
			Retryable     bool   `json:"retryable"`
			NextAction    string `json:"next_action"`
			CorrelationID string `json:"correlation_id"`
		}
		if json.Unmarshal(r.body, &env) != nil || r.status != want.Status || env.Code != want.Code || env.Retryable != want.Retryable ||
			env.NextAction == "" || env.CorrelationID == "" {
			t.Errorf("%s: %d %s, want %d %s retryable=%v", want.Case, r.status, r.body, want.Status, want.Code, want.Retryable)
		}
		delete(cases, want.Case)
	}
	if len(cases) != 0 {
		t.Fatalf("cases the fixture does not list: %v", cases)
	}
	// The next minute allows reads again; the unreadable project is gone.
	clock = clock.Add(time.Minute)
	g.wantNone(t, "after the window", byProof(testProof(t)))
	// No bearer at all, a revoked credential and a disabled peer are
	// unauthenticated.
	if r := g.call(t, g.tls, "GET", path, "", readV1, "", true); r.status != 401 || r.code() != "peer_unauthenticated" {
		t.Fatalf("no bearer: %d %s", r.status, r.body)
	}
	id, other := g.issueCredentialFor(t, "aicrew-example", access.PeerOperationReservationRead, time.Now().Add(time.Hour))
	if r := g.call(t, g.tls, "DELETE", "/v1/identity/peers/aicrew-example/credentials/"+id, g.env, nil, "", true); r.status != 200 {
		t.Fatalf("revoke: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "GET", path, other, readV1, "", true); r.status != 401 || r.code() != "peer_unauthenticated" {
		t.Fatalf("a revoked read credential: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "PUT", "/v1/identity/peers/aicrew-example", g.env, nil, `{"disabled":true}`, true); r.status != 200 {
		t.Fatalf("disable the peer: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "GET", path, g.read, readV1, "", true); r.status != 401 || r.code() != "peer_unauthenticated" {
		t.Fatalf("a disabled peer's read credential: %d %s", r.status, r.body)
	}
	g.assertNoSecretLeak(t)
}
