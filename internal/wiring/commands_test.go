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
	if written != 3 {
		t.Fatalf("written %d: %+v", written, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "prompts", "join_team.md")); err == nil {
		t.Fatal("prompt written without a Codex home")
	}
	claude := read(t, filepath.Join(dir, ".claude", "skills", "join_team", "SKILL.md"))
	for _, want := range []string{"name: join_team", "argument-hint: TEAM [worker|coordinator]", managedMarker, "--platform claude-code", "`claude --version`", "$ARGUMENTS", "docs/TEAM-PLAYBOOKS.md", "never\n   paste a token"} {
		if !strings.Contains(claude, want) {
			t.Fatalf("claude skill missing %q:\n%s", want, claude)
		}
	}
	if strings.Contains(claude, "{{") {
		t.Fatal("unrendered placeholder")
	}
	oc := read(t, filepath.Join(dir, ".opencode", "commands", "join_team.md"))
	if !strings.Contains(oc, "--platform opencode") || strings.Contains(oc, "name: join_team") {
		t.Fatalf("opencode command:\n%s", oc)
	}
	codex := read(t, filepath.Join(dir, ".agents", "skills", "join-team", "SKILL.md"))
	if !strings.Contains(codex, "name: join-team") || !strings.Contains(codex, "--platform codex") {
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
	if !strings.Contains(prompt, "argument-hint: TEAM [worker|coordinator]") || !strings.Contains(prompt, "--platform codex") {
		t.Fatalf("codex prompt:\n%s", prompt)
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
