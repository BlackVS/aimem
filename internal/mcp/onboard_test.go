package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aimem/internal/taskcred"
	"aimem/internal/teamsetup"
	"aimem/internal/teamsetup/teamsetuptest"
	"aimem/internal/teamstate"
)

// stdioFor is the stdio facade bound to the checkout at repo: the same
// construction Serve uses, with the checkout and state root of the test.
func stdioFor(repo, root string) *srv {
	return &srv{project: "alpha", taskSetup: func() (TaskCallFunc, error) { return taskCallerIn(repo, root) }, local: &localCheckout{dir: repo, root: root}}
}

type onboardReport struct {
	Status   string `json:"status"`
	Team     string `json:"team"`
	Role     string `json:"role"`
	Checkout string `json:"checkout"`
	RunAs    string `json:"run_as"`
	Checks   []struct {
		Name, Level, Detail, Fix string
	} `json:"checks"`
	Session *struct {
		ID         string `json:"id"`
		TeamID     string `json:"team_id"`
		Generation int64  `json:"generation"`
		Role       string `json:"role"`
	} `json:"session"`
	StateFile string `json:"state_file"`
}

// onboard calls one onboarding tool and decodes the report; a tool-level
// error fails the test unless wantErr.
func onboard(t *testing.T, s *srv, h *teamsetuptest.Hub, name string, args map[string]any) onboardReport {
	t.Helper()
	text, isErr := callTool(t, s, name, args)
	if isErr {
		t.Fatalf("%s: %s", name, text)
	}
	if strings.Contains(text, h.Secret) {
		t.Fatalf("token in the %s report", name)
	}
	var rep onboardReport
	if err := json.Unmarshal([]byte(text), &rep); err != nil {
		t.Fatalf("%s: not a report: %v\n%s", name, err, text)
	}
	return rep
}

func (r onboardReport) check(name string) (level, detail, fix string) {
	for _, c := range r.Checks {
		if c.Name == name {
			level, detail, fix = c.Level, c.Detail, c.Fix
		}
	}
	return
}

var workerProfile = map[string]any{"label": "w", "platform": "codex", "platform_version": "0.42"}

func TestOnboardingToolsJoinVerifyResumeAndContinue(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	s := stdioFor(repo, root)

	// Listed on the stdio facade, both of them.
	var listed struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	json.Unmarshal(s.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)), &listed)
	names := map[string]bool{}
	for _, d := range listed.Result.Tools {
		names[d["name"].(string)] = true
	}
	if !names["team_setup"] || !names["team_continue"] || !names["team_list"] {
		t.Fatalf("stdio tools: %v", names)
	}

	// Join: the session, the state file, the role entry, no token anywhere.
	rep := onboard(t, s, h, "team_setup", map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile})
	if rep.Status != "joined" || rep.Session == nil || rep.Session.Role != "worker" || rep.Session.Generation != 1 {
		t.Fatalf("%+v", rep)
	}
	if rep.RunAs == "" || rep.Checkout == "" {
		t.Fatalf("run_as/checkout missing: %+v", rep)
	}
	if lvl, detail, _ := rep.check("base commit"); lvl != "warn" || !strings.Contains(detail, "not a Git checkout") {
		t.Fatalf("base commit in a plain directory: %s %s", lvl, detail)
	}
	if lvl, _, _ := rep.check("availability"); lvl != "ok" {
		t.Fatalf("worker did not announce availability: %+v", rep.Checks)
	}
	if h.Count("/join") != 1 || h.Count("/heartbeat") != 1 || h.Count("/inbox") != 1 {
		t.Fatalf("hub calls: %v", h.Requests)
	}
	canon, _ := filepath.EvalSymlinks(repo)
	st, err := teamstate.Load(teamstate.Path(root, canon))
	if err != nil || st == nil || st.SessionID != rep.Session.ID || st.JoinKey != "" || st.Profile.Platform != "codex" {
		t.Fatalf("state: %+v %v", st, err)
	}
	if raw, _ := os.ReadFile(teamstate.Path(root, canon)); strings.Contains(string(raw), h.Secret) {
		t.Fatal("token in the state file")
	}

	// Repeat: verified, not joined again; the saved declaration is reused
	// when the call carries no profile.
	rep = onboard(t, s, h, "team_setup", map[string]any{"team": "Pilot", "role": "worker"})
	if rep.Status != "joined" || h.Count("/join") != 1 {
		t.Fatalf("second setup joined again: %+v %v", rep, h.Requests)
	}
	if _, detail, _ := rep.check("session"); !strings.Contains(detail, "already joined") {
		t.Fatalf("%s", detail)
	}
	if _, detail, _ := rep.check("profile"); strings.Contains(detail, "platform not given") {
		t.Fatal("saved platform not reused")
	}

	// A different role or team on the same checkout is refused, never a
	// silent second membership: the CLI and the MCP share one saved record.
	rep = onboard(t, s, h, "team_setup", map[string]any{"team": "Pilot", "role": "coordinator"})
	if rep.Status != "blocked" || h.Count("/join") != 1 {
		t.Fatalf("role switched in place: %+v", rep)
	}
	if _, detail, fix := rep.check("session"); !strings.Contains(detail, "cannot change in place") || !strings.Contains(fix, "new_session") && !strings.Contains(fix, "new-session") {
		t.Fatalf("%s / %s", detail, fix)
	}

	// Continue after a restart: the same session, no join; a suspect
	// session is resumed with a new generation.
	rep = onboard(t, s, h, "team_continue", map[string]any{})
	if rep.Status != "joined" || rep.Role != "worker" || rep.Team != "Pilot" || rep.Session.Generation != 1 || h.Count("/join") != 1 || h.Resumes != 0 {
		t.Fatalf("continue: %+v", rep)
	}
	h.Sessions[rep.Session.ID].Suspect = true
	rep = onboard(t, s, h, "team_continue", map[string]any{"team": "Pilot"})
	if rep.Status != "joined" || rep.Session.Generation != 2 || h.Resumes != 1 {
		t.Fatalf("resume: %+v resumes %d", rep, h.Resumes)
	}
	if _, detail, _ := rep.check("session"); !strings.Contains(detail, "resumed session") || !strings.Contains(detail, "presumed gone") {
		t.Fatalf("%s", detail)
	}
	resumed := *rep.Session
	if rep = onboard(t, s, h, "team_continue", map[string]any{"team": "Other"}); rep.Status != "blocked" {
		t.Fatalf("wrong team accepted: %+v", rep)
	}
	if _, isErr := callTool(t, s, "team_continue", map[string]any{"role": "worker"}); !isErr {
		t.Fatal("continue accepted a role")
	}
	if _, isErr := callTool(t, s, "team_setup", map[string]any{"team": "Pilot", "role": "worker", "token": "x"}); !isErr {
		t.Fatal("setup accepted an unknown argument")
	}

	// The CLI core on the same checkout sees the MCP's session: one state.
	cli := teamsetup.Continue(teamsetup.Env{Dir: repo, Root: root, Version: "v0.7.1", TeamCall: func(ctx context.Context, name string, raw json.RawMessage) (int, []byte, error) {
		return TeamRequestIn(ctx, repo, root, name, raw)
	}}, "", false)
	if !cli.Joined() || cli.Session.ID != resumed.ID || cli.Session.Generation != 2 {
		t.Fatalf("CLI continue: %+v", cli)
	}

	// An explicit leave through the MCP tool ends the membership for both.
	if text, isErr := callTool(t, s, "team_leave", map[string]any{"team": resumed.TeamID, "session_id": resumed.ID, "generation": 2, "idempotency_key": "leave-1"}); isErr {
		t.Fatal(text)
	}
	if st, _ := teamstate.Load(teamstate.Path(root, canon)); st != nil {
		t.Fatalf("state kept after leave: %+v", st)
	}
	rep = onboard(t, s, h, "team_continue", map[string]any{})
	if rep.Status != "blocked" || h.Count("/join") != 1 {
		t.Fatalf("continue after leave: %+v", rep)
	}
	if _, detail, fix := rep.check("session"); !strings.Contains(detail, "no saved membership") || !strings.Contains(fix, "never joins by itself") {
		t.Fatalf("%s / %s", detail, fix)
	}
}

func TestOnboardingStaleHandleAndLostReply(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	s := stdioFor(repo, root)

	// A join whose reply never arrived: the key is saved, the retry replays
	// it, one session exists.
	h.Garbage = 1
	rep := onboard(t, s, h, "team_setup", map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile})
	if rep.Status != "blocked" {
		t.Fatalf("unreadable reply reported joined: %+v", rep)
	}
	canon, _ := filepath.EvalSymlinks(repo)
	st, _ := teamstate.Load(teamstate.Path(root, canon))
	if st == nil || st.JoinKey == "" || st.SessionID != "" {
		t.Fatalf("pending join not recorded: %+v", st)
	}
	rep = onboard(t, s, h, "team_setup", map[string]any{"team": "Pilot", "role": "worker"})
	if rep.Status != "joined" || len(h.JoinKeys) != 2 || h.JoinKeys[0] != h.JoinKeys[1] || len(h.Sessions) != 1 {
		t.Fatalf("replay: %+v keys %v sessions %d", rep, h.JoinKeys, len(h.Sessions))
	}
	// Continue replays a pending join too, but never a fresh one.
	if rep = onboard(t, s, h, "team_continue", map[string]any{}); rep.Status != "joined" || len(h.JoinKeys) != 2 {
		t.Fatalf("%+v", rep)
	}

	// A handle the hub refuses: reported, nothing taken over, no join.
	h.Roster = 409
	rep = onboard(t, s, h, "team_continue", map[string]any{})
	if rep.Status != "blocked" || len(h.JoinKeys) != 2 {
		t.Fatalf("stale handle: %+v", rep)
	}
	if _, detail, fix := rep.check("session"); !strings.Contains(detail, "closed or was advanced") || !strings.Contains(fix, "nothing was taken over") {
		t.Fatalf("%s / %s", detail, fix)
	}
	h.Roster = 0
	// new_session is the explicit way out, and a second worker session is
	// the result the caller asked for.
	rep = onboard(t, s, h, "team_setup", map[string]any{"team": "Pilot", "role": "worker", "new_session": true})
	if rep.Status != "joined" || len(h.Sessions) != 2 {
		t.Fatalf("new_session: %+v", rep)
	}
	if _, isErr := callTool(t, s, "team_setup", map[string]any{"team": "Pilot", "role": "worker", "resume": true, "new_session": true}); !isErr {
		t.Fatal("resume and new_session together accepted")
	}
}

func TestOnboardingRefusesUnusableCredentialsAndWrongCheckout(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)

	// Another checkout of another project under the same state root: no
	// credential, no shared state, and the fix leads with the other-account
	// case because that is what a missing credential looks like from a
	// process that is not the installer.
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, ".aimem.json"), []byte(`{"project":"beta"}`), 0600); err != nil {
		t.Fatal(err)
	}
	so := &srv{project: "beta", taskSetup: func() (TaskCallFunc, error) { return taskCallerIn(other, root) }, local: &localCheckout{dir: other, root: root}}
	rep := onboard(t, so, h, "team_setup", map[string]any{"team": "Pilot", "role": "worker"})
	if rep.Status != "blocked" || len(rep.Checks) != 1 || h.Count("/join") != 0 {
		t.Fatalf("%+v", rep)
	}
	if _, detail, _ := rep.check("binding"); !strings.Contains(detail, "state root "+root) || !strings.Contains(detail, "running as "+rep.RunAs) {
		t.Fatalf("binding detail: %s", detail)
	}
	if rep = onboard(t, so, h, "team_continue", map[string]any{}); rep.Status != "blocked" || h.Count("/join") != 0 {
		t.Fatalf("%+v", rep)
	}

	// The bound checkout joined; the other checkout still has nothing.
	s := stdioFor(repo, root)
	if rep = onboard(t, s, h, "team_setup", map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile}); rep.Status != "joined" {
		t.Fatalf("%+v", rep)
	}
	if rep = onboard(t, so, h, "team_continue", map[string]any{}); rep.Status != "blocked" {
		t.Fatalf("other checkout saw a membership: %+v", rep)
	}

	// The credential exists but this process cannot open it: the classed
	// refusal, no hub call, no reinstall advice up front.
	files, _ := filepath.Glob(filepath.Join(root, "task-credentials", "*.json"))
	if len(files) != 1 {
		t.Fatalf("credential files: %v", files)
	}
	want := taskcred.ClassDecrypt
	if runtime.GOOS == "windows" {
		if err := os.WriteFile(files[0], []byte("not a dpapi blob"), 0600); err != nil {
			t.Fatal(err)
		}
	} else {
		if os.Geteuid() == 0 {
			t.Skip("root reads every file; the denied class cannot be produced")
		}
		if err := os.Chmod(files[0], 0); err != nil {
			t.Fatal(err)
		}
		want = taskcred.ClassDenied
	}
	before := len(h.Requests)
	for _, name := range []string{"team_setup", "team_continue"} {
		args := map[string]any{}
		if name == "team_setup" {
			args = map[string]any{"team": "Pilot", "role": "worker"}
		}
		rep = onboard(t, s, h, name, args)
		if rep.Status != "blocked" || len(rep.Checks) != 1 {
			t.Fatalf("%s: %+v", name, rep)
		}
		_, detail, fix := rep.check("binding")
		if want == taskcred.ClassDecrypt && !strings.Contains(detail, "cannot decrypt") || want == taskcred.ClassDenied && !strings.Contains(detail, "cannot read") {
			t.Fatalf("%s: %s", name, detail)
		}
		if !strings.Contains(fix, rep.RunAs) || strings.HasPrefix(fix, "run `aimem task-token set`") {
			t.Fatalf("%s: %s", name, fix)
		}
	}
	if len(h.Requests) != before {
		t.Fatalf("hub reached with an unusable credential: %v", h.Requests[before:])
	}
}

func TestOnboardingToolsAreAbsentFromTheHubFacade(t *testing.T) {
	// The hub facade has a task caller but no checkout: the tools are not
	// listed and refuse by name, for a full principal and for a tasks-only one.
	refusing := func(context.Context, string, string, map[string]string, []byte) (int, []byte, error) {
		return 500, nil, nil
	}
	for _, s := range []*srv{{tasks: refusing}, {tasks: refusing, tasksOnly: true}} {
		var listed struct {
			Result struct {
				Tools []map[string]any `json:"tools"`
			} `json:"result"`
		}
		json.Unmarshal(s.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)), &listed)
		for _, d := range listed.Result.Tools {
			if isOnboardTool(d["name"].(string)) {
				t.Fatalf("hub facade lists %v", d["name"])
			}
		}
		for _, name := range []string{"team_setup", "team_continue"} {
			text, isErr := callTool(t, s, name, map[string]any{"team": "Pilot", "role": "worker"})
			if !isErr || !strings.Contains(text, "never on the hub") {
				t.Fatalf("%s on the hub: %s", name, text)
			}
		}
	}
	// Hidden with the task tools when the project has tasks off.
	off := &srv{project: "alpha", taskState: taskStateDisabled, local: &localCheckout{dir: ".", root: t.TempDir()}}
	if text, isErr := callTool(t, off, "team_setup", map[string]any{"team": "Pilot", "role": "worker"}); !isErr || !strings.Contains(text, "not enabled") {
		t.Fatalf("%s", text)
	}
}

func TestOnboardToolDefsAreValidSchema(t *testing.T) {
	for _, td := range onboardToolDefs {
		schema := td["inputSchema"].(map[string]any)
		if req, present := schema["required"]; present {
			if _, ok := req.([]string); !ok {
				t.Fatalf("tool %v: required must be a string slice, got %T", td["name"], req)
			}
		}
		b, _ := json.Marshal(td)
		for _, forbidden := range []string{"token", "checkout", "command", "executable"} {
			if strings.Contains(string(b), `"`+forbidden+`"`) {
				t.Fatalf("tool %v takes %q", td["name"], forbidden)
			}
		}
	}
	// A profile, once given, must be complete: no half declaration.
	if _, isErr := (&srv{local: &localCheckout{dir: t.TempDir(), root: t.TempDir()}}).onboardTool("team_setup", json.RawMessage(`{"team":"Pilot","role":"worker","profile":{"label":"x"}}`)); isErr == nil {
		t.Fatal("profile without platform accepted")
	}
}

func TestGitHeadOnlyProbesNothingButHead(t *testing.T) {
	if _, err := teamsetup.GitHeadOnly(t.TempDir(), "status", "--porcelain"); err != teamsetup.ErrNotProbed {
		t.Fatalf("status ran: %v", err)
	}
	if _, err := teamsetup.GitHeadOnly(t.TempDir(), "rev-parse", "HEAD"); err == nil {
		t.Fatal("HEAD of a non-repository")
	}
}
