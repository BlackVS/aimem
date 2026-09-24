package wiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallCommandsWritesRefreshesAndRespectsForeignFiles(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	// No Codex home: the user-level prompt is skipped, not written.
	out := InstallCommands(dir, home, true)
	written := 0
	for _, f := range out {
		if f.Level == "fail" {
			t.Fatalf("%+v", f)
		}
		if f.Repaired {
			written++
		}
	}
	if written != 6 { // two entry points, three checkout targets each
		t.Fatalf("written %d: %+v", written, out)
	}
	resume := read(t, filepath.Join(dir, ".claude", "skills", "resume_team", "SKILL.md"))
	for _, want := range []string{"name: resume_team", `argument-hint: "[TEAM]"`, `description: "Continue the aimem team membership`, "Call `team_continue` with `team` = TEAM when given, nothing else.", "never joins", `allowed-tools: "mcp__aimem__team_continue mcp__aimem__team_context mcp__aimem__process_context Bash(git status *)"`, "STOP_REQUESTED: stop safely", "read them in\n     full with `team_inbox` from cursor 0"} {
		if !strings.Contains(resume, want) {
			t.Fatalf("resume skill missing %q:\n%s", want, resume)
		}
	}
	if !strings.Contains(read(t, filepath.Join(dir, ".agents", "skills", "resume-team", "SKILL.md")), "name: resume-team") {
		t.Fatal("codex resume skill")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "prompts", "join_team.md")); err == nil {
		t.Fatal("prompt written without a Codex home")
	}
	claude := read(t, filepath.Join(dir, ".claude", "skills", "join_team", "SKILL.md"))
	for _, want := range []string{"name: join_team", `argument-hint: "TEAM [worker|coordinator]"`, `description: "Join an aimem team`, managedMarker, `"platform": "claude-code"`, "`claude --version`", "$ARGUMENTS", "never\n   paste a token", "call the `team_list` tool", `allowed-tools: "mcp__aimem__team_setup mcp__aimem__team_list mcp__aimem__team_context mcp__aimem__process_context Bash(claude --version)"`} {
		if !strings.Contains(claude, want) {
			t.Fatalf("claude skill missing %q:\n%s", want, claude)
		}
	}
	if strings.Contains(claude, "{{") {
		t.Fatal("unrendered placeholder")
	}
	oc := read(t, filepath.Join(dir, ".opencode", "commands", "join_team.md"))
	if !strings.Contains(oc, `"platform": "opencode"`) || strings.Contains(oc, "name: join_team") {
		t.Fatalf("opencode command:\n%s", oc)
	}
	codex := read(t, filepath.Join(dir, ".agents", "skills", "join-team", "SKILL.md"))
	if !strings.Contains(codex, "name: join-team") || !strings.Contains(codex, `"platform": "codex"`) {
		t.Fatalf("codex skill:\n%s", codex)
	}
	// Idempotent: everything present and current, nothing rewritten.
	for _, f := range InstallCommands(dir, home, true) {
		if f.Repaired || f.Level != "ok" {
			t.Fatalf("second run: %+v", f)
		}
	}
	// An outdated managed file is refreshed; a foreign file is left alone.
	write(t, filepath.Join(dir, ".opencode", "commands", "join_team.md"), managedMarker+"\nold body\n")
	write(t, filepath.Join(dir, ".agents", "skills", "join-team", "SKILL.md"), "---\nname: join-team\n---\nmine\n")
	out = InstallCommands(dir, home, true)
	var refreshed, foreign bool
	for _, f := range out {
		if strings.HasPrefix(f.File, ".opencode") && f.Repaired && strings.Contains(f.Detail, "outdated") {
			refreshed = true
		}
		if strings.HasPrefix(f.File, ".agents") && f.Level == "warn" && strings.Contains(f.Detail, "not managed") {
			foreign = true
		}
	}
	if !refreshed || !foreign {
		t.Fatalf("%+v", out)
	}
	if read(t, filepath.Join(dir, ".agents", "skills", "join-team", "SKILL.md")) != "---\nname: join-team\n---\nmine\n" {
		t.Fatal("foreign file rewritten")
	}
	// With a Codex home the prompt is written there.
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	InstallCommands(dir, home, true)
	prompt := read(t, filepath.Join(home, ".codex", "prompts", "join_team.md"))
	if !strings.Contains(prompt, `argument-hint: "TEAM [worker|coordinator]"`) || !strings.Contains(prompt, `"platform": "codex"`) {
		t.Fatalf("codex prompt:\n%s", prompt)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "prompts", "resume_team.md")); err != nil {
		t.Fatal("codex resume prompt not written with a Codex home")
	}
}

func TestInstallCommandsReportOnly(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	out := InstallCommands(dir, home, false)
	for _, f := range out {
		if f.Repaired || (f.Level != "warn" && !strings.Contains(f.Detail, "skipped")) {
			t.Fatalf("%+v", f)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("report-only wrote %v", entries)
	}
}

// flat folds a rendered text's line breaks and indentation, so a phrase is
// found wherever the template wraps it.
func flat(s string) string { return strings.Join(strings.Fields(s), " ") }

// Every branch of both entry points, in every client's rendering: the
// tools checked before anything else, the stop without a shell fallback,
// the blocked report, the readiness and the delivered context with its
// terminators, a cut or stale delivery, the existing attempt and the
// heartbeat. No rendering names a repository path for the guidance, and no
// fallback contradicts the readiness rules.
func TestEntryPointsUseTheDeliveredContext(t *testing.T) {
	common := []string{
		"`team_context` and `process_context`. If any is missing",
		"tell the user which tool is missing, to upgrade aimem on this machine and to restart the client",
		"or any other shell command instead, do not read the guidance from files in a repository",
		"`status` is `blocked`: show the user each failing check with its `fix` line",
		"No context is delivered for a blocked run.",
		"`joined` is membership, not permission to work",
		"`execution` is always `not_verified`: a working MCP server",
		"check those yourself before accepting a coding attempt",
		"complete only if its last line is the terminator its first lines quote",
		"`readiness.role_context.version` and `digest`",
		"`readiness.project_process.version` as its `commit`, and its `sha256` value is `readiness.project_process.digest` without the `sha256:` prefix",
		"A block marked PINNED is the version your accepted attempt was taken under; a newer selection it names is not your rules.",
		"whose block is missing, lacks its terminator or names another version or digest was cut or is stale",
		"the process with `process_context`, and use it only when its terminator matches the readiness fields",
		"never combine parts of different versions",
		"Until you hold both complete, act as a member that is not ready, whatever `ready_for_work` or `next` says: accept nothing, heartbeat `unavailable`, and do not continue a `RUNNING` attempt: block it with the incomplete part as the reason (`team_block`) and stop that work yourself.",
		"`next` wins (an incomplete delivery above still makes you not ready)",
		"While not ready the report has already declined an OFFERED attempt or blocked a RUNNING one, or says why it could not",
		"Delivery is not acknowledgement: nothing records that you read it.",
		"name the guidance digest and the process commit you followed in the evidence of a submitted result",
		"where it differs from a step below, `next` wins",
		"a coordinator issues no offers",
		"accepts nothing, declines any offer the report lists with the readiness reason",
		"The delivered role guidance is the authority for the team protocol and the delivered project process for project policy",
		"`team_context` `section` = its id",
		"`process_context` `template` = its kind",
		"a block does not stop anything running here: stop that work yourself",
		"Never resume a blocked attempt (`team_resume_work`) because readiness came back; read the inbox for the coordinator's answer first.",
		"`available` only while the last report said `ready_for_work`",
		"Do not fall back to a shell command",
	}
	only := map[string][]string{
		"join": {
			"must list `team_setup`, `team_context` and `process_context`",
			"Do not run `aimem teams setup` or any other shell command",
			"call the `team_list` tool", "Never assume coordinator",
			"re-read it, the guidance with `team_context` `role` = ROLE",
			"Only once the report says `ready_for_work`: select only work the process allows",
			"run `/resume_team` once they are fixed",
			"then accept an offer only while ready, and decline it with a reason otherwise",
		},
		"resume": {
			"must list `team_continue`, `team_context` and `process_context`",
			"Do not run `aimem teams continue` or any other shell command",
			"this call delivers it again, and nothing you remember from before it replaces what it delivers",
			"re-read it, the guidance with `team_context` `role` = the saved role",
			"It never joins", "Reconcile before you retry anything",
			"send `team_stopped` only once you have established that the work stopped",
			"read them in full with `team_inbox` from cursor 0",
			"issue offers only while the report says `ready_for_work`",
			"does not continue or resume work",
		},
	}
	assets := commandAssets()
	if len(assets) != 8 {
		t.Fatalf("%d assets", len(assets))
	}
	for _, a := range assets {
		kind := "join"
		if strings.Contains(a.Rel, "resume") {
			kind = "resume"
		}
		text := flat(a.Content)
		for _, want := range append(append([]string{}, common...), only[kind]...) {
			if !strings.Contains(text, want) {
				t.Errorf("%s: missing %q", a.Label, want)
			}
		}
		// The guidance comes from the tools, never from a file in some
		// repository; and a worker is never told to heartbeat available
		// unconditionally.
		for _, bad := range []string{"TEAM-PLAYBOOKS", "docs/", "{{", "Both: heartbeat (`team_heartbeat`, `available`)", "only while ready, accept or decline"} {
			if strings.Contains(a.Content, bad) {
				t.Errorf("%s: contains %q", a.Label, bad)
			}
		}
	}
}

// The generated copies committed to this repository are the binary's
// rendering, byte for byte: they are regenerated with `aimem teams
// commands`, never edited by hand.
func TestCommittedCommandAssetsMatchTheBinary(t *testing.T) {
	n := 0
	for _, a := range commandAssets() {
		if a.User {
			continue
		}
		got, err := os.ReadFile(filepath.Join("..", "..", a.Rel))
		if err != nil {
			t.Fatalf("%s: %v", a.Label, err)
		}
		if string(got) != a.Content {
			t.Errorf("%s differs from the binary's rendering; run `go run ./cmd/aimem teams commands` at the repository root", a.Label)
		}
		n++
	}
	if n != 6 {
		t.Fatalf("%d checkout assets", n)
	}
}
