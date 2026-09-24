package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/teamsetup"
	"aimem/internal/teamsetup/teamsetuptest"
	"aimem/internal/teamstate"
)

func runSetup(t *testing.T, repo, root string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runTeamSetup(args, repo, root, &out)
	if strings.Contains(out.String(), "aimem_user_") {
		t.Fatalf("token leaked into output: %s", out.String())
	}
	return out.String(), err
}

func readState(t *testing.T, root, repo string) *teamstate.State {
	t.Helper()
	canon, _ := filepath.EvalSymlinks(repo)
	raw, err := os.ReadFile(teamstate.Path(root, canon))
	if err != nil {
		return nil
	}
	if strings.Contains(string(raw), "aimem_user_") {
		t.Fatal("token leaked into the state file")
	}
	var st teamstate.State
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return &st
}

func TestTeamSetupJoinsOnceAndVerifiesOnRepeat(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	out, err := runSetup(t, repo, root, "Pilot", "coordinator", "--platform", "claude-code", "--platform-version", "2.1", "--model-id", "claude-fable-5-1", "--model-source", "runtime_reported", "--model-provider", "anthropic")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"joined as coordinator: session sess-1, generation 1", "Roster (1)", "coordinator playbook", "Status: joined", "identity     user pilot-a, scope project, write granted, tasks enabled", "claude-fable-5-1 (runtime_reported)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	st := readState(t, root, repo)
	if st == nil || st.SessionID != "sess-1" || st.Generation != 1 || st.TokenID != "t-1" || st.TeamID != "team-1" || st.JoinKey != "" || st.Role != "coordinator" {
		t.Fatalf("state %+v", st)
	}
	// Second invocation: verified through the roster, no second join, no resume.
	out, err = runSetup(t, repo, root, "Pilot", "coordinator")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "already joined as coordinator: session sess-1, generation 1") || h.Count("/join") != 1 || h.Resumes != 0 {
		t.Fatalf("repeat invocation: joins %d resumes %d\n%s", h.Count("/join"), h.Resumes, out)
	}
	// A different role for the same checkout is refused, not silently switched.
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "already holds a coordinator session") {
		t.Fatalf("role switch: %v\n%s", err, out)
	}
	// JSON report carries the same facts.
	out, err = runSetup(t, repo, root, "Pilot", "coordinator", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rep teamsetup.Report
	if json.Unmarshal([]byte(out), &rep) != nil || rep.Status != "joined" || rep.Session == nil || rep.Session.ID != "sess-1" {
		t.Fatalf("json report: %s", out)
	}
}

func TestTeamSetupWorkerEntersWaiting(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	h.InboxN = 2
	h.Reserved = map[string]any{"id": "att-1", "task_id": "task-1", "state": "OFFERED"}
	repo, root := teamsetuptest.Checkout(t, h, ts)
	out, err := runSetup(t, repo, root, "team-1", "worker", "--platform", "codex", "--capabilities", "go, unit-tests")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"joined as worker", "announced available", "2 unacknowledged message(s)", "attempt att-1 on task task-1 (Fix the parser) is OFFERED", "team_accept or team_decline with a reason", "#1 m-0 lifecycle from hub team.assignment.offer attempt att-1 task task-1 -> OFFERED", "#2 m-1 question from coordinator: Question 1", "next cursor 2", "Status: joined"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if h.Count("/heartbeat") != 1 || h.Count("/inbox") != 1 || h.Count("/assignments/reserved") != 1 {
		t.Fatalf("requests %v", h.Requests)
	}
	var caps []string
	for _, s := range h.Sessions {
		var p struct {
			Capabilities []string                `json:"capabilities"`
			Platform     string                  `json:"platform"`
			Model        struct{ Source string } `json:"model"`
		}
		json.Unmarshal(s.Profile, &p)
		caps = p.Capabilities
		if p.Platform != "codex" || p.Model.Source != "unknown" {
			t.Fatalf("profile %s", s.Profile)
		}
	}
	if len(caps) != 2 || caps[1] != "unit-tests" {
		t.Fatalf("capabilities %v", caps)
	}
}

func TestTeamSetupResumeAndStaleHandle(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	if out, err := runSetup(t, repo, root, "Pilot", "worker"); err != nil {
		t.Fatal(err, out)
	}
	// A suspect session (no heartbeat) is the restart case: resumed automatically.
	h.Sessions["sess-1"].Suspect = true
	out, err := runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "resumed session sess-1 (no heartbeat since") || h.Resumes != 1 {
		t.Fatalf("%v resumes %d\n%s", err, h.Resumes, out)
	}
	if st := readState(t, root, repo); st.Generation != 2 || st.ResumeKey != "" {
		t.Fatalf("state after resume %+v", st)
	}
	// A live session is only verified; --resume fences it explicitly.
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "already joined as worker: session sess-1, generation 2") || h.Resumes != 1 {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err = runSetup(t, repo, root, "Pilot", "worker", "--resume")
	if err != nil || !strings.Contains(out, "resumed session sess-1 (--resume requested): generation 3") || h.Resumes != 2 {
		t.Fatalf("%v\n%s", err, out)
	}
	// Another process advanced the handle: reported, never taken over.
	h.Sessions["sess-1"].Generation = 9
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "advanced by another process") || h.Resumes != 2 || h.Count("/join") != 1 {
		t.Fatalf("%v joins %d resumes %d\n%s", err, h.Count("/join"), h.Resumes, out)
	}
	// --new-session discards the handle and joins afresh.
	out, err = runSetup(t, repo, root, "Pilot", "worker", "--new-session")
	if err != nil || !strings.Contains(out, "joined as worker: session sess-2") || h.Count("/join") != 2 {
		t.Fatalf("%v\n%s", err, out)
	}
	// A worker session that left outside this command looks like a stale
	// handle (the hub refuses both alike): explicit choice, no silent duplicate.
	h.Sessions["sess-2"].State = "left"
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "closed or was advanced by another process") || h.Count("/join") != 2 {
		t.Fatalf("%v\n%s", err, out)
	}
	// A leave through the CLI clears the handle, so the next setup joins afresh.
	canon, _ := filepath.EvalSymlinks(repo)
	teamstate.NoteLeave(canon, root, "sess-2")
	if readState(t, root, repo) != nil {
		t.Fatal("leave did not clear the saved handle")
	}
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "joined as worker: session sess-3") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTeamSetupCoordinatorRejoinsAfterLeaveButNotOverLiveSlot(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	if out, err := runSetup(t, repo, root, "Pilot", "coordinator", "--platform", "claude-code", "--platform-version", "2.1", "--model-id", "claude-fable-5-1", "--model-source", "runtime_reported"); err != nil {
		t.Fatal(err, out)
	}
	// The old coordinator session is still active but its generation moved
	// (another process resumed it): the hub refuses the fresh join.
	h.Sessions["sess-1"].Generation = 5
	out, err := runSetup(t, repo, root, "Pilot", "coordinator")
	if err == nil || !strings.Contains(out, "joining afresh") || !strings.Contains(out, "coordinator slot is occupied") || h.Resumes != 0 {
		t.Fatalf("%v\n%s", err, out)
	}
	// After that session left, the same command joins afresh, and a bare
	// re-run keeps the declaration the first run made.
	h.Sessions["sess-1"].State = "left"
	out, err = runSetup(t, repo, root, "Pilot", "coordinator")
	if err != nil || !strings.Contains(out, "joined as coordinator: session sess-2") || !strings.Contains(out, "platform claude-code 2.1; model claude-fable-5-1 (runtime_reported)") {
		t.Fatalf("%v\n%s", err, out)
	}
	if st := readState(t, root, repo); st == nil || st.SessionID != "sess-2" {
		t.Fatalf("state %+v", st)
	}
}

func TestTeamSetupRefusalsAndHandoff(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	// An older hub without the enrolled-teams listing: the join's own
	// refusals are what the command maps here.
	h.MineCode = 404
	h.JoinCode, h.JoinMsg = 403, "current team enrollment and bound ordinary write session required"
	out, err := runSetup(t, repo, root, "Pilot", "coordinator")
	if err == nil || !strings.Contains(out, "not enrolled in team \"Pilot\" as coordinator") || !strings.Contains(out, `aimem teams provision add alpha --team "Pilot" --member u-1 --role coordinator --no-token`) || !strings.Contains(out, "Status: blocked") {
		t.Fatalf("%v\n%s", err, out)
	}
	if readState(t, root, repo) != nil {
		t.Fatal("a definite refusal must not leave a pending join")
	}
	h.JoinCode, h.JoinMsg = 404, "team or project not found"
	out, _ = runSetup(t, repo, root, "Nope", "worker")
	if !strings.Contains(out, `team "Nope" not found`) || !strings.Contains(out, `aimem teams provision create alpha --team "Nope" --coordinator <coordinator-user>`) {
		t.Fatalf("%s", out)
	}
	// Occupied coordinator slot.
	h.JoinCode = 0
	h.Sessions["other"] = &teamsetuptest.Session{ID: "other", Role: "coordinator", State: "active", Generation: 1, Profile: json.RawMessage(`{"label":"c"}`)}
	out, err = runSetup(t, repo, root, "Pilot", "coordinator")
	if err == nil || !strings.Contains(out, "coordinator slot is occupied") || !strings.Contains(out, "never freed by a timeout") {
		t.Fatalf("%v\n%s", err, out)
	}
	// Missing grant: identity stops before any join, with the grant command.
	delete(h.Sessions, "other")
	h.Identity["task_write"] = false
	joins := h.Count("/join")
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "no current write grant") || !strings.Contains(out, "sets the project grant (missing now)") || h.Count("/join") != joins {
		t.Fatalf("%v\n%s", err, out)
	}
	h.Identity["task_write"] = true
	h.Identity["tasks_enabled"] = false
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "aimem tasks on -p alpha") {
		t.Fatalf("%v\n%s", err, out)
	}
	h.Identity["tasks_enabled"] = true
	h.Identity["role"] = "admin"
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "require an ordinary user token") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTeamSetupUncertainJoinReplaysWithSameKey(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	h.Garbage = 1
	repo, root := teamsetuptest.Checkout(t, h, ts)
	out, err := runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "not team protocol 1") {
		t.Fatalf("%v\n%s", err, out)
	}
	st := readState(t, root, repo)
	if st == nil || st.JoinKey == "" || st.SessionID != "" {
		t.Fatalf("pending join not recorded: %+v", st)
	}
	out, err = runSetup(t, repo, root, "Pilot", "worker", "--platform", "ignored-on-replay")
	if err != nil || !strings.Contains(out, "joined as worker: session sess-1") {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(h.JoinKeys) != 2 || h.JoinKeys[0] != h.JoinKeys[1] || len(h.Sessions) != 1 {
		t.Fatalf("replay keys %v sessions %d", h.JoinKeys, len(h.Sessions))
	}
}

func TestTeamSetupIgnoresForeignState(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	if out, err := runSetup(t, repo, root, "Pilot", "worker"); err != nil {
		t.Fatal(err, out)
	}
	// The same file under another credential is ignored, not revived.
	h.Identity["token_id"] = "t-2"
	out, err := runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "bound to another checkout, project, hub or credential") || !strings.Contains(out, "session sess-2") {
		t.Fatalf("%v\n%s", err, out)
	}
	// The credential store refuses a missing local override; nothing reaches the hub.
	files, _ := filepath.Glob(filepath.Join(root, "task-credentials", "*.json"))
	os.Remove(files[0])
	before := len(h.Requests)
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "aimem task-token set") || len(h.Requests) != before {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTeamSetupUncertainResumeReplaysBeforeAnyRead(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	if out, err := runSetup(t, repo, root, "Pilot", "worker"); err != nil {
		t.Fatal(err, out)
	}
	// The resume commits generation 2 on the hub, but its reply is unreadable.
	h.Sessions["sess-1"].Suspect = true
	h.ResumeGarbage = 1
	out, err := runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "not team protocol 1") {
		t.Fatalf("%v\n%s", err, out)
	}
	st := readState(t, root, repo)
	if st == nil || st.ResumeKey == "" || st.Generation != 1 {
		t.Fatalf("pending resume not recorded: %+v", st)
	}
	// Next run: the pending resume is replayed with its key before any roster
	// read with the stale handle; no fresh join, no second resume.
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "resumed session sess-1 (replaying an unconfirmed resume): generation 2") {
		t.Fatalf("%v\n%s", err, out)
	}
	if st := readState(t, root, repo); st.Generation != 2 || st.ResumeKey != "" {
		t.Fatalf("state after replay %+v", st)
	}
	if h.Resumes != 1 || h.Count("/join") != 1 || len(h.ResumeKeys) != 1 {
		t.Fatalf("resumes %d joins %d keys %d", h.Resumes, h.Count("/join"), len(h.ResumeKeys))
	}
}

func TestTeamSetupPendingJoinKeepsTeamAndRole(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	h.Garbage = 1
	// Enrolled in both teams, so the pending-join guard, not the enrollment
	// check, is what refuses the other team below.
	h.Mine = []map[string]any{{"id": "team-1", "name": "Pilot", "coordinator": true}, {"id": "team-9", "name": "Other", "coordinator": true}}
	repo, root := teamsetuptest.Checkout(t, h, ts)
	if out, err := runSetup(t, repo, root, "Pilot", "coordinator"); err == nil {
		t.Fatal("unreadable join reply accepted", out)
	}
	// A different role or team does not replay the key with new content and
	// does not join afresh either.
	for _, args := range [][]string{{"Pilot", "worker"}, {"Other", "coordinator"}} {
		out, err := runSetup(t, repo, root, args...)
		if err == nil || !strings.Contains(out, "has an unconfirmed coordinator join pending") || h.Count("/join") != 1 {
			t.Fatalf("%v: %v joins %d\n%s", args, err, h.Count("/join"), out)
		}
	}
	// The original request replays and finds the one session the hub created.
	out, err := runSetup(t, repo, root, "Pilot", "coordinator")
	if err != nil || !strings.Contains(out, "joined as coordinator: session sess-1") || len(h.Sessions) != 1 {
		t.Fatalf("%v sessions %d\n%s", err, len(h.Sessions), out)
	}
}

func TestTeamSetupRoleEntryRefusalsAreFailures(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	// A coordinator whose roster read fails right after the join: the
	// membership is kept and the run is not reported as complete.
	h.RosterFails = 1
	out, err := runSetup(t, repo, root, "Pilot", "coordinator")
	if err == nil || !strings.Contains(out, "roster not read after the session was recorded: HTTP 500") || !strings.Contains(out, "Status: blocked") {
		t.Fatalf("%v\n%s", err, out)
	}
	if st := readState(t, root, repo); st == nil || st.SessionID != "sess-1" {
		t.Fatalf("membership lost: %+v", st)
	}
	out, err = runSetup(t, repo, root, "Pilot", "coordinator")
	if err != nil || !strings.Contains(out, "already joined as coordinator: session sess-1") || h.Count("/join") != 1 {
		t.Fatalf("%v\n%s", err, out)
	}
	// A worker whose heartbeat is refused (enrollment revoked between join
	// and entry) is not "announced available".
	h2, ts2 := teamsetuptest.New(t)
	h2.Heartbeat = 403
	repo2, root2 := teamsetuptest.Checkout(t, h2, ts2)
	out, err = runSetup(t, repo2, root2, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "heartbeat (available) not accepted: HTTP 403") || strings.Contains(out, "announced available") {
		t.Fatalf("%v\n%s", err, out)
	}
	if st := readState(t, root2, repo2); st == nil || st.SessionID != "sess-1" {
		t.Fatalf("membership lost: %+v", st)
	}
}

func TestTeamSetupIncompleteRosterNeverJoinsAgain(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	if out, err := runSetup(t, repo, root, "Pilot", "worker"); err != nil {
		t.Fatal(err, out)
	}
	// More historical pages than the command reads: our row is never
	// reached, and that is an incomplete read, not a missing membership.
	h.RosterPages = 40
	out, err := runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "roster read incomplete") || h.Count("/join") != 1 {
		t.Fatalf("%v joins %d\n%s", err, h.Count("/join"), out)
	}
	if st := readState(t, root, repo); st == nil || st.SessionID != "sess-1" {
		t.Fatalf("membership lost: %+v", st)
	}
	// Two pages that do include our row verify normally.
	h.RosterPages = 2
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "already joined as worker: session sess-1") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTeamSetupIntegrationRepairAndNoRepair(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	// --no-repair reports the missing wiring and writes nothing.
	out, err := runSetup(t, repo, root, "Pilot", "worker", "--no-repair")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"warn  wiring docs/SESSION-STATE.md", "warn  wiring .claude/settings.json", "warn  wiring .mcp.json", "warn  wiring opencode.json"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".mcp.json")); err == nil {
		t.Fatal("--no-repair wrote .mcp.json")
	}
	// The default run repairs and says so; the next run finds everything present.
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || !strings.Contains(out, "repaired: Claude Code MCP registration added") || !strings.Contains(out, "repaired: handoff template created") || !strings.Contains(out, "repaired: command asset written (missing)") {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(repo, ".claude", "skills", "join_team", "SKILL.md")); err != nil {
		t.Fatal("setup did not write the /join_team skill")
	}
	if _, err := os.Stat(filepath.Join(repo, "opencode.json")); err != nil {
		t.Fatal("repair did not write opencode.json")
	}
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err != nil || strings.Contains(out, "repaired:") || !strings.Contains(out, "ok    wiring .codex/hooks.json Codex SessionStart handoff hook present") {
		t.Fatalf("%v\n%s", err, out)
	}
	var rep teamsetup.Report
	out, _ = runSetup(t, repo, root, "Pilot", "worker", "--json")
	if json.Unmarshal([]byte(out), &rep) != nil || rep.Wiring == nil || len(rep.Wiring.Findings) < 6 {
		t.Fatalf("json integration report: %s", out)
	}
}

func TestTeamSetupProjectStopHooksBlockBeforeAnyHubCall(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.json"), []byte(`{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "aimem submit-claude"}]}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	before := len(h.Requests)
	out, err := runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "would journal every turn twice") || !strings.Contains(out, "Status: blocked") || len(h.Requests) != before {
		t.Fatalf("%v requests %d\n%s", err, len(h.Requests)-before, out)
	}
	out, err = runSetup(t, repo, root, "Pilot", "worker", "--allow-project-stop-hooks")
	if err != nil || !strings.Contains(out, "joined as worker") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTeamSetupEnrollmentIsCheckedBeforeJoining(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	// Not enrolled anywhere: blocked with the handoff, no join sent.
	h.Mine = []map[string]any{}
	out, err := runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, "not enrolled in any team") || !strings.Contains(out, "aimem teams provision add alpha") || h.Count("/join") != 0 {
		t.Fatalf("%v joins %d\n%s", err, h.Count("/join"), out)
	}
	// Enrolled elsewhere: the enrolled teams are listed.
	h.Mine = []map[string]any{{"id": "team-9", "name": "Other", "coordinator": false, "coordinator_active": false}}
	out, err = runSetup(t, repo, root, "Pilot", "worker")
	if err == nil || !strings.Contains(out, `not enrolled in team "Pilot"; enrolled in: Other (team-9)`) || h.Count("/join") != 0 {
		t.Fatalf("%v\n%s", err, out)
	}
	// Enrolled without coordinator eligibility: coordinator blocked, worker proceeds.
	h.Mine = []map[string]any{{"id": "team-1", "name": "Pilot", "coordinator": false, "coordinator_active": true}}
	out, err = runSetup(t, repo, root, "Pilot", "coordinator")
	if err == nil || !strings.Contains(out, "not as coordinator-eligible") || !strings.Contains(out, "--role coordinator --no-token") || h.Count("/join") != 0 {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err = runSetup(t, repo, root, "team-1", "worker")
	if err != nil || !strings.Contains(out, `enrolled in team "Pilot" (team-1); a coordinator session is active now`) || !strings.Contains(out, "joined as worker") {
		t.Fatalf("%v\n%s", err, out)
	}
	// An older hub without the route (404, or the ordinary-token gate's
	// 403 as observed live on v0.7.0): reported, and the join decides.
	for _, code := range []int{404, 403} {
		h2, ts2 := teamsetuptest.New(t)
		h2.MineCode = code
		repo2, root2 := teamsetuptest.Checkout(t, h2, ts2)
		out, err = runSetup(t, repo2, root2, "Pilot", "worker")
		if err != nil || !strings.Contains(out, "does not list enrolled teams") || !strings.Contains(out, "joined as worker") {
			t.Fatalf("code %d: %v\n%s", code, err, out)
		}
	}
}

func runContinue(t *testing.T, repo, root string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runTeamContinue(args, repo, root, &out)
	if strings.Contains(out.String(), "aimem_user_") {
		t.Fatalf("token leaked into output: %s", out.String())
	}
	return out.String(), err
}

func TestTeamContinueRestoresDutiesAndNeverJoins(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	// No membership yet: continue refuses to join.
	out, err := runContinue(t, repo, root)
	if err == nil || !strings.Contains(out, "no saved membership") || h.Count("/join") != 0 {
		t.Fatalf("%v joins %d\n%s", err, h.Count("/join"), out)
	}
	if out, err := runSetup(t, repo, root, "Pilot", "worker", "--platform", "codex"); err != nil {
		t.Fatal(err, out)
	}
	// A live session is verified (no generation churn); duties come from
	// the hub: a RUNNING attempt with its title, the inbox with its cursor.
	h.Reserved = map[string]any{"id": "att-1", "task_id": "task-1", "state": "RUNNING"}
	h.InboxN = 2
	out, err = runContinue(t, repo, root)
	for _, want := range []string{"already joined as worker: session sess-1, generation 1", "attempt att-1 on task task-1 (Fix the parser) is RUNNING", "continue the accepted work in your isolated worktree", "reconcile before retrying anything", "2 unacknowledged message(s)", "next cursor 2", "Status: joined"} {
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("missing %q: %v\n%s", want, err, out)
		}
	}
	if h.Resumes != 0 || h.Count("/join") != 1 {
		t.Fatalf("continue churned: resumes %d joins %d", h.Resumes, h.Count("/join"))
	}
	// A team argument must match the saved membership.
	out, err = runContinue(t, repo, root, "Other")
	if err == nil || !strings.Contains(out, `the saved membership is in team "Pilot"`) {
		t.Fatalf("%v\n%s", err, out)
	}
	// A suspect session is resumed; --fence resumes a live one.
	h.Sessions["sess-1"].Suspect = true
	out, err = runContinue(t, repo, root, "Pilot")
	if err != nil || !strings.Contains(out, "resumed session sess-1 (no heartbeat since") || h.Resumes != 1 {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err = runContinue(t, repo, root, "--fence")
	if err != nil || !strings.Contains(out, "resumed session sess-1 (--resume requested): generation 3") || h.Resumes != 2 {
		t.Fatalf("%v\n%s", err, out)
	}
	// Each attempt state maps to its next step.
	for state, want := range map[string]string{"STOP_REQUESTED": "then team_stopped", "SUBMITTED": "wait for accept or rework", "BLOCKED": "team_resume_work", "STOPPED": "close-stop"} {
		h.Reserved["state"] = state
		out, err = runContinue(t, repo, root)
		if err != nil || !strings.Contains(out, want) {
			t.Fatalf("%s: %v\n%s", state, err, out)
		}
	}
	// The membership ended (session left): reported, never re-joined.
	h.Sessions["sess-1"].State = "left"
	out, err = runContinue(t, repo, root)
	if err == nil || !strings.Contains(out, "closed or was advanced by another process") || h.Count("/join") != 1 {
		t.Fatalf("%v joins %d\n%s", err, h.Count("/join"), out)
	}
	// Same for a coordinator whose session left: setup would re-join, continue does not.
	h2, ts2 := teamsetuptest.New(t)
	repo2, root2 := teamsetuptest.Checkout(t, h2, ts2)
	if out, err := runSetup(t, repo2, root2, "Pilot", "coordinator"); err != nil {
		t.Fatal(err, out)
	}
	h2.Sessions["sess-1"].State = "left"
	out, err = runContinue(t, repo2, root2)
	if err == nil || !strings.Contains(out, "the saved membership has ended") || h2.Count("/join") != 1 {
		t.Fatalf("%v joins %d\n%s", err, h2.Count("/join"), out)
	}
}

// gitCommit makes the checkout a Git repository (or adds a commit to it)
// and returns HEAD.
func gitCommit(t *testing.T, repo, name string) string {
	t.Helper()
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.invalid"}, {"config", "user.name", "t"}} {
			if _, err := teamsetup.GitOutput(repo, args...); err != nil {
				t.Skipf("git unavailable: %v", err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := teamsetup.GitOutput(repo, "add", name); err != nil {
		t.Fatal(err)
	}
	if _, err := teamsetup.GitOutput(repo, "-c", "commit.gpgsign=false", "commit", "-q", "-m", name); err != nil {
		t.Fatal(err)
	}
	head, err := teamsetup.GitOutput(repo, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return head
}

func TestTeamContinueComparesHeadWithThePreviouslyRecordedBase(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	a := gitCommit(t, repo, "a.txt")
	if out, err := runSetup(t, repo, root, "Pilot", "worker"); err != nil {
		t.Fatal(err, out)
	}
	if st := readState(t, root, repo); st.BaseCommit != a {
		t.Fatalf("base not recorded at join: %+v", st)
	}
	// HEAD moves on while a RUNNING attempt is held; a fenced resume must
	// compare HEAD with the base recorded before, then advance the baseline.
	b := gitCommit(t, repo, "b.txt")
	h.Reserved = map[string]any{"id": "att-1", "task_id": "task-1", "state": "RUNNING"}
	out, err := runContinue(t, repo, root, "--fence")
	if err != nil || !strings.Contains(out, "resumed session sess-1") || !strings.Contains(out, "HEAD "+b[:12]+" differs from the base recorded at the last verification, "+a[:12]) {
		t.Fatalf("%v\n%s", err, out)
	}
	if st := readState(t, root, repo); st.BaseCommit != b || st.Generation != 2 {
		t.Fatalf("baseline not advanced: %+v", st)
	}
	// A verify-only run now finds HEAD equal to the recorded base.
	out, err = runContinue(t, repo, root)
	if err != nil || !strings.Contains(out, "HEAD is still the base recorded at the last verification, "+b[:12]) {
		t.Fatalf("%v\n%s", err, out)
	}
	// A membership recorded without a base (older release) says so.
	st := readState(t, root, repo)
	st.BaseCommit = ""
	canon, _ := filepath.EvalSymlinks(repo)
	if err := teamstate.Save(teamstate.Path(root, canon), st); err != nil {
		t.Fatal(err)
	}
	out, err = runContinue(t, repo, root)
	if err != nil || !strings.Contains(out, "no base commit was recorded at the last verification") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestTeamSetupArgs(t *testing.T) {
	for _, args := range [][]string{nil, {"Pilot"}, {"Pilot", "reviewer"}, {"--json", "Pilot", "worker"}, {"Pilot", "worker", "extra"}, {"Pilot", "worker", "--model-source", "guess"}, {"Pilot", "worker", "--model-id", "x"}, {"Pilot", "worker", "--model-source", "agent_reported"}, {"Pilot", "worker", "--resume", "--new-session"}} {
		if _, err := parseTeamSetupArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	o, err := parseTeamSetupArgs([]string{"My Team", "coordinator", "--label", "coord", "--model-id", "m", "--model-source", "operator_configured"})
	if err != nil || o.Team != "My Team" || o.Profile.Label != "coord" || o.Profile.Model.ObservedAt == "" || o.Profile.Platform != "unknown" {
		t.Fatalf("%v %+v", err, o)
	}
}

// The CLI report shows the readiness parts and prints the delivered
// guidance and process after the status, each ending with its terminator.
func TestTeamSetupPrintsReadinessAndDeliveries(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	out, err := runSetup(t, repo, root, "Pilot", "worker", "--platform", "codex")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Readiness: ready_for_work true", "role_context     delivered", "project_process  ready version " + teamsetuptest.Selection.Commit, "execution        not_verified", "=== end aimem-team-guidance role worker version dev", "Work only on READY tasks", "=== end aimem process context unit project alpha"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if strings.Index(out, "Status: joined") > strings.Index(out, "=== end aimem-team-guidance") {
		t.Fatal("the deliveries must follow the report")
	}
}
