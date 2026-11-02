package wiring

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestProjectFilesAreWhatWireWrites holds ProjectFiles to Wire: an empty
// directory, wired, holds exactly the listed paths.
func TestProjectFilesAreWhatWireWrites(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	wireOK(t, dir, ProjectOptions{Home: home})
	var got, want []string
	for p := range files(t, dir) {
		got = append(got, p)
	}
	for _, f := range ProjectFiles {
		want = append(want, f.Path)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("Wire wrote %q,\nProjectFiles lists %q", got, want)
	}
}

// TestGitignoreLinesKeepTheLocalFiles asks Git which paths the
// GitignoreLines ignore: every local ProjectFile and every skill
// `aimem skills install` puts in place, and no committed ProjectFile.
func TestGitignoreLinesKeepTheLocalFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir := t.TempDir()
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	if out, err := git("init", "-q"); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(strings.Join(GitignoreLines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ignored := map[string]bool{
		"docs/SESSION-STATE-ARCHIVE.md":             true,
		".claude/settings.local.json":               true,
		".claude/skills/oh-code-review/SKILL.md":    true,
		".claude/skills/.ai-skills.json":            true,
		".agents/skills/forge-gates/SKILL.md":       true,
		".agents/skills/.ai-skills.json":            true,
		".opencode/skills/verify-delivery/SKILL.md": true,
		".openhands/skills/oh-code-review/SKILL.md": true,
	}
	for _, f := range ProjectFiles {
		ignored[f.Path] = f.Local
	}
	for p, want := range ignored {
		// check-ignore exits 0 when the path is ignored, 1 when it is not.
		out, err := git("check-ignore", "-q", "--no-index", p)
		got := err == nil
		if ee, ok := err.(*exec.ExitError); err != nil && (!ok || ee.ExitCode() != 1) {
			t.Fatalf("git check-ignore %s: %v\n%s", p, err, out)
		}
		if got != want {
			t.Errorf("%s: ignored %v, want %v", p, got, want)
		}
	}
}

func TestWiredLineNamesTheLocalFiles(t *testing.T) {
	const want = "project wired. If the project is tracked, commit the files above except .aimem.json and docs/SESSION-STATE.md, which stay local (the .gitignore lines: docs/QUICKSTART-NEW-PROJECT.md, step 3.5)."
	if got := WiredLine(); got != want {
		t.Errorf("WiredLine() = %q, want %q", got, want)
	}
}
