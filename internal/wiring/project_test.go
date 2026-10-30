package wiring

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aimem/templates"
)

// files reads every file below dir, keyed by its slash path.
func files(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = read(t, p)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func wireOK(t *testing.T, dir string, o ProjectOptions) []Finding {
	t.Helper()
	out, err := Wire(dir, o)
	if err != nil {
		t.Fatalf("Wire: %v", err)
	}
	for _, f := range out {
		if f.Level == "fail" {
			t.Fatalf("Wire failed on %s: %s", f.File, f.Detail)
		}
	}
	return out
}

func TestWireEmptyDirectoryThenNothingChanges(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	wireOK(t, dir, ProjectOptions{Home: home})
	first := files(t, dir)
	for _, want := range []string{"docs/SESSION-STATE.md", ".claude/settings.json", ".codex/hooks.json", ".mcp.json",
		"opencode.json", ".claude/skills/join_team/SKILL.md", ".claude/commands/aimem-analyze.md",
		".opencode/command/aimem-analyze.md", "AGENTS.md", "CLAUDE.md", ".aimem.json"} {
		if _, ok := first[want]; !ok {
			t.Errorf("%s not wired", want)
		}
	}
	// Claude Code runs hooks through sh outside Windows: the installer's
	// guarded form, so a collaborator without aimem sees no hook error.
	// Codex runs them without a shell and gets the bare command.
	claude := `"command": "aimem session-start"`
	if runtime.GOOS != "windows" {
		claude = `"command": "command -v aimem >/dev/null 2>&1 && aimem session-start || true"`
	}
	if !strings.Contains(first[".claude/settings.json"], claude) {
		t.Errorf(".claude/settings.json lacks %s:\n%s", claude, first[".claude/settings.json"])
	}
	if !strings.Contains(first[".codex/hooks.json"], `"command": "aimem session-start"`) {
		t.Errorf(".codex/hooks.json lacks the bare command:\n%s", first[".codex/hooks.json"])
	}
	if first["AGENTS.md"] != string(templates.AgentsMD) {
		t.Error("AGENTS.md is not the template")
	}
	if got := strings.TrimSpace(first[".aimem.json"]); got != "{\n  \"groups\": []\n}" {
		t.Errorf(".aimem.json = %q, want only empty groups", got)
	}
	for _, f := range wireOK(t, dir, ProjectOptions{Home: home}) {
		if f.Repaired {
			t.Errorf("second run rewrote %s: %s", f.File, f.Detail)
		}
	}
	second := files(t, dir)
	if len(second) != len(first) {
		t.Errorf("second run: %d files, first %d", len(second), len(first))
	}
	for k, v := range first {
		if second[k] != v {
			t.Errorf("second run changed %s", k)
		}
	}
}

func TestWireSetsOnlyTheGivenKeys(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	cfg := filepath.Join(dir, ".aimem.json")
	write(t, cfg, "\xef\xbb\xbf"+`{"groups":["x"],"docs":["NOTES.md"],"session_facts":300,"project":"old"}`)
	wireOK(t, dir, ProjectOptions{Home: home, Hub: "lab"})
	got := read(t, cfg)
	if strings.HasPrefix(got, "\xef\xbb\xbf") {
		t.Error(".aimem.json written with a BOM")
	}
	for _, want := range []string{`"hub": "lab"`, `"x"`, `"NOTES.md"`, `"session_facts": 300`, `"project": "old"`} {
		if !strings.Contains(got, want) {
			t.Errorf(".aimem.json lacks %s:\n%s", want, got)
		}
	}
	wireOK(t, dir, ProjectOptions{Home: home, Project: "new", Groups: []string{}})
	got = read(t, cfg)
	for _, want := range []string{`"project": "new"`, `"groups": []`, `"hub": "lab"`, `"NOTES.md"`} {
		if !strings.Contains(got, want) {
			t.Errorf(".aimem.json lacks %s:\n%s", want, got)
		}
	}
	// No keys named: an existing file is not touched at all.
	before := read(t, cfg)
	wireOK(t, dir, ProjectOptions{Home: home})
	if read(t, cfg) != before {
		t.Error("a run naming no keys rewrote .aimem.json")
	}
}

// An empty or whitespace-only .aimem.json is one a session cannot read:
// wire and --check report it and leave it alone. `{}` is a valid config.
func TestWireRefusesAnEmptyConfig(t *testing.T) {
	for name, content := range map[string]string{"empty": "", "whitespace": " \r\n\t\n", "BOM only": "\xef\xbb\xbf\n"} {
		for _, check := range []bool{false, true} {
			dir, home := t.TempDir(), t.TempDir()
			cfg := filepath.Join(dir, ".aimem.json")
			write(t, cfg, content)
			out, err := Wire(dir, ProjectOptions{Home: home, Check: check, Project: "demo"})
			if err != nil {
				t.Fatalf("%s, check %v: %v", name, check, err)
			}
			var got *Finding
			for i := range out {
				if out[i].File == ".aimem.json" {
					got = &out[i]
				}
			}
			if got == nil || got.Level != "fail" || !strings.Contains(got.Detail, "empty") || !strings.Contains(got.Fix, "`{}`") || got.Repaired {
				t.Errorf("%s, check %v: finding %+v", name, check, got)
			}
			if read(t, cfg) != content {
				t.Errorf("%s, check %v: the file was rewritten", name, check)
			}
		}
	}
	dir, home := t.TempDir(), t.TempDir()
	write(t, filepath.Join(dir, ".aimem.json"), "{}")
	wireOK(t, dir, ProjectOptions{Home: home})
	out := wireOK(t, dir, ProjectOptions{Home: home, Check: true})
	for _, f := range out {
		if f.File == ".aimem.json" && f.Level != "ok" {
			t.Errorf("{} reported as %s: %s", f.Level, f.Detail)
		}
	}
}

func TestWireCheckWritesNothing(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	out := wireOK(t, dir, ProjectOptions{Home: home, Check: true, Project: "demo"})
	if n := len(files(t, dir)); n != 0 {
		t.Fatalf("--check wrote %d files", n)
	}
	missing := 0
	for _, f := range out {
		if f.Repaired {
			t.Errorf("--check claims a repair of %s", f.File)
		}
		if f.Level == "warn" && strings.Contains(f.Fix, "aimem wire") {
			missing++
		}
	}
	if missing < 10 {
		t.Errorf("--check on an empty directory reported %d missing items:\n%v", missing, out)
	}
	wireOK(t, dir, ProjectOptions{Home: home, Project: "demo"})
	for _, f := range wireOK(t, dir, ProjectOptions{Home: home, Check: true, Project: "demo"}) {
		if f.Level != "ok" {
			t.Errorf("--check after wiring: %s %s: %s", f.Level, f.File, f.Detail)
		}
	}
}

func TestWireKeepsTheUsersFiles(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	write(t, filepath.Join(dir, "AGENTS.md"), "my agents\n")
	write(t, filepath.Join(dir, "CLAUDE.md"), "my claude\n")
	write(t, filepath.Join(dir, ".claude", "commands", "aimem-analyze.md"), "my own analyze command\n")
	old := "---\n---\n<!-- managed by aimem install.sh — edits here are overwritten on reinstall -->\nold text\n"
	write(t, filepath.Join(dir, ".opencode", "command", "aimem-analyze.md"), old)
	wireOK(t, dir, ProjectOptions{Home: home})
	for rel, want := range map[string]string{"AGENTS.md": "my agents\n", "CLAUDE.md": "my claude\n",
		".claude/commands/aimem-analyze.md": "my own analyze command\n", ".opencode/command/aimem-analyze.md": analyzeCommand} {
		if got := read(t, filepath.Join(dir, rel)); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

func TestWireRefusesBeforeWriting(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	state := filepath.Join(base, "state")
	checkout := filepath.Join(base, "checkout")
	nested := filepath.Join(checkout, "sub")
	plain := filepath.Join(base, "plain")
	for _, d := range []string{home, filepath.Join(state, "inner"), filepath.Join(checkout, ".git"), nested, plain} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(checkout, ".aimem.json"), `{"groups":[]}`)
	// A linked worktree of that checkout, the way git lays one out.
	worktree := filepath.Join(base, "worktree")
	if err := os.MkdirAll(filepath.Join(worktree, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(checkout, ".git", "worktrees", "w", "commondir"), "../..\n")
	write(t, filepath.Join(worktree, ".git"), "gitdir: "+filepath.Join(checkout, ".git", "worktrees", "w")+"\n")
	o := ProjectOptions{Home: home, StateRoot: state}
	for _, c := range []struct {
		name, dir, want string
		o               ProjectOptions
	}{
		{"home", home, "refusing to wire the home directory", o},
		{"state root", state, "inside aimem's state root", o},
		{"inside the state root", filepath.Join(state, "inner"), "inside aimem's state root", o},
		{"inside a wired checkout", nested, "move it out of that checkout and run `aimem wire` in it", o},
		{"inside a linked worktree", filepath.Join(worktree, "sub"), "it is inside the linked worktree", o},
		{"keys in a linked worktree", worktree, "refusing to set the project, hub or groups", ProjectOptions{Home: home, Hub: "lab"}},
		{"bad project", plain, "invalid project name", ProjectOptions{Home: home, Project: "group-x"}},
		{"bad hub", plain, "invalid hub name", ProjectOptions{Home: home, Hub: "Lab Hub"}},
		{"bad group", plain, "invalid group name", ProjectOptions{Home: home, Groups: []string{"ok", "No"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := len(files(t, base))
			_, err := Wire(c.dir, c.o)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Wire(%s) = %v, want %q", c.dir, err, c.want)
			}
			if after := len(files(t, base)); after != before {
				t.Errorf("a refused run wrote %d files", after-before)
			}
		})
	}
}

// A linked worktree of a wired checkout whose wired files are untracked
// gets the client files and no .aimem.json: the main checkout's governs
// it, is named in the output, and is left byte for byte as it was.
func TestWireLinkedWorktree(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not found")
	}
	base, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(gitPath, append([]string{"-c", "user.name=t", "-c", "user.email=t@t.invalid", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	main := filepath.Join(base, "main")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	git(main, "init", "-q")
	write(t, filepath.Join(main, "README"), "x\n")
	git(main, "add", "README")
	git(main, "commit", "-q", "-m", "init")
	// The wired files stay untracked, so a new worktree has none of them.
	wireOK(t, main, ProjectOptions{Home: home, Project: "p1", Hub: "lab"})
	cfg := read(t, filepath.Join(main, ".aimem.json"))
	wt := filepath.Join(base, "wt")
	git(main, "worktree", "add", "-q", wt)

	governed := func(t *testing.T, out []Finding) {
		t.Helper()
		for _, f := range out {
			if f.File == ".aimem.json" {
				if f.Level != "ok" || !strings.Contains(f.Detail, "governed by") || !strings.Contains(f.Detail, filepath.Base(main)) || !strings.Contains(f.Detail, "project: p1, hub: lab") {
					t.Errorf(".aimem.json: %s %s (fix %q)", f.Level, f.Detail, f.Fix)
				}
				return
			}
		}
		t.Error("no .aimem.json finding")
	}
	before := files(t, wt)
	out, err := Wire(wt, ProjectOptions{Home: home, Check: true})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	governed(t, out)
	missing := 0
	for _, f := range out {
		if strings.Contains(f.Fix, "aimem wire") {
			missing++
		}
	}
	if missing == 0 {
		t.Error("the check reports nothing missing in a bare worktree")
	}
	if got := files(t, wt); len(got) != len(before) {
		t.Errorf("the check wrote %d files", len(got)-len(before))
	}

	governed(t, wireOK(t, wt, ProjectOptions{Home: home}))
	if _, err := os.Lstat(filepath.Join(wt, ".aimem.json")); !os.IsNotExist(err) {
		t.Errorf("the worktree got a .aimem.json: %v", err)
	}
	for _, rel := range []string{filepath.Join(".claude", "settings.json"), ".mcp.json", "opencode.json", "AGENTS.md", "CLAUDE.md", filepath.Join(".claude", "commands", "aimem-analyze.md")} {
		if _, err := os.Stat(filepath.Join(wt, rel)); err != nil {
			t.Errorf("%s not written in the worktree: %v", rel, err)
		}
	}
	out, err = Wire(wt, ProjectOptions{Home: home, Check: true})
	if err != nil {
		t.Fatalf("check after wiring: %v", err)
	}
	governed(t, out)
	for _, f := range out {
		if f.Fix != "" {
			t.Errorf("still missing after wiring: %s: %s (%s)", f.File, f.Detail, f.Fix)
		}
	}
	if got := read(t, filepath.Join(main, ".aimem.json")); got != cfg {
		t.Errorf("the main checkout's .aimem.json changed:\n%s\nwas\n%s", got, cfg)
	}
}

// A rewrite keeps a user's & and > as written, not as JSON escapes.
func TestWireKeepsShellOperatorsReadable(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	settings := filepath.Join(dir, ".claude", "settings.json")
	write(t, settings, `{"hooks":{"PostToolUse":[{"hooks":[{"type":"command","command":"lint && fmt >/dev/null 2>&1"}]}]}}`)
	wireOK(t, dir, ProjectOptions{Home: home})
	got := read(t, settings)
	if !strings.Contains(got, `"lint && fmt >/dev/null 2>&1"`) || strings.Contains(got, `\u00`) {
		t.Fatalf("settings.json lost its literal operators:\n%s", got)
	}
}
