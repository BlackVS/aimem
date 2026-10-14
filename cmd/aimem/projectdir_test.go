package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pinnedCheckout lays out a pinned main checkout with a handoff and a
// linked worktree with no .aimem.json of its own (gitignored files are not
// checked out into a worktree).
func pinnedCheckout(t *testing.T) (main, wt string) {
	t.Helper()
	base := t.TempDir()
	main = filepath.Join(base, "main")
	wt = filepath.Join(base, "wt")
	gitDir := filepath.Join(main, ".git", "worktrees", "wt")
	for _, d := range []string{filepath.Join(main, "docs"), gitDir, filepath.Join(wt, "internal")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(main, ".aimem.json"):              `{"project":"pinned"}`,
		filepath.Join(main, "docs", "SESSION-STATE.md"): "MAIN CHECKOUT HANDOFF\n",
		filepath.Join(wt, ".git"):                       "gitdir: " + filepath.ToSlash(gitDir) + "\n",
		filepath.Join(gitDir, "commondir"):              "../..\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return main, wt
}

func TestProjectIDAndSessionStartFromWorktree(t *testing.T) {
	main, wt := pinnedCheckout(t)
	for _, dir := range []string{main, filepath.Join(main, "docs"), wt, filepath.Join(wt, "internal")} {
		out, errOut, err := runAimem(t, dir, t.TempDir(), "", "project-id")
		if err != nil || strings.TrimSpace(out) != "pinned" {
			t.Errorf("project-id in %s: %q %v %s", dir, out, err, errOut)
		}
	}
	if out, _, err := runAimem(t, t.TempDir(), t.TempDir(), "", "project-id", filepath.Join(wt, "internal")); err != nil || strings.TrimSpace(out) != "pinned" {
		t.Errorf("project-id DIR: %q %v", out, err)
	}

	// A session launched in the worktree or a subdirectory gets the
	// project's handoff; a checkout's own handoff file still comes first.
	for _, dir := range []string{wt, filepath.Join(wt, "internal"), filepath.Join(main, "docs")} {
		if ctx := sessionStartContext(t, dir, nil); !strings.Contains(ctx, "MAIN CHECKOUT HANDOFF") {
			t.Errorf("session-start in %s missed the project's handoff: %q", dir, ctx)
		}
	}
	if err := os.MkdirAll(filepath.Join(wt, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "docs", "SESSION-STATE.md"), []byte("WORKTREE HANDOFF\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ctx := sessionStartContext(t, wt, nil); !strings.Contains(ctx, "WORKTREE HANDOFF") || strings.Contains(ctx, "MAIN CHECKOUT HANDOFF") {
		t.Errorf("session-start ignored the worktree's own handoff: %q", ctx)
	}
}
