package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/teamsession"
	"aimem/internal/teamstate"
)

// stateSnapshot lists every file under the state root with its content, so
// a test can prove a hook wrote nothing there.
func stateSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(root, p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

func sameSnapshot(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestTeamConversationHooksCaptureAndRecallNothing: with AIMEM_TEAM_SESSION
// set, the capture hooks write nothing and ask no hub, and session start
// injects only the checkout's own handoff (D4). Without it, the same hooks
// capture as before.
func TestTeamConversationHooksCaptureAndRecallNothing(t *testing.T) {
	g := newTeamSessionRig(t)
	a := scriptAicrew(g)
	a.set(handleA, aicrewEntry{session: "sess-A", token: g.aliceTk, generation: "4"})
	path := g.openSession(t, handleA, "sess-A")
	team := []string{teamsession.EnvVar + "=" + path}

	checkout := t.TempDir()
	os.MkdirAll(filepath.Join(checkout, "docs"), 0o755)
	os.WriteFile(filepath.Join(checkout, "docs", "SESSION-STATE.md"), []byte("HANDOFF-MARKER\n"), 0o644)
	os.WriteFile(filepath.Join(checkout, ".aimem.json"), []byte(`{"session_facts": 800}`), 0o644)
	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	os.WriteFile(transcript, []byte(`{"type":"user","message":{"role":"user","content":"hello"}}`+"\n"), 0o644)
	claude := `{"session_id":"s-1","transcript_path":"` + filepath.ToSlash(transcript) + `","cwd":"` + filepath.ToSlash(checkout) + `","hook_event_name":"Stop"}`
	codex := `{"session_id":"s-1","turn_id":"t-1","cwd":"` + filepath.ToSlash(checkout) + `","hook_event_name":"Stop","last_assistant_message":"done"}`
	submit := `{"project_id":"alpha","event":{"session_id":"s-1","kind":"turn","user_request":"hello"}}`

	for _, c := range []struct{ cmd, stdin string }{{"submit-claude", claude}, {"submit-codex", codex}, {"submit", submit}} {
		before, hubBefore := stateSnapshot(t, g.state), g.count()
		out, errOut, err := runAimemEnv(t, checkout, g.state, c.stdin, team, c.cmd)
		if err != nil || out != "" {
			t.Fatalf("%s in a team conversation: %v %q %q", c.cmd, err, out, errOut)
		}
		if !sameSnapshot(before, stateSnapshot(t, g.state)) {
			t.Errorf("%s wrote into the state root in a team conversation", c.cmd)
		}
		if g.count() != hubBefore {
			t.Errorf("%s reached the hub in a team conversation", c.cmd)
		}
	}
	// The same capture outside team mode does reach the hub or its spool:
	// the refusal above is team mode's doing, not a dead input.
	before, hubBefore := stateSnapshot(t, g.state), g.count()
	if _, errOut, err := runAimemEnv(t, checkout, g.state, codex, nil, "submit-codex"); err != nil {
		t.Fatalf("personal submit-codex: %v %s", err, errOut)
	}
	if sameSnapshot(before, stateSnapshot(t, g.state)) && g.count() == hubBefore {
		t.Fatal("personal submit-codex captured nothing; the team-mode check proves nothing")
	}

	// Session start: only the checkout's handoff and the team notice. With a
	// checkpoint token configured, personal session start asks the hub for a
	// newer handoff; team mode never does. Personal hub traffic ignores
	// ca_file, so the test hub's certificate is reached through insecure.
	if err := adapter.SaveHubs(g.state, map[string]*adapter.HubConfig{
		"hub": {URL: g.ts.URL, Token: identityAdminToken, TaskToken: g.secret, CAFile: g.caFile, Insecure: true}}, "hub"); err != nil {
		t.Fatal(err)
	}
	hubBefore = g.count()
	out, errOut, err := runAimemEnv(t, checkout, g.state, "", team, "session-start")
	if err != nil {
		t.Fatalf("team session-start: %v %s", err, errOut)
	}
	if !strings.Contains(out, "HANDOFF-MARKER") || !strings.Contains(out, "aicrew team conversation") ||
		strings.Contains(out, "Possibly relevant knowledge") || strings.Contains(out, "Process context") ||
		strings.Contains(out, "writing_rule") { // a team reads the rule with its role set

		t.Fatalf("team session-start output: %s", out)
	}
	if g.count() != hubBefore {
		t.Fatal("session-start asked the hub in a team conversation")
	}
	hubBefore = g.count()
	if out, _, _ := runAimemEnv(t, checkout, g.state, "", nil, "session-start"); strings.Contains(out, "aicrew team conversation") {
		t.Fatalf("personal session-start carries the team notice: %s", out)
	}
	if g.count() == hubBefore {
		t.Fatal("personal session-start asked no hub; the team-mode check proves nothing")
	}
}

// TestAicrewSessionsAndLegacyTeamStateNeverCross: the two kinds of state live
// in different directories, and neither side's operations touch the other's
// files.
func TestAicrewSessionsAndLegacyTeamStateNeverCross(t *testing.T) {
	g := newTeamSessionRig(t)
	a := scriptAicrew(g)
	a.set(handleA, aicrewEntry{session: "sess-A", token: g.aliceTk, generation: "4"})
	a.set(handleA2, aicrewEntry{session: "sess-A", token: g.aliceTk, generation: "4"})
	checkout := t.TempDir()
	repo, err := teamstate.Canonical(checkout)
	if err != nil {
		t.Fatal(err)
	}
	legacy := teamstate.Path(g.state, repo)
	if filepath.Dir(legacy) == teamsession.Dir(g.state) {
		t.Fatal("aicrew sessions and legacy team state share a directory")
	}
	// Legacy state exists first; aicrew operations leave it byte-identical.
	if err := teamstate.Save(legacy, &teamstate.State{Version: 1, Repo: repo, Project: "alpha", HubName: "hub", SessionID: "legacy-1"}); err != nil {
		t.Fatal(err)
	}
	legacyBytes, _ := os.ReadFile(legacy)
	path := g.openSession(t, handleA, "sess-A")
	if err := runAicrewSession([]string{"refresh", "sess-A"}, strings.NewReader(handleA2), &bytes.Buffer{}, g.state); err != nil {
		t.Fatal(err)
	}
	stdin := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"list_tasks","arguments":{"project":"alpha"}}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"team_accept","arguments":{}}}` + "\n" +
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"team_leave","arguments":{"session_id":"legacy-1"}}}` + "\n"
	out, errOut, err := runAimemEnv(t, checkout, g.state, stdin, []string{teamsession.EnvVar + "=" + path}, "mcp", "-p", "alpha")
	if err != nil || !strings.Contains(out, g.taskID) || strings.Count(out, "not available in a team conversation") != 2 {
		t.Fatalf("team process in a checkout with legacy state: %v %s %s", err, out, errOut)
	}
	if now, _ := os.ReadFile(legacy); !bytes.Equal(now, legacyBytes) {
		t.Fatal("aicrew session work changed the legacy team state")
	}
	// Legacy operations leave the aicrew session file byte-identical.
	aicrewBytes, _ := os.ReadFile(path)
	teamstate.NoteLeave(checkout, g.state, "legacy-1")
	if err := teamstate.Clear(legacy); err != nil {
		t.Fatal(err)
	}
	if st, err := teamstate.Load(teamstate.Path(g.state, repo)); err != nil || st != nil {
		t.Fatalf("legacy load after clear: %+v %v", st, err)
	}
	if now, _ := os.ReadFile(path); !bytes.Equal(now, aicrewBytes) {
		t.Fatal("legacy team state operations changed the aicrew session file")
	}
	entries, _ := os.ReadDir(teamsession.Dir(g.state))
	var names []string
	for _, e := range entries {
		if e.Name() != ".lock" { // the lifecycle commands' lock file
			names = append(names, e.Name())
		}
	}
	if len(names) != 1 {
		t.Fatalf("the aicrew session directory holds %v", names)
	}
}
