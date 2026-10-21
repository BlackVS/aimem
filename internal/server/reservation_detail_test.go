package server

// Task 5fcf: invalid_request and unsupported_version carry the validation
// detail in the reservation envelope, since it describes only the caller's
// own input; every other code still sends its fixed message alone.

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"aimem/internal/access"
	"aimem/internal/uuidv7"
)

func TestReservationRefusalDetail(t *testing.T) {
	g := newReservationRig(t)
	task := g.readyTask(t, g.alpha)
	call := func(suffix, key, b string, headers map[string]string) (identityResp, map[string]any) {
		t.Helper()
		if headers == nil {
			headers = rhdr(key, nil)
		}
		r := g.call(t, g.tls, "POST", g.rpath(task.ID, suffix), g.alice, headers, b, true)
		var raw map[string]any
		if err := json.Unmarshal(r.body, &raw); err != nil {
			t.Fatalf("%s: %d %s", suffix, r.status, r.body)
		}
		return r, raw
	}
	detail := func(t *testing.T, what string, r identityResp, raw map[string]any, code string, want ...string) {
		t.Helper()
		wantRefusal(t, what, r, 400, code, "")
		d, _ := raw["detail"].(string)
		for _, w := range want {
			if !strings.Contains(d, w) {
				t.Fatalf("%s: detail %q lacks %q", what, d, w)
			}
		}
	}

	noDetail := func(t *testing.T, what string, r identityResp, status int, code string) {
		t.Helper()
		wantRefusal(t, what, r, status, code, "personal")
		var raw map[string]any
		if json.Unmarshal(r.body, &raw) != nil {
			t.Fatalf("%s: %s", what, r.body)
		}
		if _, ok := raw["detail"]; ok {
			t.Fatalf("%s: a conflict carries a detail: %s", what, r.body)
		}
	}
	// Conflict codes keep their detail in the log: an update of a task
	// nobody holds, and Bob's update of Alice's hold.
	update := body(t, map[string]any{"expected_revision": task.Revision, "reservation_id": uuidv7.New(), "fence": "1",
		"content": map[string]any{"title": task.Title, "state": "IN_PROGRESS"}})
	r, _ := call("/update", "u-none", update, nil)
	noDetail(t, "an update of an unheld task", r, 409, "stale_fence")

	claim := body(t, map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "standalone", "work_ref": "own-1"}})
	held := decodeOutcome(t, g.call(t, g.tls, "POST", g.rpath(task.ID, "/claim"), g.alice, rhdr("c1", nil), claim, true))
	_, bobSecret, err := g.db.IssueScoped("admin", g.bobID, "agent", access.ScopeUser, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	g.secrets = append(g.secrets, bobSecret)
	bobs := body(t, map[string]any{"expected_revision": held.TaskRevision, "reservation_id": held.Reservation.ID, "fence": held.Reservation.Fence,
		"content": map[string]any{"title": task.Title, "state": "IN_PROGRESS"}})
	noDetail(t, "Bob's update of Alice's hold", g.call(t, g.tls, "POST", g.rpath(task.ID, "/update"), bobSecret, rhdr("bob-u", nil), bobs, true),
		409, "reservation_conflict")

	// The observed case: a finalize whose content has a bare-hash commit
	// reference names the field and the rule.
	fin := body(t, map[string]any{"expected_revision": held.TaskRevision, "reservation_id": held.Reservation.ID, "fence": held.Reservation.Fence,
		"reason": "delivered", "terminal_evidence": []string{"review-head", "human-merge"},
		"content": map[string]any{"title": task.Title, "state": "DONE", "candidate_refs": []map[string]any{
			{"kind": "url", "ref": "https://example.com/pr/1"},
			{"kind": "commit", "ref": "034be0d42550534889a1cccf7b5ce98c3ef751f2"},
		}}})
	r, raw := call("/finalize", "f-bad", fin, nil)
	detail(t, "a bare-hash commit reference", r, raw, "invalid_request", "candidate_refs[1]", "a commit reference is an http(s) URL")

	// The wire's own checks name what they refused.
	r, raw = call("/claim", "", claim, map[string]string{"Idempotency-Key": "c2"})
	detail(t, "no version", r, raw, "unsupported_version", reservationVersionHeader)
	r, raw = call("/claim", "", claim, nil)
	detail(t, "no key", r, raw, "invalid_request", "Idempotency-Key")
	r, raw = call("/claim", "c3", strings.Replace(claim, "{", `{"actor_id":"someone",`, 1), nil)
	detail(t, "an unknown field", r, raw, "invalid_request", "actor_id")
	r, raw = call("/claim", "c4", claim+claim, nil)
	detail(t, "two JSON values", r, raw, "invalid_request", "more than one JSON value")
	r, raw = call("/update", "u-fence", strings.Replace(update, `"fence":"1"`, `"fence":"01"`, 1), nil)
	detail(t, "a non-decimal fence", r, raw, "invalid_request", "fence")
	rr := g.call(t, g.tls, "GET", g.rpath(task.ID, "/receipts/steal/u1"), g.alice, rhdr("", nil), "", true)
	var rraw map[string]any
	json.Unmarshal(rr.body, &rraw)
	detail(t, "an unknown receipt operation", rr, rraw, "invalid_request", "claim, transfer, update, release or finalize")

	// The detail is bounded on a rune boundary; the log keeps the whole.
	w := httptest.NewRecorder()
	g.s.reservationRefuseDetail(w, httptest.NewRequest("POST", g.rpath(task.ID, "/claim"), nil), "invalid_request", "x"+strings.Repeat("é", 400))
	var e identityRefusalBody
	if json.Unmarshal(w.Body.Bytes(), &e) != nil || len(e.Detail) > reservationDetailMax || len(e.Detail) < reservationDetailMax-2 || !utf8.ValidString(e.Detail) {
		t.Fatalf("a long detail: %d bytes, %s", len(e.Detail), w.Body)
	}
}
