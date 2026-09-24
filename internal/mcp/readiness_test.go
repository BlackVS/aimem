package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/process"
	"aimem/internal/teamguide"
	"aimem/internal/teamsetup/teamsetuptest"
	"aimem/internal/teamstate"
)

// readinessReport is the part of the onboarding report these tests read.
type readinessReport struct {
	Status    string   `json:"status"`
	Next      []string `json:"next"`
	Delivered any      `json:"delivered"`
	Checks    []struct {
		Name, Level, Detail string
	} `json:"checks"`
	Readiness struct {
		ReadyForWork   bool   `json:"ready_for_work"`
		Membership     part   `json:"membership"`
		RoleContext    part   `json:"role_context"`
		ProjectProcess part   `json:"project_process"`
		Execution      part   `json:"execution"`
		Acknowledged   string `json:"acknowledged"`
	} `json:"readiness"`
}

type part struct {
	State, Detail, Fix, Version, Digest string
}

func (r readinessReport) check(name string) (string, string) {
	for _, c := range r.Checks {
		if c.Name == name {
			return c.Level, c.Detail
		}
	}
	return "", ""
}

// onboardBlocks calls an onboarding tool through the JSON-RPC handler and
// returns the decoded report (block 0) and every further text block.
func onboardBlocks(t *testing.T, s *srv, name string, args map[string]any) (readinessReport, []string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(s.handle(context.Background(), raw), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result.IsError || len(resp.Result.Content) == 0 {
		t.Fatalf("%s: %+v", name, resp.Result)
	}
	var rep readinessReport
	if err := json.Unmarshal([]byte(resp.Result.Content[0].Text), &rep); err != nil {
		t.Fatalf("%s: block 0 is not the report: %v", name, err)
	}
	var blocks []string
	for _, c := range resp.Result.Content[1:] {
		blocks = append(blocks, c.Text)
	}
	return rep, blocks
}

func lastLineOf(s string) string {
	s = strings.TrimRight(s, "\n")
	return s[strings.LastIndexByte(s, '\n')+1:]
}

// Setup and continue deliver the complete role guidance and the selected
// process as their own blocks, and a worker that has both is announced
// available; nothing in this proves the agent's own execution.
func TestSetupDeliversContextAndAWorkerWithItIsReady(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	s := stdioFor(repo, root)
	u, _ := teamguide.Embedded()

	rep, blocks := onboardBlocks(t, s, "team_setup", map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile})
	rd := rep.Readiness
	if rep.Status != "joined" || !rd.ReadyForWork || rd.Membership.State != "active" || rd.RoleContext.State != "delivered" || rd.RoleContext.Digest != u.Digest ||
		rd.ProjectProcess.State != "ready" || rd.ProjectProcess.Version != teamsetuptest.Selection.Commit || rd.Execution.State != "not_verified" {
		t.Fatalf("readiness: %+v", rd)
	}
	if !strings.HasPrefix(rd.Acknowledged, "not recorded") || rep.Delivered != nil {
		t.Fatalf("acknowledgement %q; delivered repeated in the JSON: %v", rd.Acknowledged, rep.Delivered)
	}
	if len(blocks) != 2 || lastLineOf(blocks[0]) != u.Terminator("role worker", "dev") ||
		!strings.Contains(blocks[1], "Work only on READY tasks") || !strings.HasPrefix(lastLineOf(blocks[1]), "=== end aimem process context unit project alpha commit "+teamsetuptest.Selection.Commit) {
		t.Fatalf("blocks: %d\n%.300s", len(blocks), strings.Join(blocks, "\n----\n"))
	}
	if strings.Contains(blocks[0], "coordinator playbook") || !strings.Contains(blocks[0], "Worker playbook") {
		t.Fatal("the worker received another role's set")
	}
	if got := h.HeartbeatAvailability; len(got) != 1 || got[0] != "available" {
		t.Fatalf("heartbeats: %v", got)
	}
	if !strings.HasPrefix(rep.Next[0], "ready_for_work:") || !strings.Contains(strings.Join(rep.Next, "\n"), "execution is not verified here") {
		t.Fatalf("next: %q", rep.Next)
	}

	// Continue delivers the same content for the saved role and joins nothing.
	rep, blocks = onboardBlocks(t, s, "team_continue", map[string]any{})
	if rep.Status != "joined" || !rep.Readiness.ReadyForWork || len(blocks) != 2 || h.Count("/join") != 1 {
		t.Fatalf("continue: %+v blocks %d joins %d", rep.Readiness, len(blocks), h.Count("/join"))
	}
}

// A coordinator receives the coordinator set, sends no heartbeat, and is
// told to offer nothing while not ready.
func TestCoordinatorReadinessAndNoOffersWhileNotReady(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	s := stdioFor(repo, root)
	rep, blocks := onboardBlocks(t, s, "team_setup", map[string]any{"team": "Pilot", "role": "coordinator", "profile": map[string]any{"label": "c", "platform": "codex", "platform_version": "1"}})
	if !rep.Readiness.ReadyForWork || len(blocks) != 2 || !strings.Contains(blocks[0], "Coordinator playbook") || strings.Contains(blocks[0], "## Worker playbook") {
		t.Fatalf("ready coordinator: %+v", rep.Readiness)
	}
	if len(h.HeartbeatAvailability) != 0 {
		t.Fatalf("a coordinator sent heartbeats: %v", h.HeartbeatAvailability)
	}

	h2, ts2 := teamsetuptest.New(t)
	h2.Selection = nil
	repo2, root2 := teamsetuptest.Checkout(t, h2, ts2)
	rep, blocks = onboardBlocks(t, stdioFor(repo2, root2), "team_setup", map[string]any{"team": "Pilot", "role": "coordinator", "profile": map[string]any{"label": "c", "platform": "codex", "platform_version": "1"}})
	if rep.Status != "joined" || rep.Readiness.ReadyForWork || rep.Readiness.ProjectProcess.State != "not_selected" || len(blocks) != 1 ||
		!strings.Contains(rep.Next[0], "issue no offers until team_continue reports ready_for_work") || !strings.Contains(rep.Next[0], "aimem process select") {
		t.Fatalf("unready coordinator: %+v\n%q", rep.Readiness, rep.Next)
	}
	noUnqualifiedStep(t, rep.Next, "offer one attempt per task")
}

// A worker without the project process stays a member, is announced
// unavailable on every run, and is never joined twice; once the process is
// there, continue announces it available without a join.
func TestUnreadyWorkerIsUnavailableAndNeverJoinsTwice(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	h.Selection = nil
	repo, root := teamsetuptest.Checkout(t, h, ts)
	s := stdioFor(repo, root)
	args := map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile}

	for range 2 {
		rep, blocks := onboardBlocks(t, s, "team_setup", args)
		rd := rep.Readiness
		if rep.Status != "joined" || rd.ReadyForWork || rd.Membership.State != "active" || rd.ProjectProcess.State != "not_selected" || rd.RoleContext.State != "delivered" || len(blocks) != 1 {
			t.Fatalf("unready worker: %+v", rd)
		}
		if lvl, detail := rep.check("availability"); lvl != "warn" || !strings.Contains(detail, "announced unavailable") {
			t.Fatalf("availability: %s %s", lvl, detail)
		}
		if !strings.Contains(rep.Next[0], "Accept nothing") || !strings.Contains(rep.Next[0], "keep every heartbeat unavailable") {
			t.Fatalf("next: %q", rep.Next)
		}
		noUnqualifiedStep(t, rep.Next, "accept or decline an offer")
	}
	if h.Count("/join") != 1 || strings.Join(h.HeartbeatAvailability, ",") != "unavailable,unavailable" {
		t.Fatalf("joins %d, heartbeats %v", h.Count("/join"), h.HeartbeatAvailability)
	}

	// A restart: the session is suspect, continue resumes it (new
	// generation, no join) and, with the process now selected, announces
	// available.
	for _, sess := range h.Sessions {
		sess.Suspect = true
	}
	sel := teamsetuptest.Selection
	h.Selection = &sel
	rep, blocks := onboardBlocks(t, s, "team_continue", map[string]any{})
	if rep.Status != "joined" || !rep.Readiness.ReadyForWork || len(blocks) != 2 || h.Resumes != 1 || h.Count("/join") != 1 {
		t.Fatalf("continue: %+v resumes %d joins %d", rep.Readiness, h.Resumes, h.Count("/join"))
	}
	if got := h.HeartbeatAvailability; got[len(got)-1] != "available" {
		t.Fatalf("heartbeats: %v", got)
	}
}

// A heartbeat the hub refuses is a reported failure: the membership is
// kept, nothing is left or released, and nothing claims availability.
func TestHeartbeatFailureKeepsTheMembership(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	h.Selection = nil
	h.Heartbeat = 403
	repo, root := teamsetuptest.Checkout(t, h, ts)
	rep, _ := onboardBlocks(t, stdioFor(repo, root), "team_setup", map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile})
	if lvl, detail := rep.check("availability"); rep.Status != "blocked" || lvl != "fail" || !strings.Contains(detail, "heartbeat (unavailable) not accepted: HTTP 403") {
		t.Fatalf("status %s, availability %s %s", rep.Status, lvl, detail)
	}
	if rep.Readiness.Membership.State != "active" || rep.Readiness.ReadyForWork {
		t.Fatalf("readiness: %+v", rep.Readiness)
	}
	if !strings.Contains(strings.Join(rep.Next, "\n"), "the unavailable announcement was not accepted: the hub may still offer you work") {
		t.Fatalf("next: %q", rep.Next)
	}
	canon, _ := filepath.EvalSymlinks(repo)
	if st, _ := teamstate.Load(teamstate.Path(root, canon)); st == nil || st.SessionID != "sess-1" || h.Count("/leave") != 0 {
		t.Fatalf("membership not kept: %+v, leaves %d", st, h.Count("/leave"))
	}
}

// Old hubs: one without the process route leaves the member joined but not
// ready; one older than the team protocol blocks before any membership,
// and nothing is evaluated or delivered.
func TestOldHubs(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	h.ProcessCode = 404
	repo, root := teamsetuptest.Checkout(t, h, ts)
	rep, blocks := onboardBlocks(t, stdioFor(repo, root), "team_setup", map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile})
	if rep.Status != "joined" || rep.Readiness.ReadyForWork || rep.Readiness.ProjectProcess.State != "unavailable" || len(blocks) != 1 || h.HeartbeatAvailability[0] != "unavailable" {
		t.Fatalf("no process route: %+v", rep.Readiness)
	}

	h2, ts2 := teamsetuptest.New(t)
	h2.Version = "v0.6.0"
	repo2, root2 := teamsetuptest.Checkout(t, h2, ts2)
	rep, blocks = onboardBlocks(t, stdioFor(repo2, root2), "team_setup", map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile})
	rd := rep.Readiness
	if rep.Status != "blocked" || rd.ReadyForWork || rd.Membership.State != "not_verified" || rd.RoleContext.State != "not_evaluated" || rd.ProjectProcess.State != "not_evaluated" || len(blocks) != 0 || h2.Count("/join") != 0 {
		t.Fatalf("old hub: %+v blocks %d", rd, len(blocks))
	}
}

// The process is served from the exact cache only when the commit is
// there; without it (and Git unreachable) the member is not ready.
func TestProcessCacheBoundary(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	if err := os.RemoveAll(process.CacheDir(root, teamsetuptest.Selection)); err != nil {
		t.Fatal(err)
	}
	rep, blocks := onboardBlocks(t, stdioFor(repo, root), "team_setup", map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile})
	if rep.Readiness.ReadyForWork || rep.Readiness.ProjectProcess.State != "unavailable" || len(blocks) != 1 || h.HeartbeatAvailability[0] != "unavailable" {
		t.Fatalf("cache miss: %+v", rep.Readiness)
	}
}

// A continue without a saved membership evaluates and delivers nothing.
func TestContinueWithoutMembershipDeliversNothing(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	rep, blocks := onboardBlocks(t, stdioFor(repo, root), "team_continue", map[string]any{})
	if rep.Status != "blocked" || rep.Readiness.Membership.State != "not_verified" || rep.Readiness.RoleContext.State != "not_evaluated" || len(blocks) != 0 {
		t.Fatalf("%+v blocks %d", rep.Readiness, len(blocks))
	}
}

// noUnqualifiedStep fails when a step that starts work is not qualified by
// readiness: the report must not contradict a member that is not ready.
func noUnqualifiedStep(t *testing.T, next []string, step string) {
	t.Helper()
	for _, line := range next {
		if strings.Contains(line, step) && !strings.HasPrefix(line, "only once team_continue reports ready_for_work: ") {
			t.Fatalf("unqualified work step for a member that is not ready: %q", line)
		}
	}
}

// A failure after the context was evaluated (here, the inbox read) still
// carries the readiness steps: a worker that is not ready is told to
// accept nothing even when the report ends blocked.
func TestReadinessStepsSurviveALaterFailure(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	h.Selection = nil
	h.InboxCode = 500
	repo, root := teamsetuptest.Checkout(t, h, ts)
	rep, _ := onboardBlocks(t, stdioFor(repo, root), "team_setup", map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile})
	if rep.Status != "blocked" || rep.Readiness.ReadyForWork || !strings.Contains(strings.Join(rep.Next, "\n"), "Accept nothing") {
		t.Fatalf("%s %+v\n%q", rep.Status, rep.Readiness, rep.Next)
	}
}
