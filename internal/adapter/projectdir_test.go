package adapter

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// pinnedWorktree lays out a pinned main checkout bound to a named hub, a
// subdirectory of it and a linked worktree with no .aimem.json of its own,
// as `git worktree add` leaves one when the file is gitignored.
func pinnedWorktree(t *testing.T) (main, sub, wt string) {
	t.Helper()
	base := t.TempDir()
	main = filepath.Join(base, "main")
	sub = filepath.Join(main, "internal", "pkg")
	wt = filepath.Join(base, "wt")
	gitDir := filepath.Join(main, ".git", "worktrees", "wt")
	for _, d := range []string{sub, gitDir, filepath.Join(wt, "cmd")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(main, ".aimem.json"): `{"project":"pinned","hub":"named","groups":["fw"]}`,
		filepath.Join(wt, ".git"):          "gitdir: " + filepath.ToSlash(gitDir) + "\n",
		filepath.Join(gitDir, "commondir"): "../..\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return main, sub, wt
}

// Every capture path files a session standing in a subdirectory or a
// linked worktree under the pinned project, on its bound hub, with the
// main checkout as the directory its bound files are read from.
func TestCaptureResolvesThroughGoverningConfig(t *testing.T) {
	main, sub, wt := pinnedWorktree(t)
	tp := writeTranscript(t, []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"fix it"}}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	})
	check := func(name string, p *Payload) {
		t.Helper()
		// A hook's own payload is already resolved, before the submit
		// path's ResolveProjectID runs.
		if p.ProjectID != "" && p.ProjectDir != main {
			t.Errorf("%s: built payload dir %q; want %q", name, p.ProjectDir, main)
		}
		if err := p.ResolveProjectID(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if p.ProjectID != "pinned" || p.ProjectDir != main {
			t.Errorf("%s: project %q dir %q; want pinned in %q", name, p.ProjectID, p.ProjectDir, main)
		}
		if p.Hub == nil || *p.Hub != "named" {
			t.Errorf("%s: hub %v; want the bound hub", name, p.Hub)
		}
		if p.Groups == nil || len(*p.Groups) != 1 || (*p.Groups)[0] != "group-fw" {
			t.Errorf("%s: groups %v", name, p.Groups)
		}
	}
	for _, cwd := range []string{sub, wt, filepath.Join(wt, "cmd")} {
		raw, _ := json.Marshal(map[string]string{
			"session_id": "s", "transcript_path": tp, "cwd": cwd, "hook_event_name": "Stop",
		})
		p, err := BuildClaudeEvent(raw)
		if err != nil {
			t.Fatal(err)
		}
		check("claude "+cwd, p)
		check("codex "+cwd, buildCodex(t, map[string]any{
			"session_id": "s", "turn_id": "t", "cwd": cwd, "hook_event_name": "Stop",
			"last_assistant_message": "done",
		}))
		// The OpenCode plugin sends its launch directory as project_dir
		// and the service resolves the rest.
		check("project_dir "+cwd, &Payload{ProjectDir: cwd})
	}
}

// The event's branch is the one checked out where the session stands: a
// worktree on its own branch records that branch, not the main checkout's.
func TestWorktreeEventKeepsItsBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	main := filepath.Join(base, "main")
	wt := filepath.Join(base, "wt")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	git(main, "init", "-q", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(main, "README"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(main, "add", "README")
	git(main, "commit", "-q", "-m", "init")
	if err := os.WriteFile(filepath.Join(main, ".aimem.json"), []byte(`{"project":"pinned"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	git(main, "worktree", "add", "-q", "-b", "feature", wt)
	tp := writeTranscript(t, []string{
		`{"type":"user","uuid":"u1","message":{"role":"user","content":"fix it"}}`,
		`{"type":"assistant","uuid":"a1","message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`,
	})
	raw, _ := json.Marshal(map[string]string{"session_id": "s", "transcript_path": tp, "cwd": wt, "hook_event_name": "Stop"})
	p, err := BuildClaudeEvent(raw)
	if err != nil {
		t.Fatal(err)
	}
	cp := buildCodex(t, map[string]any{"session_id": "s", "turn_id": "t", "cwd": wt, "hook_event_name": "Stop", "last_assistant_message": "done"})
	for name, p := range map[string]*Payload{"claude": p, "codex": cp} {
		if p.ProjectID != "pinned" || p.Event.GitBranch != "feature" {
			t.Errorf("%s: project %q branch %q; want pinned on feature", name, p.ProjectID, p.Event.GitBranch)
		}
	}
}
