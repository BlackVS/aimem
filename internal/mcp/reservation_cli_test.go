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
	"strconv"
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

// A mutation that commits at the hub and loses its reply exits 5. The
// receipt for the same key reports it committed, and the same-key retry
// replays it without a second mutation. aicrew's step driver reconciles on
// exactly this path. The test covers a claim, which changes the reservation,
// and an update, which also advances the task revision.
func TestReservationCLICommittedReplyLost(t *testing.T) {
	f := newHub(t)
	ctx := context.Background()
	s := &srv{tasks: fixtureCaller(f, nil)}
	text, isErr := toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "create_task",
		"arguments": map[string]any{"project": "alpha", "title": "lost reply", "state": "READY", "idempotency_key": "lost-create"}}))
	if isErr {
		t.Fatal(text)
	}
	var task struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	json.Unmarshal([]byte(text), &task)

	// The hub serves each POST in full, and the CLI never sees the answer.
	var served int
	lossy := &srv{tasks: func(ctx context.Context, method, path string, headers map[string]string, body []byte) (int, []byte, error) {
		code, resp, err := fixtureCaller(f, nil)(ctx, method, path, headers, body)
		if method != "POST" {
			return code, resp, err
		}
		served++
		if err != nil || code/100 != 2 {
			t.Fatalf("the lossy POST did not commit: %d %s %v", code, resp, err)
		}
		return 0, nil, &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	}}
	type outcome struct {
		Receipt struct {
			Replayed bool `json:"replayed"`
		} `json:"receipt"`
		TaskRevision int64 `json:"task_revision"`
		Reservation  struct {
			ID    string `json:"id"`
			Fence string `json:"fence"`
		} `json:"reservation"`
	}
	status := func() (state, id, fence string) {
		t.Helper()
		out, code := reservationCommand(ctx, s, ReservationRequest{Command: "status", TaskID: task.ID})
		var st struct {
			State         string `json:"state"`
			ReservationID string `json:"reservation_id"`
			Fence         string `json:"fence"`
		}
		if code != ExitCommitted || json.Unmarshal([]byte(out), &st) != nil {
			t.Fatalf("status: exit %d, %s", code, out)
		}
		return st.State, st.ReservationID, st.Fence
	}
	revision := func() int64 {
		t.Helper()
		text, isErr := toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "get_task", "arguments": map[string]any{"id": task.ID}}))
		var got struct {
			Revision int64 `json:"revision"`
		}
		if isErr || json.Unmarshal([]byte(text), &got) != nil {
			t.Fatalf("task read: %s", text)
		}
		return got.Revision
	}
	// snapshot is the hub's state of the task: its hold and its revision.
	type snapshot struct {
		state, id string
		fence     int64
		revision  int64
	}
	now := func() snapshot {
		t.Helper()
		state, id, fence := status()
		n, _ := strconv.ParseInt(fence, 10, 64) // no hold: no fence, 0
		return snapshot{state, id, n, revision()}
	}
	// lose runs req with its reply lost, reconciles it with the receipt, and
	// retries it with the same key. The lost step must have committed exactly
	// once (the fence advanced by one) and the retry must replay it,
	// changing nothing.
	lose := func(req ReservationRequest) (outcome, snapshot, snapshot) {
		t.Helper()
		before, sent := now(), served
		out, code := reservationCommand(ctx, lossy, req)
		if code != ExitUnknown || envelopeCode(t, out) != "receipt_unresolved" || served != sent+1 {
			t.Fatalf("%s with a lost reply: exit %d, served %d, %s", req.Command, code, served-sent, out)
		}
		committed := now()
		if committed.fence != before.fence+1 {
			t.Fatalf("%s with a lost reply did not commit once: %+v, then %+v", req.Command, before, committed)
		}
		out, code = reservationCommand(ctx, s, ReservationRequest{Command: "receipt", TaskID: task.ID, Key: req.Key, Operation: req.Command})
		var receipt struct {
			State      string `json:"state"`
			RequestKey string `json:"request_key"`
			Operation  string `json:"operation"`
		}
		if code != ExitCommitted || json.Unmarshal([]byte(out), &receipt) != nil ||
			receipt.State != "committed" || receipt.RequestKey != req.Key || receipt.Operation != req.Command {
			t.Fatalf("%s receipt after a lost reply: exit %d, %s", req.Command, code, out)
		}
		out, code = reservationCommand(ctx, s, req)
		var o outcome
		if code != ExitCommitted || json.Unmarshal([]byte(out), &o) != nil || !o.Receipt.Replayed {
			t.Fatalf("%s same-key retry: exit %d, %s", req.Command, code, out)
		}
		if after := now(); after != committed || o.Reservation.ID != committed.id ||
			o.Reservation.Fence != strconv.FormatInt(committed.fence, 10) || o.TaskRevision != committed.revision {
			t.Fatalf("%s same-key retry mutated or answered another outcome: committed %+v, after %+v, replay %+v", req.Command, committed, after, o)
		}
		return o, before, committed
	}

	// A claim: the lost step made the hold, and the replay is that hold.
	claimed, _, held := lose(ReservationRequest{Command: "claim", TaskID: task.ID, Key: "lost-claim",
		Body: cliBody(map[string]any{"expected_revision": task.Revision, "holder": map[string]any{"mode": "standalone", "work_ref": "lost"}})})
	if held.state != "held" || held.id == "" {
		t.Fatalf("after a lost claim: %+v", held)
	}

	// An update: the lost step advanced the task once, and the replay
	// leaves it there.
	_, before, updated := lose(ReservationRequest{Command: "update", TaskID: task.ID, Key: "lost-update", Body: cliBody(map[string]any{
		"expected_revision": claimed.TaskRevision, "reservation_id": held.id, "fence": claimed.Reservation.Fence,
		"content": map[string]any{"title": "lost reply", "state": "IN_PROGRESS"}})})
	if updated.revision != before.revision+1 || updated.id != held.id || updated.state != "held" {
		t.Fatalf("after a lost update: %+v, then %+v", before, updated)
	}
}

// A mismatched finalize (C5-w3) and a transition on a project whose tasks
// are off (01a0eda3) are final: the CLI exits 3 and a client never retries
// them with the same key.
func TestReservationCLIFinalRefusals(t *testing.T) {
	for _, code := range []string{"evidence_mismatch", "tasks_disabled"} {
		// The hub's refusal reaches the tool as its envelope (callReservationTool).
		r := &reservationToolRefusal{envelope: `{"code":"` + code + `","message":"m","active_mode":"team","retryable":false,"next_action":"n","correlation_id":"c"}`}
		if got := reservationExit(r); got != ExitFinal {
			t.Fatalf("%s exits %d", code, got)
		}
	}
}

// A team-context refusal reaches the tool exactly as the hub gave it: its
// mode, denied action and role pass through, and no mode is invented
// (19f6 R4, folding 01a0e90a-0f3c).
func TestReservationToolKeepsTheHubsRefusal(t *testing.T) {
	for _, ref := range []teamsession.Refusal{
		{Code: "role_forbidden", Message: "m", ActiveMode: "team", DeniedAction: "reservation.claim", ActiveRole: "worker", NextAction: "Check your aicrew inbox.", CorrelationID: "c-1"},
		{Code: "context_unavailable", Message: "m", Retryable: true, CorrelationID: "c-2"},
	} {
		got := reservationToolError(&ref)
		var env map[string]any
		if err := json.Unmarshal([]byte(got.envelope), &env); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"code": ref.Code, "message": "m", "retryable": ref.Retryable, "correlation_id": ref.CorrelationID}
		if ref.ActiveMode != "" {
			want["active_mode"], want["denied_action"], want["active_role"] = ref.ActiveMode, ref.DeniedAction, ref.ActiveRole
		}
		next := ref.NextAction
		if next == "" {
			next = reservationNextActions[ref.Code]
		}
		want["next_action"] = next
		if len(env) != len(want) {
			t.Fatalf("%s: envelope %v, want %v", ref.Code, env, want)
		}
		for k, v := range want {
			if env[k] != v {
				t.Fatalf("%s: %s is %v, want %v", ref.Code, k, env[k], v)
			}
		}
	}
}
