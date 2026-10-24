package wiring

import (
	"io/fs"
	"os"
	"path/filepath"
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
	o := ProjectOptions{Home: home, StateRoot: state}
	for _, c := range []struct {
		name, dir, want string
		o               ProjectOptions
	}{
		{"home", home, "refusing to wire the home directory", o},
		{"state root", state, "inside aimem's state root", o},
		{"inside the state root", filepath.Join(state, "inner"), "inside aimem's state root", o},
		{"inside a wired checkout", nested, "inside the wired checkout", o},
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
