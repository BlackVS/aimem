package mcp

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aimem/internal/teamsetup/teamsetuptest"
	"aimem/internal/teamstate"
)

// attemptReport is the part of the onboarding report these tests read.
type attemptReport struct {
	Status   string   `json:"status"`
	Next     []string `json:"next"`
	Reserved *struct {
		ID, State, Next string
	} `json:"reserved"`
	Checks []struct {
		Name, Level, Detail string
	} `json:"checks"`
}

func (r attemptReport) attemptCheck() string {
	for _, c := range r.Checks {
		if c.Name == "attempt" {
			return c.Level + " " + c.Detail
		}
	}
	return ""
}

// unreadyWorker is a hub whose project has no selected process (so the
// worker is not ready) and which holds attempt att-1 for it in state.
func unreadyWorker(t *testing.T, state string) (*teamsetuptest.Hub, *srv, string, string) {
	t.Helper()
	h, ts := teamsetuptest.New(t)
	h.Selection = nil
	h.Reserved = map[string]any{"id": "att-1", "task_id": "task-1", "state": state}
	h.TaskRevision = 3
	repo, root := teamsetuptest.Checkout(t, h, ts)
	return h, stdioFor(repo, root), repo, root
}

func setupWorker(t *testing.T, s *srv) attemptReport {
	t.Helper()
	text, isErr := callTool(t, s, "team_setup", map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile})
	if isErr {
		t.Fatalf("team_setup: %s", text)
	}
	var rep attemptReport
	if err := json.Unmarshal([]byte(text), &rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func pending(t *testing.T, repo, root string) *teamstate.PendingWork {
	t.Helper()
	canon, _ := filepath.EvalSymlinks(repo)
	st, err := teamstate.Load(teamstate.Path(root, canon))
	if err != nil || st == nil {
		t.Fatalf("state: %+v %v", st, err)
	}
	return st.PendingWork
}

// forbidden fails on any request the not-ready handling must never make;
// allowed names operations the test itself sends on purpose.
func forbidden(t *testing.T, h *teamsetuptest.Hub, allowed ...string) {
	t.Helper()
	for _, req := range h.Requests {
		for _, op := range []string{"/accept", "/stopped", "/resume-work", "/leave", "/withdraw", "/cancel", "/close-stop", "/submit"} {
			if strings.HasSuffix(req, op) && !slices.Contains(allowed, op) {
				t.Fatalf("forbidden request %s in %v", req, h.Requests)
			}
		}
	}
}

// never is a report that tells a member that is not ready to take the
// reserved work: accept it, continue it or resume it.
func neverTellsToTakeTheWork(t *testing.T, rep attemptReport) {
	t.Helper()
	lines := append([]string{}, rep.Next...)
	if rep.Reserved != nil {
		lines = append(lines, rep.Reserved.Next)
	}
	for _, l := range lines {
		for _, bad := range []string{"team_accept or team_decline", "continue the accepted work", "then team_resume_work when"} {
			if strings.Contains(l, bad) {
				t.Fatalf("a not-ready worker was told %q: %q", bad, l)
			}
		}
	}
}

// OFFERED (an offer that raced the join: the join made the session
// available, the offer landed before the unavailable heartbeat): declined
// once with the readiness reason; the hub no longer reserves it; a repeat
// run sends nothing.
func TestUnreadyWorkerDeclinesAReservedOffer(t *testing.T) {
	h, s, repo, root := unreadyWorker(t, "OFFERED")
	rep := setupWorker(t, s)
	if len(h.Writes) != 1 || !strings.HasPrefix(h.Writes[0], "decline att-1 ") || h.Reserved != nil {
		t.Fatalf("writes %v, reserved %v", h.Writes, h.Reserved)
	}
	if rep.Reserved == nil || rep.Reserved.State != "DECLINED" || !strings.Contains(rep.attemptCheck(), "declined") || !strings.Contains(rep.attemptCheck(), "not ready for work (project_process not_selected)") {
		t.Fatalf("report: %+v / %s", rep.Reserved, rep.attemptCheck())
	}
	neverTellsToTakeTheWork(t, rep)
	if pending(t, repo, root) != nil {
		t.Fatal("a confirmed write left its retry record")
	}
	// The unavailable announcement precedes the decline: no further offer
	// can land while the raced one is being declined.
	hb, dec := -1, -1
	for i, req := range h.Requests {
		if strings.HasSuffix(req, "/heartbeat") && hb < 0 {
			hb = i
		}
		if strings.HasSuffix(req, "/decline") {
			dec = i
		}
	}
	if hb < 0 || dec < hb || h.HeartbeatAvailability[0] != "unavailable" {
		t.Fatalf("order: heartbeat %d decline %d %v", hb, dec, h.HeartbeatAvailability)
	}
	setupWorker(t, s)
	if len(h.Writes) != 1 || h.Count("/join") != 1 {
		t.Fatalf("repeat: writes %v joins %d", h.Writes, h.Count("/join"))
	}
	forbidden(t, h)
}

// RUNNING: blocked once, naming the task's current revision (read, never
// guessed); the attempt stays reserved; a repeat preserves BLOCKED.
func TestUnreadyWorkerBlocksRunningWorkAndPreservesIt(t *testing.T) {
	h, s, _, _ := unreadyWorker(t, "RUNNING")
	rep := setupWorker(t, s)
	if len(h.Writes) != 1 || !strings.HasPrefix(h.Writes[0], "block att-1 ") || h.TaskReads != 1 || h.TaskRevision != 4 || h.Reserved["state"] != "BLOCKED" {
		t.Fatalf("writes %v, task reads %d, revision %d, reserved %v", h.Writes, h.TaskReads, h.TaskRevision, h.Reserved)
	}
	if rep.Reserved.State != "BLOCKED" || !strings.Contains(rep.Reserved.Next, "keep it blocked") {
		t.Fatalf("report: %+v", rep.Reserved)
	}
	neverTellsToTakeTheWork(t, rep)
	rep = setupWorker(t, s)
	if len(h.Writes) != 1 || !strings.Contains(rep.attemptCheck(), "already BLOCKED: preserved") {
		t.Fatalf("repeat: writes %v, %s", h.Writes, rep.attemptCheck())
	}
	forbidden(t, h)
}

// BLOCKED, STOP_REQUESTED, STOPPED and SUBMITTED: nothing is sent; a stop
// is never acknowledged for the worker.
func TestUnreadyWorkerSendsNothingForOtherStates(t *testing.T) {
	for state, want := range map[string]string{
		"BLOCKED":        "keep it blocked",
		"STOP_REQUESTED": "send team_stopped only once you have established that the work stopped",
		"STOPPED":        "wait for the coordinator's close-stop",
		"SUBMITTED":      "your result is under review",
	} {
		h, s, _, _ := unreadyWorker(t, state)
		rep := setupWorker(t, s)
		if len(h.Writes) != 0 || h.TaskReads != 0 || h.Reserved["state"] != state {
			t.Fatalf("%s: writes %v, reserved %v", state, h.Writes, h.Reserved)
		}
		if rep.Reserved == nil || !strings.Contains(rep.Reserved.Next, want) {
			t.Fatalf("%s: reserved %+v", state, rep.Reserved)
		}
		neverTellsToTakeTheWork(t, rep)
		forbidden(t, h)
	}
}

// sameKey reports whether every recorded write used one retry key.
func sameKey(writes []string) bool {
	for _, w := range writes {
		if w != writes[0] {
			return false
		}
	}
	return len(writes) > 0
}

// A decline that landed but whose reply was lost (on the transport's own
// keyed replay too) keeps its retry record; the next run sees the attempt
// gone and drops the record without sending anything.
func TestUncertainWriteThatLandedIsNotRepeated(t *testing.T) {
	h, s, repo, root := unreadyWorker(t, "OFFERED")
	h.DropAfter = map[string]int{"decline": 2}
	rep := setupWorker(t, s)
	if w := pending(t, repo, root); w == nil || w.Op != "decline" || !strings.Contains(rep.attemptCheck(), "outcome is unknown") || h.Reserved != nil || !sameKey(h.Writes) {
		t.Fatalf("pending %+v, %s, reserved %v, writes %v", w, rep.attemptCheck(), h.Reserved, h.Writes)
	}
	if rep.Reserved.State != "OFFERED" || !strings.Contains(rep.Reserved.Next, "do not accept it") {
		t.Fatalf("an unconfirmed decline must not be reported as done: %+v", rep.Reserved)
	}
	sent := len(h.Writes)
	rep = setupWorker(t, s)
	if len(h.Writes) != sent || pending(t, repo, root) != nil || !strings.Contains(rep.attemptCheck(), "not repeated: the attempt is no longer reserved") {
		t.Fatalf("writes %v, %s", h.Writes, rep.attemptCheck())
	}
	forbidden(t, h)
}

// A block that never landed is replayed as the identical request with the
// same key, and lands once.
func TestUncertainWriteThatDidNotLandIsReplayedIdentically(t *testing.T) {
	h, s, repo, root := unreadyWorker(t, "RUNNING")
	h.DropBefore = map[string]int{"block": 2}
	setupWorker(t, s)
	w := pending(t, repo, root)
	if w == nil || w.ExpectedRevision != 3 || h.Reserved["state"] != "RUNNING" || h.TaskRevision != 3 {
		t.Fatalf("pending %+v, reserved %v, revision %d", w, h.Reserved, h.TaskRevision)
	}
	sent := len(h.Writes)
	setupWorker(t, s)
	if len(h.Writes) != sent+1 || !sameKey(h.Writes) || h.Reserved["state"] != "BLOCKED" || pending(t, repo, root) != nil {
		t.Fatalf("writes %v, reserved %v", h.Writes, h.Reserved)
	}
	if h.TaskReads != 1 {
		t.Fatalf("the replay re-read the revision instead of repeating the recorded request: %d reads", h.TaskReads)
	}
	forbidden(t, h)
}

// A resume between the runs changes the generation: the recorded request is
// not the request this session would send; a new one is sent.
func TestPendingWriteFromAnotherGenerationIsNotReplayed(t *testing.T) {
	h, s, repo, root := unreadyWorker(t, "RUNNING")
	h.DropBefore = map[string]int{"block": 2}
	setupWorker(t, s)
	first := h.Writes[0]
	for _, sess := range h.Sessions {
		sess.Suspect = true
	}
	text, isErr := callTool(t, s, "team_continue", map[string]any{})
	if isErr || h.Resumes != 1 {
		t.Fatalf("continue: %v %s", isErr, text)
	}
	last := h.Writes[len(h.Writes)-1]
	if last == first || h.Reserved["state"] != "BLOCKED" || pending(t, repo, root) != nil {
		t.Fatalf("writes %v, reserved %v", h.Writes, h.Reserved)
	}
	forbidden(t, h)
}

// A refused write changes nothing, drops its record, and the report still
// never tells the worker to accept.
func TestRefusedWriteIsReported(t *testing.T) {
	h, s, repo, root := unreadyWorker(t, "OFFERED")
	h.WorkCode = map[string]int{"decline": 403}
	rep := setupWorker(t, s)
	if h.Reserved["state"] != "OFFERED" || pending(t, repo, root) != nil || !strings.Contains(rep.attemptCheck(), "was refused") {
		t.Fatalf("reserved %v, %s", h.Reserved, rep.attemptCheck())
	}
	if !strings.Contains(rep.Reserved.Next, "do not accept it; decline it with the readiness reason") {
		t.Fatalf("reserved next: %q", rep.Reserved.Next)
	}
	neverTellsToTakeTheWork(t, rep)
	forbidden(t, h)
}

// A heartbeat the hub refuses stops the run before any attempt write.
func TestHeartbeatFailureSendsNoAttemptWrite(t *testing.T) {
	h, s, _, _ := unreadyWorker(t, "OFFERED")
	h.Heartbeat = 403
	setupWorker(t, s)
	if len(h.Writes) != 0 || h.Reserved["state"] != "OFFERED" {
		t.Fatalf("writes %v, reserved %v", h.Writes, h.Reserved)
	}
	forbidden(t, h)
}

// A ready worker's offer is left to the worker; a record left by an earlier
// not-ready run is dropped, not replayed.
func TestReadyWorkerSendsNothingAndDropsAStaleRecord(t *testing.T) {
	h, s, repo, root := unreadyWorker(t, "OFFERED")
	h.DropBefore = map[string]int{"decline": 2}
	setupWorker(t, s)
	if pending(t, repo, root) == nil {
		t.Fatal("no record of the uncertain decline")
	}
	sent := len(h.Writes)
	sel := teamsetuptest.Selection
	h.Selection = &sel
	rep := setupWorker(t, s)
	if len(h.Writes) != sent || h.Reserved["state"] != "OFFERED" || pending(t, repo, root) != nil || !strings.Contains(rep.attemptCheck(), "this session is ready now") {
		t.Fatalf("writes %v, reserved %v, %s", h.Writes, h.Reserved, rep.attemptCheck())
	}
	if !strings.Contains(rep.Reserved.Next, "team_accept or team_decline") {
		t.Fatalf("a ready worker's offer guidance: %q", rep.Reserved.Next)
	}
	forbidden(t, h)
}
