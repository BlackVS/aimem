package mcp

// The tool sets per mode (toolsets.go, docs/DESIGN-tool-sets.md §4-5): the
// first join and the resume through the entry points' tools, the
// announcement of the changed list, and the notices on a call by name.

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"aimem/internal/ident"
	"aimem/internal/store"
	"aimem/internal/teamsetup/teamsetuptest"
	"aimem/internal/teamstate"
)

// joinedCheckout is a checkout holding a team record, as a join before this
// change left it: the record alone, no session verified in this process.
func joinedCheckout(t *testing.T) (dir, root string) {
	t.Helper()
	dir, root = t.TempDir(), t.TempDir()
	repo, err := teamstate.Canonical(dir)
	if err != nil {
		t.Fatal(err)
	}
	project, err := ident.ProjectID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := teamstate.Save(teamstate.Path(root, repo), &teamstate.State{Version: 1, Repo: repo, Project: project, Team: "Pilot", Role: "worker", SessionID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	return dir, root
}

func toolSet(t *testing.T, s *srv) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, raw := range listedTools(t, s) {
		var d struct {
			Name string `json:"name"`
		}
		json.Unmarshal(raw, &d)
		out[d.Name] = true
	}
	return out
}

// teamWorkToolNames are the 26 tools of the teams set.
func teamWorkToolNames() []string {
	var out []string
	for _, d := range taskToolDefs {
		if n := d["name"].(string); isTeamWorkTool(n) {
			out = append(out, n)
		}
	}
	return out
}

// checkStandalone fails unless set is a standalone list: the four entry
// tools, no team work tool and no reservation tool.
func checkStandalone(t *testing.T, label string, set map[string]bool) {
	t.Helper()
	for n := range teamEntryTools {
		if !set[n] {
			t.Errorf("%s: entry tool %s not listed", label, n)
		}
	}
	for n := range set {
		if isTeamWorkTool(n) || reservationTools[n] {
			t.Errorf("%s: %s listed", label, n)
		}
	}
}

// A wired checkout that never joined lists the standalone set; team_setup
// through the stdio loop joins, says the tools arrive, announces the changed
// list after its answer, and the next list holds the teams set. A second
// call announces nothing: the setting was already on.
func TestFirstJoinListsTheTeamsSetAndAnnouncesIt(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	s := stdioFor(repo, root)
	checkStandalone(t, "before the join", toolSet(t, s))

	setup, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{
		"name": "team_setup", "arguments": map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile}}})
	in := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" + string(setup) + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}` + "\n" + strings.Replace(string(setup), `"id":2`, `"id":4`, 1) + "\n"
	var out bytes.Buffer
	if err := s.serve(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("%d lines, want initialize, setup, the announcement, the list and the second setup:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"tools":{"listChanged":true}`) {
		t.Fatalf("initialize does not declare listChanged: %s", lines[0])
	}
	report := func(line string) []string {
		var resp struct {
			Result map[string]any `json:"result"`
		}
		json.Unmarshal([]byte(line), &resp)
		text, isErr := toolText(resp.Result)
		var rep struct {
			Status string   `json:"status"`
			Next   []string `json:"next"`
		}
		if isErr || json.Unmarshal([]byte(text), &rep) != nil || rep.Status != "joined" {
			t.Fatalf("team_setup: %v %.400s", isErr, text)
		}
		return rep.Next
	}
	if next := report(lines[1]); !strings.Contains(strings.Join(next, "\n"), teamsArrivedNotice) {
		t.Fatalf("the join's next steps do not say the tools arrive: %v", next)
	}
	if lines[2] != `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}` {
		t.Fatalf("no announcement after the join's answer: %s", lines[2])
	}
	var list struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	json.Unmarshal([]byte(lines[3]), &list)
	after := map[string]bool{}
	for _, tl := range list.Result.Tools {
		after[tl.Name] = true
	}
	for _, n := range teamWorkToolNames() {
		if !after[n] {
			t.Errorf("after the join: %s not listed", n)
		}
	}
	if next := report(lines[4]); strings.Contains(strings.Join(next, "\n"), teamsArrivedNotice) {
		t.Fatal("a second team_setup said the tools arrive again")
	}
}

// A checkout that joined before this change lists the teams set at start:
// /resume_team needs no restart, and team_continue announces nothing.
func TestResumeInACheckoutThatJoinedBefore(t *testing.T) {
	dir, root := joinedCheckout(t)
	s := &srv{project: "alpha", local: &localCheckout{dir: dir, root: root}}
	set := toolSet(t, s)
	for _, n := range append(teamWorkToolNames(), "team_continue", "team_setup") {
		if !set[n] {
			t.Errorf("at start: %s not listed", n)
		}
	}
	if got := TeamsSetting(dir, root); !strings.HasPrefix(got, "on: team Pilot, role worker, record ") {
		t.Fatalf("teams setting: %s", got)
	}
	// Leaving clears the record: the setting is off and the list standalone.
	teamstate.NoteLeave(dir, root, "sess-1")
	checkStandalone(t, "after leaving", toolSet(t, s))
	if got := TeamsSetting(dir, root); !strings.HasPrefix(got, "off: ") {
		t.Fatalf("teams setting after leaving: %s", got)
	}
}

// A call by name to a tool outside the session's set is served as before,
// with its notice as a block of its own: the team work tools while the
// setting is off, the reservation tools wherever they are unlisted. A tool
// in the set carries none, and a team member's session none at all.
func TestCallByNameOutsideTheSetCarriesTheNotice(t *testing.T) {
	notice := func(s *srv, name string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": map[string]any{}}})
		var resp struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		json.Unmarshal(s.handle(t.Context(), raw), &resp)
		if c := resp.Result.Content; len(c) > 1 && strings.HasPrefix(c[len(c)-1].Text, "notice: ") {
			return c[len(c)-1].Text
		}
		return ""
	}
	never := &srv{project: "alpha", local: &localCheckout{dir: t.TempDir(), root: t.TempDir()}}
	if n := notice(never, "team_inbox"); !strings.Contains(n, "has not joined a team") || !strings.Contains(n, servedByNamePeriod) {
		t.Fatalf("team_inbox by name, never joined: %q", n)
	}
	for _, name := range []string{"task_reservation_claim", "task_reservation_release"} {
		if n := notice(never, name); n != never.reservationNotice() || !strings.Contains(n, "`aimem reservation release`") {
			t.Fatalf("%s by name, standalone: %q", name, n)
		}
	}
	if n := notice(never, "team_list"); n != "" {
		t.Fatalf("an entry tool carries a notice: %q", n)
	}
	dir, root := joinedCheckout(t)
	joined := &srv{project: "alpha", local: &localCheckout{dir: dir, root: root}}
	if n := notice(joined, "team_inbox"); n != "" {
		t.Fatalf("team_inbox by name, joined: %q", n)
	}
	hub := &srv{tasksOnly: true}
	if n := notice(hub, "team_inbox"); !strings.Contains(n, "?tools=teams") {
		t.Fatalf("team_inbox on the hub without the parameter: %q", n)
	}
	if n := notice(&srv{tasksOnly: true, teamsByURL: true}, "team_inbox"); n != "" {
		t.Fatalf("team_inbox on the hub with the parameter: %q", n)
	}
	if n := notice(hub, "task_reservation_status"); n != "" {
		t.Fatalf("a holder's tool on tasks-only carries a notice: %q", n)
	}
	// The hub's endpoint names the holder's three listed tools, never the
	// command a client without a checkout cannot run.
	n := notice(hub, "task_reservation_claim")
	for _, want := range []string{"task_reservation_status", "task_reservation_release", "task_reservation_finalize"} {
		if !strings.Contains(n, want) {
			t.Fatalf("task_reservation_claim on tasks-only does not name %s: %q", want, n)
		}
	}
	if strings.Contains(n, "aimem reservation") || strings.Contains(n, "in the checkout") {
		t.Fatalf("task_reservation_claim on tasks-only names the command: %q", n)
	}
	if n := (&srv{team: &teamMode{}}).noticeFor("task_reservation_claim"); n != "" {
		t.Fatalf("a member's reservation call carries a notice: %q", n)
	}
}

// The hint completes only the reserved-task refusal, as each facade offers
// the way out; a member's session and the reservation tools' own envelopes
// are left as the hub wrote them.
func TestReservedHintPerFacade(t *testing.T) {
	refusal := &apiError{msg: store.ErrTaskReserved.Error()}
	if got := (&srv{}).reservedHint("update_task", refusal).Error(); !strings.Contains(got, "`aimem reservation release`") {
		t.Fatalf("stdio: %s", got)
	}
	if got := (&srv{tasksOnly: true}).reservedHint("update_task", refusal).Error(); !strings.Contains(got, "task_reservation_release or task_reservation_finalize") {
		t.Fatalf("tasks-only: %s", got)
	}
	for _, s := range []*srv{{team: &teamMode{}}, {}} {
		name := "update_task"
		if s.team == nil {
			name = "task_reservation_update"
		}
		if got := s.reservedHint(name, refusal); got != error(refusal) {
			t.Fatalf("%s changed: %v", name, got)
		}
	}
	if got := (&srv{}).reservedHint("team_edit", refusal); got != error(refusal) {
		t.Fatalf("a team tool's refusal changed: %v", got)
	}
	other := &apiError{msg: "revision conflict"}
	if got := (&srv{}).reservedHint("update_task", other); got != error(other) {
		t.Fatalf("another refusal changed: %v", got)
	}
}

// serveLines runs the stdio loop over the messages and returns its output
// lines: each reply, and any announcement after it.
func serveLines(t *testing.T, s *srv, msgs ...map[string]any) []string {
	t.Helper()
	var in strings.Builder
	for i, m := range msgs {
		m["jsonrpc"], m["id"] = "2.0", i+1
		raw, _ := json.Marshal(m)
		in.Write(append(raw, '\n'))
	}
	var out bytes.Buffer
	if err := s.serve(strings.NewReader(in.String()), &out); err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(out.String()), "\n")
}

func setupCall() map[string]any {
	return map[string]any{"method": "tools/call", "params": map[string]any{"name": "team_setup",
		"arguments": map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile}}}
}

// setupReport decodes team_setup's answer line into its report text.
func setupReport(t *testing.T, line string) string {
	t.Helper()
	var resp struct {
		Result map[string]any `json:"result"`
	}
	json.Unmarshal([]byte(line), &resp)
	text, isErr := toolText(resp.Result)
	if isErr {
		t.Fatalf("team_setup failed as a tool: %s", text)
	}
	return text
}

const listChangedLine = `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`

// A join the hub never received leaves the record with its retry key and no
// session id: the setting stays off, the report does not say the checkout
// joined, nothing is announced and the list stays standalone. The retry
// that succeeds turns it on, with the step and the announcement once.
func TestUnconfirmedJoinLeavesTheSettingOff(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	s := stdioFor(repo, root)
	h.JoinDrop = 2 // net/http replays a keyed POST once on a dropped connection
	lines := serveLines(t, s, setupCall(), map[string]any{"method": "tools/list"})
	if len(lines) != 2 {
		t.Fatalf("%d lines, want the answer and the list, no announcement:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	report := setupReport(t, lines[0])
	if !strings.Contains(report, "join not confirmed") || strings.Contains(report, teamsArrivedNotice) {
		t.Fatalf("the unconfirmed join's report: %.600s", report)
	}
	if got := TeamsSetting(repo, root); !strings.HasPrefix(got, "off: ") {
		t.Fatalf("teams setting after an unconfirmed join: %s", got)
	}
	checkStandalone(t, "after an unconfirmed join", toolSet(t, s))

	lines = serveLines(t, s, setupCall(), setupCall())
	if len(lines) != 3 || lines[1] != listChangedLine {
		t.Fatalf("the retry: want the answer, one announcement, the second answer:\n%s", strings.Join(lines, "\n"))
	}
	if r := setupReport(t, lines[0]); !strings.Contains(r, `"status": "joined"`) || !strings.Contains(r, teamsArrivedNotice) {
		t.Fatalf("the retry's report: %.600s", r)
	}
	if r := setupReport(t, lines[2]); strings.Contains(r, teamsArrivedNotice) {
		t.Fatal("a second team_setup said the tools arrive again")
	}
}

// A checkout that is on, whose membership ended on the hub, still lists the
// teams set. A new join (new_session) that is not confirmed replaces its record with a
// pending one: the setting goes off within the call, the answer says the
// work tools are withdrawn, and the server announces the changed list, so
// the client's list and the served list do not part.
func TestUnconfirmedRejoinTurnsTheSettingOffAndAnnouncesIt(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	s := stdioFor(repo, root)
	if lines := serveLines(t, s, setupCall()); len(lines) != 2 || lines[1] != listChangedLine {
		t.Fatalf("the first join: %s", strings.Join(lines, "\n"))
	}
	for _, sess := range h.Sessions {
		sess.State = "left" // the membership ended on the hub
	}
	set := toolSet(t, s)
	for _, n := range teamWorkToolNames() {
		if !set[n] {
			t.Fatalf("an ended membership's checkout no longer lists %s", n)
		}
	}
	// The way back in after an ended membership: a join with new_session,
	// which drops the saved handle and saves a pending record.
	rejoin := setupCall()
	rejoin["params"].(map[string]any)["arguments"].(map[string]any)["new_session"] = true
	h.JoinDrop = 2
	lines := serveLines(t, s, rejoin, map[string]any{"method": "tools/list"})
	if len(lines) != 3 || lines[1] != listChangedLine {
		t.Fatalf("want the answer, the announcement, the list:\n%s", strings.Join(lines, "\n"))
	}
	if r := setupReport(t, lines[0]); !strings.Contains(r, "join not confirmed") || !strings.Contains(r, teamsWithdrawnNotice) || strings.Contains(r, teamsArrivedNotice) {
		t.Fatalf("the unconfirmed rejoin's report: %.600s", r)
	}
	if strings.Contains(lines[2], `"team_inbox"`) || !strings.Contains(lines[2], `"team_setup"`) {
		t.Fatalf("the list after the unconfirmed rejoin is not standalone: %.300s", lines[2])
	}
}

// The setting's report is written from one read: a pending join's record,
// an unreadable record and no record all read as off, never a failure.
func TestTeamsSettingFromOneRead(t *testing.T) {
	dir, root := t.TempDir(), t.TempDir()
	if got := TeamsSetting(dir, root); !strings.HasPrefix(got, "off: this checkout holds no record") {
		t.Fatalf("no record: %s", got)
	}
	repo, _ := teamstate.Canonical(dir)
	project, _ := ident.ProjectID(dir)
	path := teamstate.Path(root, repo)
	if err := teamstate.Save(path, &teamstate.State{Version: 1, Repo: repo, Project: project, Team: "Pilot", Role: "worker", JoinKey: "k"}); err != nil {
		t.Fatal(err)
	}
	if got := TeamsSetting(dir, root); !strings.HasPrefix(got, "off: this checkout holds no record") {
		t.Fatalf("a pending join: %s", got)
	}
	if err := os.WriteFile(path, []byte("not a record"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := TeamsSetting(dir, root); !strings.HasPrefix(got, "off: this checkout's team record cannot be read") {
		t.Fatalf("an unreadable record: %s", got)
	}
}
