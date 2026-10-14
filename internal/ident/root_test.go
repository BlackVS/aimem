package ident

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// linkWorktree lays out what `git worktree add` writes: the worktree's .git
// file naming its git dir under the main checkout, and that dir's commondir
// file naming the shared git dir, as relative or absolute paths.
func linkWorktree(t *testing.T, main, wt string, relative bool) {
	t.Helper()
	gitDir := mkdir(t, main, ".git", "worktrees", filepath.Base(wt))
	ref := filepath.ToSlash(gitDir)
	if relative {
		rel, err := filepath.Rel(wt, gitDir)
		if err != nil {
			t.Fatal(err)
		}
		ref = filepath.ToSlash(rel)
	}
	writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+ref+"\n")
	writeFile(t, filepath.Join(gitDir, "commondir"), "../..\n")
}

func wantProject(t *testing.T, dir, want string) {
	t.Helper()
	if id, err := ProjectID(dir); err != nil || id != want {
		t.Errorf("ProjectID(%s) = %q, %v; want %q", dir, id, err, want)
	}
}

func TestConfigDirResolution(t *testing.T) {
	base := t.TempDir()
	main := mkdir(t, base, "main")
	mkdir(t, main, ".git")
	writeConfig(t, main, `{"project": "pinned", "hub": "named", "groups": ["fw"]}`)
	deep := mkdir(t, main, "internal", "pkg")

	// The checkout root and any subdirectory share the pin.
	wantProject(t, main, "pinned")
	wantProject(t, deep, "pinned")
	if d, via := ConfigDir(deep); d != main || via {
		t.Errorf("ConfigDir(subdir) = %q, %v; want %q, false", d, via, main)
	}

	// A linked worktree outside the checkout (absolute gitdir) and one
	// inside it (relative gitdir, as `git worktree add --relative-paths`
	// writes) both resolve through the main checkout, from any depth.
	for _, tc := range []struct {
		wt       string
		relative bool
	}{
		{filepath.Join(base, "wt-abs"), false},
		{filepath.Join(main, ".work", "wt-rel"), true},
	} {
		mkdir(t, tc.wt)
		linkWorktree(t, main, tc.wt, tc.relative)
		sub := mkdir(t, tc.wt, "internal", "pkg")
		for _, d := range []string{tc.wt, sub} {
			wantProject(t, d, "pinned")
			if got, via := ConfigDir(d); got != main || !via {
				t.Errorf("ConfigDir(%s) = %q, %v; want %q, true", d, got, via, main)
			}
			// The hub binding and groups travel with the pin: data under
			// the pinned id never goes to the default hub.
			if h, err := ProjectHubName(d); err != nil || h != "named" {
				t.Errorf("ProjectHubName(%s) = %q, %v", d, h, err)
			}
			if g, err := ProjectGroups(d); err != nil || len(g) != 1 || g[0] != "group-fw" {
				t.Errorf("ProjectGroups(%s) = %v, %v", d, g, err)
			}
		}
	}

	// A worktree with its own .aimem.json keeps its own pin.
	own := mkdir(t, base, "wt-own")
	linkWorktree(t, main, own, false)
	writeConfig(t, own, `{"project": "own"}`)
	wantProject(t, own, "own")
	wantProject(t, mkdir(t, own, "sub"), "own")

	// A nested checkout is its own project: with its own file it keeps its
	// pin, and without one the walk stops at its top instead of borrowing
	// the enclosing checkout's.
	nested := mkdir(t, main, "vendor", "nested")
	mkdir(t, nested, ".git")
	writeConfig(t, nested, `{"project": "nested"}`)
	wantProject(t, mkdir(t, nested, "sub"), "nested")
	bare := mkdir(t, main, "vendor", "bare")
	mkdir(t, bare, ".git")
	inner := mkdir(t, bare, "sub")
	if d, via := ConfigDir(inner); d != inner || via {
		t.Errorf("ConfigDir(nested without config) = %q, %v; want unchanged", d, via)
	}
	if id, err := ProjectID(inner); err != nil || id == "pinned" {
		t.Errorf("nested checkout without config borrowed the enclosing pin: %q, %v", id, err)
	}

	// A submodule's .git file names a git dir with no commondir file: it
	// has no main checkout to read.
	sm := mkdir(t, base, "submodule")
	smGit := mkdir(t, main, ".git", "modules", "sm")
	writeFile(t, filepath.Join(sm, ".git"), "gitdir: "+filepath.ToSlash(smGit)+"\n")
	if d, via := ConfigDir(sm); d != sm || via {
		t.Errorf("ConfigDir(submodule) = %q, %v; want unchanged", d, via)
	}
}

// Outside any checkout nothing changes: an enclosing directory's
// .aimem.json is not borrowed, and the id stays path-derived.
func TestConfigDirOutsideCheckout(t *testing.T) {
	outer := t.TempDir()
	writeConfig(t, outer, `{"project": "outer"}`)
	child := mkdir(t, outer, "child")
	if d, via := ConfigDir(child); d != child || via {
		t.Errorf("ConfigDir outside a checkout = %q, %v; want unchanged", d, via)
	}
	id, err := ProjectID(child)
	if err != nil || id == "outer" || id == "" {
		t.Errorf("ProjectID outside a checkout = %q, %v", id, err)
	}
}

// The layout git itself writes: a real `git worktree add` resolves through
// the main checkout's .aimem.json, without git on the resolution path.
func TestConfigDirRealGitWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	main := mkdir(t, base, "main")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git(main, "init", "-q")
	writeFile(t, filepath.Join(main, "README"), "x\n")
	git(main, "add", "README")
	git(main, "commit", "-q", "-m", "init")
	writeConfig(t, main, `{"project": "pinned"}`)
	wt := filepath.Join(base, "wt")
	git(main, "worktree", "add", "-q", wt)
	wantProject(t, wt, "pinned")
	wantProject(t, mkdir(t, wt, "sub"), "pinned")
	if d, via := ConfigDir(wt); !via || !sameDir(t, d, main) {
		t.Errorf("ConfigDir(real worktree) = %q, %v; want %q, true", d, via, main)
	}
}

func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}
