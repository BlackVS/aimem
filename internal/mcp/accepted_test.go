package mcp

import (
	"net/http"
	"net/http/httptest"

	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/process"
	"aimem/internal/teamsetup/teamsetuptest"
	"aimem/internal/teamstate"
)

// A is the fixture hub's selection (teamsetuptest.Selection, cached by
// Checkout with teamsetuptest.Handbook); B is the one an admin selects
// later, with rules of its own.
var selB = process.Ref{Repo: teamsetuptest.Selection.Repo, Commit: strings.Repeat("bc", 20), Manifest: teamsetuptest.Selection.Manifest}

const rulesA = "Work only on READY tasks"
const rulesB = "B rules: squash every commit before review."

// acceptedFixture is a ready worker, joined, holding an OFFERED attempt,
// with both A and B in this machine's exact cache.
func acceptedFixture(t *testing.T) (*teamsetuptest.Hub, *srv, string, string) {
	t.Helper()
	h, ts := teamsetuptest.New(t)
	h.Reserved = map[string]any{"id": "att-1", "task_id": "task-1", "state": "OFFERED"}
	h.TaskRevision = 3
	repo, root := teamsetuptest.Checkout(t, h, ts)
	teamsetuptest.CacheProcessWith(t, root, selB, "# Handbook B\n\n"+rulesB+"\n")
	s := stdioFor(repo, root)
	if rep := setupWorker(t, s); rep.Status != "joined" {
		t.Fatalf("setup: %+v", rep)
	}
	return h, s, repo, root
}

func accept(t *testing.T, s *srv, key string) (string, bool) {
	t.Helper()
	return callTool(t, s, "team_accept", map[string]any{"team": "team-1", "session_id": "sess-1", "generation": 1, "attempt": "att-1", "idempotency_key": key})
}

func accepted(t *testing.T, repo, root string) *teamstate.AcceptedAttempt {
	t.Helper()
	canon, _ := filepath.EvalSymlinks(repo)
	st, err := teamstate.Load(teamstate.Path(root, canon))
	if err != nil || st == nil {
		t.Fatalf("state: %+v %v", st, err)
	}
	return st.Accepted
}

func writesOf(h *teamsetuptest.Hub, op string) int {
	n := 0
	for _, w := range h.Writes {
		if strings.HasPrefix(w, op+" ") {
			n++
		}
	}
	return n
}

// processBlock is the delivered project process text, "" when none.
func processBlock(blocks []string) string {
	for _, b := range blocks {
		if strings.Contains(b, "=== end aimem process context unit") {
			return b
		}
	}
	return ""
}

// The explicit case: accept under A, an admin selects B, the client
// restarts, continue delivers A pinned (never B as the attempt's rules),
// the worker stays ready and nothing is blocked; process_context agrees.
// Once the attempt is no longer reserved, the record is dropped and B is
// the process for new work.
func TestAcceptedAttemptKeepsItsVersionAcrossASelectionChange(t *testing.T) {
	h, s, repo, root := acceptedFixture(t)
	text, isErr := accept(t, s, "k1")
	rec := accepted(t, repo, root)
	if isErr || rec == nil || rec.Commit != teamsetuptest.Selection.Commit || !rec.Confirmed || rec.SessionID != "sess-1" || rec.Key != "k1" || rec.RoleDigest == "" ||
		!strings.Contains(text, "process version is recorded on this checkout: commit "+teamsetuptest.Selection.Commit) || h.Reserved["state"] != "RUNNING" {
		t.Fatalf("accept: %v %s\nrecord %+v", isErr, text, rec)
	}

	sel := selB
	h.Selection = &sel
	s = stdioFor(repo, root) // a restart: a new MCP process, nothing held in memory
	rep, blocks := onboardBlocks(t, s, "team_continue", map[string]any{})
	proc := processBlock(blocks)
	pp := rep.Readiness.ProjectProcess
	if rep.Status != "joined" || !rep.Readiness.ReadyForWork || pp.State != "ready" || pp.Version != teamsetuptest.Selection.Commit {
		t.Fatalf("continue: %+v", rep.Readiness)
	}
	if !strings.Contains(proc, rulesA) || strings.Contains(proc, rulesB) || !strings.Contains(proc, "PINNED") || !strings.Contains(proc, "The project now selects "+teamsetuptest.Selection.Repo+" @ "+selB.Commit) {
		t.Fatalf("delivered process:\n%s", proc)
	}
	if lvl, detail := rep.check("accepted version"); lvl != "ok" || !strings.Contains(detail, "the project now selects "+selB.Commit+", which applies to new work only") {
		t.Fatalf("accepted version check: %s %s", lvl, detail)
	}
	if writesOf(h, "block") != 0 || h.Reserved["state"] != "RUNNING" {
		t.Fatalf("a recoverable attempt was blocked: %v", h.Writes)
	}
	pc, isErr := callTool(t, s, "process_context", map[string]any{})
	if isErr || !strings.Contains(pc, rulesA) || strings.Contains(pc, rulesB) || !strings.Contains(pc, "accepted attempt att-1") {
		t.Fatalf("process_context: %v\n%s", isErr, pc)
	}

	// The attempt ends: the record goes, and B is the process from now on.
	h.Reserved = nil
	rep, blocks = onboardBlocks(t, s, "team_continue", map[string]any{})
	if accepted(t, repo, root) != nil || !strings.Contains(processBlock(blocks), rulesB) || rep.Readiness.ProjectProcess.Version != selB.Commit {
		t.Fatalf("after the attempt: record %+v, %+v", accepted(t, repo, root), rep.Readiness.ProjectProcess)
	}
	forbidden(t, h, "/accept")
}

// A resume moves the attempt to a new generation; the record still applies.
func TestAcceptedVersionSurvivesAGenerationChange(t *testing.T) {
	h, s, repo, root := acceptedFixture(t)
	if text, isErr := accept(t, s, "k1"); isErr {
		t.Fatal(text)
	}
	sel := selB
	h.Selection = &sel
	for _, sess := range h.Sessions {
		sess.Suspect = true
	}
	rep, blocks := onboardBlocks(t, stdioFor(repo, root), "team_continue", map[string]any{})
	if h.Resumes != 1 || !rep.Readiness.ReadyForWork || !strings.Contains(processBlock(blocks), rulesA) || writesOf(h, "block") != 0 {
		t.Fatalf("resumes %d, %+v, writes %v", h.Resumes, rep.Readiness, h.Writes)
	}
}

// An accept whose reply is lost keeps its record; a retry after B is
// selected (same key: the hub replays the accept; new key: the hub refuses,
// the attempt is already running) never rebinds the attempt to B.
func TestUncertainAcceptKeepsTheVersionItRecorded(t *testing.T) {
	h, s, repo, root := acceptedFixture(t)
	h.DropAfter = map[string]int{"accept": 2}
	if _, isErr := accept(t, s, "k1"); !isErr {
		t.Fatal("a lost reply was reported as a success")
	}
	rec := accepted(t, repo, root)
	if rec == nil || rec.Commit != teamsetuptest.Selection.Commit || rec.Confirmed || h.Reserved["state"] != "RUNNING" {
		t.Fatalf("record after an uncertain accept: %+v, reserved %v", rec, h.Reserved)
	}
	sel := selB
	h.Selection = &sel
	if text, isErr := accept(t, s, "k1"); isErr {
		t.Fatalf("replay: %s", text)
	}
	if rec = accepted(t, repo, root); rec.Commit != teamsetuptest.Selection.Commit || !rec.Confirmed {
		t.Fatalf("replay rebound the attempt: %+v", rec)
	}
	if _, isErr := accept(t, s, "k2"); !isErr {
		t.Fatal("a second accept of a running attempt succeeded")
	}
	if rec = accepted(t, repo, root); rec == nil || rec.Commit != teamsetuptest.Selection.Commit {
		t.Fatalf("a refused retry dropped or rebound the record: %+v", rec)
	}
	forbidden(t, h, "/accept")
}

// Nothing is accepted without a record: no saved membership for the
// session, or a project process that is not ready, refuses locally and
// sends nothing.
func TestAcceptIsRefusedLocallyWhenTheVersionCannotBeRecorded(t *testing.T) {
	h, s, repo, root := acceptedFixture(t)
	text, isErr := callTool(t, s, "team_accept", map[string]any{"team": "team-1", "session_id": "someone-else", "generation": 1, "attempt": "att-1", "idempotency_key": "k1"})
	if !isErr || !strings.Contains(text, "refused locally, nothing was sent") || writesOf(h, "accept") != 0 {
		t.Fatalf("foreign session: %v %s, writes %v", isErr, text, h.Writes)
	}
	h.Selection = nil
	text, isErr = accept(t, s, "k1")
	if !isErr || !strings.Contains(text, "the project process is not_selected") || writesOf(h, "accept") != 0 || accepted(t, repo, root) != nil {
		t.Fatalf("not ready: %v %s, writes %v", isErr, text, h.Writes)
	}
	forbidden(t, h)
}

// A RUNNING attempt whose version cannot be established is not ready and
// is blocked (the merged policy), and the current selection is never
// delivered as its rules: no record, a malformed record, a record of
// another session, the exact commit unavailable, access to it denied.
func TestUnrecoverableAcceptedVersionBlocksTheAttempt(t *testing.T) {
	cases := map[string]struct {
		prepare func(t *testing.T, h *teamsetuptest.Hub, repo, root string)
		want    string
	}{
		"no record": {func(t *testing.T, h *teamsetuptest.Hub, repo, root string) {}, "not_recorded"},
		"malformed record": {func(t *testing.T, h *teamsetuptest.Hub, repo, root string) {
			record(t, repo, root, &teamstate.AcceptedAttempt{Attempt: "att-1", SessionID: "sess-1", Repo: "not a url", Commit: "xyz", Manifest: "m.json"})
		}, "not_recorded"},
		"another session's record": {func(t *testing.T, h *teamsetuptest.Hub, repo, root string) {
			a := teamsetuptest.Selection
			record(t, repo, root, &teamstate.AcceptedAttempt{Attempt: "att-1", SessionID: "sess-9", Repo: a.Repo, Commit: a.Commit, Manifest: a.Manifest})
		}, "not_recorded"},
		"exact commit unavailable": {func(t *testing.T, h *teamsetuptest.Hub, repo, root string) {
			a := process.Ref{Repo: teamsetuptest.Selection.Repo, Commit: strings.Repeat("de", 20), Manifest: teamsetuptest.Selection.Manifest}
			record(t, repo, root, &teamstate.AcceptedAttempt{Attempt: "att-1", SessionID: "sess-1", Repo: a.Repo, Commit: a.Commit, Manifest: a.Manifest})
		}, "unavailable"},
		"access denied": {func(t *testing.T, h *teamsetuptest.Hub, repo, root string) {
			git := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("WWW-Authenticate", `Basic realm="process"`)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			t.Cleanup(git.Close)
			t.Setenv("GIT_SSL_NO_VERIFY", "1")
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "none"))
			record(t, repo, root, &teamstate.AcceptedAttempt{Attempt: "att-1", SessionID: "sess-1", Repo: git.URL + "/process.git", Commit: strings.Repeat("ef", 20), Manifest: teamsetuptest.Selection.Manifest})
		}, "denied"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h, ts := teamsetuptest.New(t)
			h.TaskRevision = 3
			repo, root := teamsetuptest.Checkout(t, h, ts)
			s := stdioFor(repo, root)
			setupWorker(t, s)
			h.Reserved = map[string]any{"id": "att-1", "task_id": "task-1", "state": "RUNNING"}
			c.prepare(t, h, repo, root)
			rep, blocks := onboardBlocks(t, s, "team_continue", map[string]any{})
			pp := rep.Readiness.ProjectProcess
			if rep.Readiness.ReadyForWork || pp.State != c.want {
				t.Fatalf("readiness: %+v", rep.Readiness)
			}
			if processBlock(blocks) != "" {
				t.Fatalf("the current selection was delivered for an attempt whose version is unknown:\n%s", processBlock(blocks))
			}
			if writesOf(h, "block") != 1 || h.Reserved["state"] != "BLOCKED" || h.HeartbeatAvailability[len(h.HeartbeatAvailability)-1] != "unavailable" {
				t.Fatalf("writes %v, reserved %v, heartbeats %v", h.Writes, h.Reserved, h.HeartbeatAvailability)
			}
			forbidden(t, h)
		})
	}
}

func record(t *testing.T, repo, root string, a *teamstate.AcceptedAttempt) {
	t.Helper()
	canon, _ := filepath.EvalSymlinks(repo)
	path := teamstate.Path(root, canon)
	st, err := teamstate.Load(path)
	if err != nil || st == nil {
		t.Fatalf("state: %+v %v", st, err)
	}
	st.Accepted = a
	if err := teamstate.Save(path, st); err != nil {
		t.Fatal(err)
	}
}

// A definite refusal of a fresh accept withdraws the record it made: the
// attempt was never accepted, so no version is bound to it.
func TestRefusedAcceptWithdrawsItsRecord(t *testing.T) {
	h, s, repo, root := acceptedFixture(t)
	h.WorkCode = map[string]int{"accept": 409}
	if _, isErr := accept(t, s, "k1"); !isErr {
		t.Fatal("a refused accept succeeded")
	}
	if rec := accepted(t, repo, root); rec != nil || h.Reserved["state"] != "OFFERED" {
		t.Fatalf("record after a refusal: %+v, reserved %v", rec, h.Reserved)
	}
	forbidden(t, h, "/accept")
}

// process_context pins only a record of this checkout's current project.
func TestProcessContextIgnoresARecordOfAnotherProject(t *testing.T) {
	h, s, repo, root := acceptedFixture(t)
	if text, isErr := accept(t, s, "k1"); isErr {
		t.Fatal(text)
	}
	canon, _ := filepath.EvalSymlinks(repo)
	path := teamstate.Path(root, canon)
	st, _ := teamstate.Load(path)
	st.Project = "another-project"
	if err := teamstate.Save(path, st); err != nil {
		t.Fatal(err)
	}
	pc, isErr := callTool(t, s, "process_context", map[string]any{})
	if isErr || strings.Contains(pc, "PINNED") || !strings.Contains(pc, rulesA) {
		t.Fatalf("%v\n%s", isErr, pc)
	}
	forbidden(t, h, "/accept")
}
