package teamsetup

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/store"
	"aimem/internal/taskcred"
	"aimem/internal/teamstate"
)

const testToken = "aimem_user_" + "e" + "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

// identityHub answers the two unauthenticated-or-identity reads the core
// makes directly (status, identity); everything else goes through the
// injected session caller, so a hub that served only these proves the
// caller is the seam.
func identityHub(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/status":
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": "v0.7.1", "hub_name": "fake"})
		case r.Header.Get("Authorization") != "Bearer "+testToken:
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]any{"error": "bad credential"})
		case r.URL.Path == "/v1/access/identity":
			json.NewEncoder(w).Encode(map[string]any{"user_id": "u-1", "token_id": "t-1", "name": "pilot-a", "role": "user", "scope": "project", "project": "alpha", "task_write": true, "tasks_enabled": true})
		default:
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"error": "unexpected " + r.URL.Path})
		}
	}))
	t.Cleanup(ts.Close)
	return ts
}

// checkout binds a temporary checkout to the hub with a project-local
// credential and returns (checkout, state root).
func checkout(t *testing.T, ts *httptest.Server) (string, string) {
	t.Helper()
	root, repo := t.TempDir(), t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, Token: "checkpoint"}}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".aimem.json"), []byte(`{"project":"alpha"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := taskcred.Set(context.Background(), repo, root, testToken); err != nil {
		t.Fatal(err)
	}
	return repo, root
}

// sessionCaller is an in-memory hub session API: it records every
// operation the core sends and answers the smallest protocol-1 replies.
type sessionCaller struct {
	mu    sync.Mutex
	calls []string
}

func (c *sessionCaller) call(_ context.Context, name string, raw json.RawMessage) (int, []byte, error) {
	c.mu.Lock()
	c.calls = append(c.calls, name)
	c.mu.Unlock()
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil {
		return 0, nil, err
	}
	if args["project"] != "alpha" {
		return 0, nil, errors.New("project not bound: " + string(raw))
	}
	if strings.Contains(string(raw), testToken) {
		return 0, nil, errors.New("token in session arguments")
	}
	session := map[string]any{"id": "sess-1", "team_id": "team-1", "generation": 1, "role": "worker", "state": "active", "availability": "available", "profile_revision": 1, "label": "w", "last_seen_at": "2026-09-23T04:00:00Z"}
	reply := func(code int, v any) (int, []byte, error) {
		b, _ := json.Marshal(v)
		return code, b, nil
	}
	switch name {
	case "team_list":
		return reply(200, map[string]any{"protocol_version": 1, "teams": []map[string]any{{"id": "team-1", "name": "Pilot", "coordinator": false, "coordinator_active": true}}})
	case "team_join":
		return reply(201, map[string]any{"protocol_version": 1, "session": session})
	case "team_members":
		return reply(200, map[string]any{"protocol_version": 1, "members": []map[string]any{session}, "next_cursor": "", "server_time": "2026-09-23T04:00:02Z"})
	case "team_heartbeat":
		return reply(200, map[string]any{"protocol_version": 1})
	case "team_reserved":
		return reply(404, map[string]any{"error": "no reserved attempt"})
	case "team_inbox":
		return reply(200, map[string]any{"protocol_version": 1, "messages": []any{}, "next_cursor": 0, "has_more": false})
	}
	return reply(404, map[string]any{"error": "unexpected " + name})
}

func (c *sessionCaller) names() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.calls, " ")
}

func TestRunDrivesTheHubThroughTheInjectedCallerOnly(t *testing.T) {
	ts := identityHub(t)
	repo, root := checkout(t, ts)
	caller := &sessionCaller{}
	env := Env{Dir: repo, Root: root, Version: "v0.7.1", TeamCall: caller.call}
	rep := Run(env, &Options{Team: "Pilot", Role: "worker", Profile: store.TeamProfile{Label: "w", Platform: "codex", PlatformVersion: "1", Model: store.TeamModel{Provider: "unknown", ID: "unknown", Version: "unknown", Source: "unknown"}}, PlatformSet: true, ProfileSet: true})
	if !rep.Joined() || rep.Session == nil || rep.Session.ID != "sess-1" {
		t.Fatalf("not joined: %+v", rep)
	}
	if got := caller.names(); got != "team_list team_join team_members team_heartbeat team_reserved team_inbox" {
		t.Fatalf("session operations: %s", got)
	}
	if rep.RunAs == "" || rep.RunAs == "unknown user" {
		t.Fatalf("run_as not recorded: %q", rep.RunAs)
	}
	// A host that runs no probes gets them reported as unchecked, not
	// guessed and not failed.
	for name, want := range map[string]string{"process": "not checked by this entry point", "base commit": "not probed by this entry point"} {
		found := false
		for _, c := range rep.Checks {
			if c.Name == name {
				found = true
				if c.Level != "warn" || !strings.Contains(c.Detail, want) {
					t.Fatalf("%s: %+v", name, c)
				}
			}
		}
		if !found {
			t.Fatalf("no %s check: %+v", name, rep.Checks)
		}
	}
	canon, _ := filepath.EvalSymlinks(repo)
	st, err := teamstate.Load(teamstate.Path(root, canon))
	if err != nil || st == nil || st.SessionID != "sess-1" || st.TokenID != "t-1" || st.JoinKey != "" {
		t.Fatalf("state: %+v %v", st, err)
	}
	var out strings.Builder
	rep.Print(&out, true)
	rep.Print(&out, false)
	if strings.Contains(out.String(), testToken) {
		t.Fatal("token in the report")
	}
	if !strings.Contains(out.String(), `"run_as"`) {
		t.Fatal("run_as missing from the JSON report")
	}
}

func TestContinueNeverJoins(t *testing.T) {
	ts := identityHub(t)
	repo, root := checkout(t, ts)
	caller := &sessionCaller{}
	env := Env{Dir: repo, Root: root, Version: "v0.7.1", TeamCall: caller.call}
	rep := Continue(env, "", false)
	if rep.Joined() || rep.Role != "saved" {
		t.Fatalf("continued without a membership: %+v", rep)
	}
	if strings.Contains(caller.names(), "team_join") {
		t.Fatalf("continue joined: %s", caller.names())
	}
	last := rep.Checks[len(rep.Checks)-1]
	if last.Name != "session" || last.Level != "fail" || !strings.Contains(last.Detail, "no saved membership") {
		t.Fatalf("%+v", last)
	}
	// After a setup, continue verifies the same session through the same
	// caller and still sends no join.
	if !Run(env, &Options{Team: "Pilot", Role: "worker", Profile: store.TeamProfile{Label: "w", Platform: "codex", PlatformVersion: "1", Model: store.TeamModel{Provider: "unknown", ID: "unknown", Version: "unknown", Source: "unknown"}}, PlatformSet: true, ProfileSet: true}).Joined() {
		t.Fatal("setup did not join")
	}
	caller.calls = nil
	rep = Continue(env, "Pilot", false)
	if !rep.Joined() || rep.Team != "Pilot" || rep.Role != "worker" {
		t.Fatalf("continue: %+v", rep)
	}
	if got := caller.names(); got != "team_members team_members team_heartbeat team_reserved team_inbox" {
		t.Fatalf("continue operations: %s", got)
	}
	if rep = Continue(env, "Other", false); rep.Joined() || !strings.Contains(rep.Checks[len(rep.Checks)-1].Detail, `not "Other"`) {
		t.Fatalf("wrong team accepted: %+v", rep)
	}
}

// TestUnusableCredentialIsNotReplaced covers the pilot failure: a
// credential that exists and may be valid but that this process cannot
// open (another account's file on Unix, another account's DPAPI blob on
// Windows). The fix names the account and the owner-context path, never
// a bare token reinstall.
func TestUnusableCredentialIsNotReplaced(t *testing.T) {
	ts := identityHub(t)
	repo, root := checkout(t, ts)
	canon, _ := filepath.EvalSymlinks(repo)
	files, _ := filepath.Glob(filepath.Join(root, "task-credentials", "*.json"))
	if len(files) != 1 {
		t.Fatalf("credential files: %v", files)
	}
	wantClass := taskcred.ClassDecrypt
	if runtime.GOOS == "windows" {
		// Bytes DPAPI never protected: refused like another account's blob.
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
		wantClass = taskcred.ClassDenied
	}
	_, err := taskcred.Resolve(canon, root)
	if got := taskcred.Classify(err); got != wantClass {
		t.Fatalf("class %q, want %q (%v)", got, wantClass, err)
	}
	caller := &sessionCaller{}
	rep := Run(Env{Dir: repo, Root: root, Version: "v0.7.1", TeamCall: caller.call}, &Options{Team: "Pilot", Role: "worker"})
	if rep.Joined() || len(rep.Checks) != 1 || rep.Checks[0].Name != "binding" || rep.Checks[0].Level != "fail" {
		t.Fatalf("%+v", rep)
	}
	fix := rep.Checks[0].Fix
	if !strings.Contains(fix, rep.RunAs) || !strings.Contains(fix, "local aimem MCP process") || !strings.Contains(fix, "only if this account is meant to hold its own credential") {
		t.Fatalf("fix does not route to the owner context: %s", fix)
	}
	if strings.HasPrefix(fix, "run `aimem task-token set`") {
		t.Fatalf("fix replaces a credential this process merely cannot open: %s", fix)
	}
	if caller.names() != "" {
		t.Fatalf("hub reached with an unusable credential: %s", caller.names())
	}
}

func TestCredentialFixByClass(t *testing.T) {
	reinstall := "run `aimem task-token set` in this checkout"
	cases := []struct {
		err       error
		must, not string
	}{
		{&taskcred.Failure{Class: taskcred.ClassMissing, Msg: "local task credential required: no such file"}, reinstall, "MCP"},
		{&taskcred.Failure{Class: taskcred.ClassMissing, Msg: `hub "h" has no task credential: aimem hub task-token h <ordinary-token>`}, "aimem hub task-token", "MCP"},
		{&taskcred.Failure{Class: taskcred.ClassMalformed, Msg: "malformed local task credential; run aimem task-token set"}, reinstall, "MCP"},
		{&taskcred.Failure{Class: taskcred.ClassRebound, Msg: "local task credential binding changed; run aimem task-token set"}, reinstall, "MCP"},
		{&taskcred.Failure{Class: taskcred.ClassDenied, Msg: "cannot read required local task credential"}, "this process (acct) is not allowed to read it", ""},
		{&taskcred.Failure{Class: taskcred.ClassDecrypt, Msg: "cannot decrypt required local task credential"}, "protected for another OS account than this process (acct)", ""},
		{&taskcred.Failure{Class: taskcred.ClassConfig, Msg: "task credential state directory must be outside the checkout"}, "credential location", "task-token set"},
		{errors.New(`no hub configured for this project (binding "x"); configure it with aimem hub add`), "aimem hub add", "task-token"},
		{errors.New("task tools refused: invalid .aimem.json"), "repair .aimem.json", "task-token"},
		{errors.New("something else"), "fix the checkout binding or credential", "task-token"},
	}
	for _, c := range cases {
		fix := CredentialFix(c.err, "acct")
		if !strings.Contains(fix, c.must) || c.not != "" && strings.Contains(fix, c.not) {
			t.Errorf("%v: %s", c.err, fix)
		}
	}
	for _, class := range []taskcred.Class{taskcred.ClassDenied, taskcred.ClassDecrypt} {
		fix := CredentialFix(&taskcred.Failure{Class: class}, "acct")
		if !strings.Contains(fix, "reinstall with `aimem task-token set` only if this account is meant to hold its own credential") {
			t.Errorf("%s: reinstall not conditioned: %s", class, fix)
		}
	}
}
