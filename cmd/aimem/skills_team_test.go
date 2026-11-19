package main

// `aimem skills install` for a team member (aicrew's bring-up): the member's
// user holds no personal grant on the team's project, so the checkout's
// credential is refused; in the team conversation AIMEM_TEAM_SESSION names,
// the command reads the process and the skills as that conversation, under
// the team's grant, against the hub's TLS listener, and no request is made
// without the conversation's handle.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/introspect/introspecttest"
	"aimem/internal/teamsession"
)

func TestSkillsInstallInATeamConversation(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no git on the member machine
	g := newTeamSessionRig(t)
	admin := func(method, path string, body any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, g.ts.URL+path, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+identityAdminToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := g.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s %s: %d", method, path, resp.StatusCode)
		}
	}
	// The hub's release set, and a process for alpha (granted to the team)
	// and beta (not granted) that pins one of its skills.
	admin("PUT", "/v1/skills/release", map[string]any{"version": "1.33.13", "files": map[string][]byte{
		"oh-code-review/SKILL.md": []byte("review 1.33.13"), "verify-delivery/SKILL.md": []byte("verify 1.33.13")}})
	manifest := `{"version":2,"handbook":"proc/handbook.md","skills":[{"name":"oh-code-review","version":"1.33.13","source":"release"}]}`
	selection := map[string]any{"repo": "https://forge.example.org/team/process.git", "commit": strings.Repeat("7", 40),
		"manifest": "proc/manifest.json", "expected_commit": "",
		"files": map[string][]byte{"proc/manifest.json": []byte(manifest), "proc/handbook.md": []byte("# Handbook\n")}}
	if _, err := g.reg.Open("beta"); err != nil {
		t.Fatal(err)
	}
	admin("PUT", "/v1/projects/alpha/process", selection)
	admin("PUT", "/v1/projects/beta/process", selection)

	// The member's home, provisioned as docs/PILOT-HUB-RUNBOOK.md says: the
	// member's own user token is both the hub entry's token and the
	// individual credential, and the member holds no personal grant on alpha.
	if err := adapter.SaveHubs(g.state, map[string]*adapter.HubConfig{
		"hub": {URL: g.ts.URL, Token: g.secret, TaskToken: g.secret, CAFile: g.caFile}}, "hub"); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", g.state)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(teamsession.EnvVar, "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".aimem.json"), []byte(`{"project":"alpha"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(dir, ".claude", "skills", "oh-code-review", "SKILL.md")

	// Without a team conversation: the member's own credential, refused as
	// it was in the field, with the text that names the personal grant.
	from := g.count()
	err := skillsInstall(dir, "alpha", false, "claude", false)
	if err == nil || !strings.Contains(err.Error(), "the project's process is not available (denied): the hub knows no project alpha, or this checkout's credential may not read it") ||
		!strings.Contains(err.Error(), "aimem access grant add") {
		t.Fatalf("personal install: %v", err)
	}
	if _, err := os.Stat(installed); !os.IsNotExist(err) {
		t.Fatal("a refused install wrote a skill")
	}
	for _, r := range g.requestsSince(from) {
		if r.handle != "" {
			t.Fatalf("a personal install sent a team handle on %s", r.path)
		}
	}

	var out bytes.Buffer
	if err := runAicrewSession([]string{"open", "--service", "aicrew-example", "--team", "team-1", "--session", "sess-A"},
		strings.NewReader(handleA), &out, g.state); err != nil {
		t.Fatalf("open: %v", err)
	}
	session := strings.TrimSpace(out.String())
	t.Setenv(teamsession.EnvVar, session)
	onlyTeam := func(what string, from int) {
		t.Helper()
		reqs := g.requestsSince(from)
		if len(reqs) == 0 {
			t.Fatalf("%s: no request reached the hub", what)
		}
		for _, r := range reqs {
			if r.handle != handleA {
				t.Fatalf("%s: %s was sent without the team conversation's handle", what, r.path)
			}
		}
	}

	// In the team conversation: the pinned skill is installed.
	from = g.count()
	text, err := stdoutOf(t, func() error { return skillsInstall(dir, "alpha", false, "claude", false) })
	if err != nil || !strings.Contains(text, "installed oh-code-review 1.33.13 (release) for claude: "+filepath.Dir(installed)) {
		t.Fatalf("team install: %v\n%s", err, text)
	}
	if raw, _ := os.ReadFile(installed); string(raw) != "review 1.33.13" {
		t.Fatalf("installed skill: %q", raw)
	}
	onlyTeam("install", from)

	// list reads the same way.
	from = g.count()
	text, err = stdoutOf(t, func() error { return skillsList(dir, "alpha") })
	if err != nil || !strings.Contains(text, "release set: 1.33.13") || !strings.Contains(text, "oh-code-review 1.33.13 (release; installed here)") {
		t.Fatalf("team list: %v\n%s", err, text)
	}
	onlyTeam("list", from)

	// A project outside the team's grant: refused, naming the conversation
	// and the team's grant, with nothing substituted.
	from = g.count()
	err = skillsInstall(dir, "beta", false, "claude", false)
	if err == nil || !strings.Contains(err.Error(), "team conversation sess-A of team team-1") || !strings.Contains(err.Error(), "grant_denied") ||
		!strings.Contains(err.Error(), "aimem identity team grant") || !strings.Contains(err.Error(), "nothing was read outside the team conversation") {
		t.Fatalf("ungranted project: %v", err)
	}
	onlyTeam("ungranted project", from)
	// The index read refuses it the same way, for list and install --all,
	// which read no process first.
	from = g.count()
	for name, run := range map[string]func() error{
		"list":          func() error { return skillsList(dir, "beta") },
		"install --all": func() error { return skillsInstall(dir, "beta", false, "claude", true) },
	} {
		err := run()
		if err == nil || !strings.Contains(err.Error(), "team conversation sess-A of team team-1: the hub refused the skills read for project beta: grant_denied") ||
			!strings.Contains(err.Error(), "aimem identity team grant") || !strings.Contains(err.Error(), "nothing was read outside the team conversation") {
			t.Fatalf("ungranted project, %s: %v", name, err)
		}
	}
	onlyTeam("ungranted project, index", from)

	// A conversation aicrew has ended: the hub does not verify it, the
	// command fails naming it, and nothing falls back.
	g.fake.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		introspecttest.WriteJSON(w, introspecttest.InactiveReply(got.Nonce))
	})
	from = g.count()
	err = skillsInstall(dir, "alpha", false, "claude", false)
	if err == nil || !strings.Contains(err.Error(), "team conversation sess-A of team team-1: the team context is not verified") ||
		!strings.Contains(err.Error(), "restart the conversation through aicrew") || !strings.Contains(err.Error(), "nothing was read outside the team conversation") {
		t.Fatalf("ended conversation: %v", err)
	}
	onlyTeam("ended conversation", from)
	if err := skillsList(dir, "alpha"); err == nil || !strings.Contains(err.Error(), "the team context is not verified") {
		t.Fatalf("list in an ended conversation: %v", err)
	}

	// A session file that cannot be read: nothing is sent at all.
	t.Setenv(teamsession.EnvVar, filepath.Join(t.TempDir(), "missing.json"))
	from = g.count()
	err = skillsInstall(dir, "alpha", false, "claude", true)
	if err == nil || !strings.Contains(err.Error(), "the team session file cannot be used") || !strings.Contains(err.Error(), "nothing was read outside the team conversation") {
		t.Fatalf("unusable session file: %v", err)
	}
	if n := len(g.requestsSince(from)); n != 0 {
		t.Fatalf("%d requests reached the hub with an unusable session file", n)
	}
}
