package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/taskcred"
	"aimem/internal/teamsession"
)

const cliProof = "acp1_CLIPROOFCLIPROOFCLIPROOFCLIPROOFCLIPROOFCLI"

// fixtureCaller is a personal task caller for the test hub: Alice's own
// token, through the hub's real routes. fail, when set, may replace a call's
// transport with an error.
func fixtureCaller(f *hubFixture, fail func(method, path string) error) TaskCallFunc {
	return func(ctx context.Context, method, path string, headers map[string]string, body []byte) (int, []byte, error) {
		if fail != nil {
			if err := fail(method, path); err != nil {
				return 0, nil, err
			}
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+f.alice)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		return w.Code, w.Body.Bytes(), nil
	}
}

func cliBody(pairs map[string]any) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range pairs {
		b, _ := json.Marshal(v)
		out[k] = b
	}
	return out
}

func envelopeCode(t *testing.T, text string) string {
	t.Helper()
	var e struct {
		Code          string `json:"code"`
		CorrelationID string `json:"correlation_id"`
		NextAction    string `json:"next_action"`
	}
	if json.Unmarshal([]byte(text), &e) != nil || e.CorrelationID == "" || e.NextAction == "" {
		t.Fatalf("not a refusal envelope: %s", text)
	}
	return e.Code
}

// The personal CLI through the hub's real routes: each exit code, the
// outcome as one JSON document, and never the proof.
func TestReservationCLIPersonal(t *testing.T) {
	f := newHub(t)
	s := &srv{tasks: fixtureCaller(f, nil)}
	text, isErr := toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "create_task",
		"arguments": map[string]any{"project": "alpha", "title": "cli work", "state": "READY", "idempotency_key": "cli-create"}}))
	if isErr {
		t.Fatal(text)
	}
	var task struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	json.Unmarshal([]byte(text), &task)
	ctx := context.Background()
	run := func(req ReservationRequest) (string, int) {
		out, code := reservationCommand(ctx, s, req)
		if strings.Contains(out, cliProof) {
			t.Fatalf("the proof was printed: %s", out)
		}
		return out, code
	}
	claim := ReservationRequest{Command: "claim", TaskID: task.ID, Key: "cli-claim",
		Body: cliBody(map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "standalone", "work_ref": "cli"}})}
	out, code := run(claim)
	var o struct {
		Receipt struct {
			Replayed bool `json:"replayed"`
		} `json:"receipt"`
		TaskRevision int64 `json:"task_revision"`
		Reservation  struct {
			ID    string `json:"id"`
			Fence string `json:"fence"`
		} `json:"reservation"`
	}
	if code != ExitCommitted || json.Unmarshal([]byte(out), &o) != nil || o.Reservation.Fence != "1" {
		t.Fatalf("claim: %d %s", code, out)
	}
	if out, code = run(claim); code != ExitCommitted || !strings.Contains(out, `"replayed": true`) {
		t.Fatalf("a replay exits 0: %d %s", code, out)
	}
	// A proof on a personal step is refused, final, and not printed back.
	withProof := claim
	withProof.Key = "cli-proof"
	withProof.Body = cliBody(map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "standalone", "work_ref": "cli"},
		"coordination_proof": cliProof})
	if out, code = run(withProof); code != ExitFinal || envelopeCode(t, out) != "invalid_request" {
		t.Fatalf("a personal step with a proof: %d %s", code, out)
	}
	stale := ReservationRequest{Command: "update", TaskID: task.ID, Key: "cli-stale", Body: cliBody(map[string]any{
		"expected_revision": o.TaskRevision, "reservation_id": o.Reservation.ID, "fence": "9",
		"content": map[string]any{"title": "cli work", "state": "IN_PROGRESS"}})}
	if out, code = run(stale); code != ExitFinal || envelopeCode(t, out) != "stale_fence" {
		t.Fatalf("a stale fence exits 3: %d %s", code, out)
	}
	if out, code = run(ReservationRequest{Command: "receipt", TaskID: task.ID, Key: "cli-claim", Operation: "claim"}); code != ExitCommitted ||
		!strings.Contains(out, `"state": "committed"`) {
		t.Fatalf("receipt: %d %s", code, out)
	}
	if out, code = run(ReservationRequest{Command: "status", TaskID: task.ID}); code != ExitCommitted || !strings.Contains(out, `"state": "held"`) {
		t.Fatalf("status: %d %s", code, out)
	}
	if out, code = run(ReservationRequest{Command: "status", TaskID: "01a0ffff-ffff-7000-8000-000000000000"}); code != ExitFinal ||
		envelopeCode(t, out) != "task_unavailable" {
		t.Fatalf("an unknown task: %d %s", code, out)
	}
	// The hub not answering is retryable (exit 4), however the layer that
	// met it words the error: a real refused connection, as the caller and
	// the local credential's check wrap it, and a truncated answer.
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	_, refused := http.Get(closed.URL)
	if refused == nil {
		t.Fatal("a closed server answered")
	}
	for _, e := range []error{
		fmt.Errorf("hub unreachable: %w", refused),
		fmt.Errorf("cannot validate local task credential: %w", refused),
		io.ErrUnexpectedEOF,
		&net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")},
	} {
		down := &srv{tasks: fixtureCaller(f, func(string, string) error { return e })}
		if out, code = reservationCommand(ctx, down, ReservationRequest{Command: "status", TaskID: task.ID}); code != ExitRetryable ||
			envelopeCode(t, out) != "context_unavailable" {
			t.Fatalf("an unreachable hub (%v): %d %s", e, code, out)
		}
	}
	// A refusal the caller decided without the hub's answer stays final.
	for _, e := range []error{
		&taskcred.Rejected{Status: http.StatusForbidden},
		errors.New("local task credential must be project-scoped with current write access to this project; no fallback"),
	} {
		refusing := &srv{tasks: fixtureCaller(f, func(string, string) error { return e })}
		if out, code = reservationCommand(ctx, refusing, ReservationRequest{Command: "status", TaskID: task.ID}); code != ExitFinal ||
			envelopeCode(t, out) != "task_unavailable" {
			t.Fatalf("a local refusal (%v): %d %s", e, code, out)
		}
	}
	// A mutation sent and never answered: exit 5, reconcile with the key.
	lost := &srv{tasks: fixtureCaller(f, func(method, _ string) error {
		if method == "POST" {
			return errors.New("hub unreachable: connection reset")
		}
		return nil
	})}
	update := ReservationRequest{Command: "update", TaskID: task.ID, Key: "cli-lost", Body: cliBody(map[string]any{
		"expected_revision": o.TaskRevision, "reservation_id": o.Reservation.ID, "fence": "1",
		"content": map[string]any{"title": "cli work", "state": "IN_PROGRESS"}})}
	if out, code = reservationCommand(ctx, lost, update); code != ExitUnknown || envelopeCode(t, out) != "receipt_unresolved" {
		t.Fatalf("an unanswered mutation: %d %s", code, out)
	}
	// The same key, answered this time: it commits, and the receipt shows it.
	if out, code = run(update); code != ExitCommitted {
		t.Fatalf("the same key after an unknown outcome: %d %s", code, out)
	}
}

// In team mode the CLI is that team conversation: every call carries the
// session's handle, a context the hub refuses is typed, and a session that
// cannot be used blocks with nothing sent, never falling back to personal.
func TestReservationCLITeam(t *testing.T) {
	h := newTeamHub(t)
	root := teamRoot(t, h, nil)
	h.addSession(teamHandle('C'), "sess-cli")
	var sawProof bool
	h.custom = func(w http.ResponseWriter, r *http.Request, session string) bool {
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/tasks/t-1":
			json.NewEncoder(w).Encode(map[string]any{"id": "t-1", "project": "alpha", "revision": 3})
		case r.Method == "POST" && r.URL.Path == "/v1/projects/alpha/tasks/t-1/reservation/claim":
			raw, _ := io.ReadAll(r.Body)
			sawProof = strings.Contains(string(raw), cliProof)
			if r.Header.Get("X-Aimem-Reservation-Version") != "1" || r.Header.Get("Idempotency-Key") != "team-claim" {
				w.WriteHeader(http.StatusBadRequest)
				return true
			}
			json.NewEncoder(w).Encode(map[string]any{
				"receipt":       map[string]any{"id": "rcpt_x", "state": "committed", "operation": "claim", "request_key": "team-claim", "replayed": false, "actor_id": "user-1", "verified_mode": "team"},
				"task_revision": 3, "reservation": map[string]any{"id": "r-1", "fence": "1", "active": true, "holder_mode": "external", "own_work_ref": "offer-1"}})
		default:
			return false
		}
		return true
	}
	s := newTeamSrv(writeSession(t, root, h.ts.URL, "sess-cli", teamHandle('C')), root, "")
	out, code := reservationCommand(context.Background(), s, ReservationRequest{Command: "claim", TaskID: "t-1", Key: "team-claim",
		Body: cliBody(map[string]any{"expected_revision": 3, "holder": map[string]any{"mode": "external", "work_ref": "offer-1"}, "coordination_proof": cliProof})})
	if code != ExitCommitted || !strings.Contains(out, `"verified_mode": "team"`) || strings.Contains(out, cliProof) || !sawProof {
		t.Fatalf("team claim: %d %s (proof sent %v)", code, out, sawProof)
	}
	for _, rec := range h.requests() {
		if rec.handle != teamHandle('C') {
			t.Fatalf("a team CLI call went without the session's handle: %+v", rec)
		}
	}
	// The hub ending the context after it was verified: the task read's
	// refusal keeps the hub's code.
	h.stale[teamHandle('C')] = true
	if out, code := reservationCommand(context.Background(), s, ReservationRequest{Command: "status", TaskID: "t-1"}); code != ExitFinal ||
		envelopeCode(t, out) != "context_stale" {
		t.Fatalf("a context ended after verification: %d %s", code, out)
	}
	// A context the hub refuses: its envelope, final.
	stale := newTeamSrv(writeSession(t, root, h.ts.URL, "sess-gone", teamHandle('D')), root, "")
	if out, code := reservationCommand(context.Background(), stale, ReservationRequest{Command: "status", TaskID: "t-1"}); code != ExitFinal ||
		envelopeCode(t, out) != "context_stale" {
		t.Fatalf("a stale context: %d %s", code, out)
	}
	// AIMEM_TEAM_SESSION naming a file that cannot be used: the production
	// chooser serves that team conversation, blocked, and sends nothing: no
	// fallback to the personal credential.
	before := len(h.requests())
	t.Setenv(teamsession.EnvVar, filepath.Join(t.TempDir(), "missing-session.json"))
	if out, code := ReservationCommand(context.Background(), ReservationRequest{Command: "status", TaskID: "t-1"}); code != ExitFinal ||
		envelopeCode(t, out) != "context_missing" {
		t.Fatalf("an unusable team session: %d %s", code, out)
	}
	if len(h.requests()) != before {
		t.Fatal("an unusable team session reached a hub")
	}
}
