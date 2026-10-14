package ident

// Project root resolution: which .aimem.json governs a directory. A
// session's hooks, MCP server and CLI calls run wherever the agent stands:
// the checkout root, a subdirectory of it, or a linked git worktree, which
// has no copy of the (usually gitignored) .aimem.json. Reading the file
// only in the given directory journaled such sessions under a derived twin
// id on the default hub, silently, while the pinned project went stale.
//
// The rule, read from the filesystem alone (hooks never needed a git
// binary for a pinned project, and still do not):
//
//  1. The nearest .aimem.json in dir or an ancestor, up to the top of the
//     checkout dir belongs to (the first directory holding a .git entry).
//     A nested checkout is its own project: the walk never leaves it.
//  2. None there, and that top is a linked worktree: the main checkout's
//     .aimem.json, found through the worktree's gitdir and commondir files
//     (what git rev-parse --git-common-dir reports).
//  3. Otherwise dir itself. Outside any checkout nothing changes, and a
//     checkout without the file keeps its derived id.

import (
	"os"
	"path/filepath"
	"strings"
)

// ConfigDir returns the directory whose .aimem.json governs dir, and
// whether it was found through a linked worktree's main checkout. When no
// file governs dir, it returns dir unchanged and false.
func ConfigDir(dir string) (string, bool) {
	if hasConfig(dir) {
		return dir, false
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir, false
	}
	// The nearest file counts only once the checkout's top is found at or
	// above it: an enclosing directory outside any checkout governs nothing.
	nearest := ""
	for d := abs; ; {
		if nearest == "" && hasConfig(d) {
			nearest = d
		}
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			if nearest != "" {
				return nearest, false
			}
			if main := mainCheckout(d); main != "" && hasConfig(main) {
				return main, true
			}
			return dir, false
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir, false // no checkout above dir: unchanged
		}
		d = parent
	}
}

// CheckoutDir returns the top of the checkout dir belongs to (the first
// directory at or above it holding a .git entry), or "" outside any
// checkout. A linked worktree's top is the worktree, not its main checkout.
func CheckoutDir(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for d := abs; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return ""
		}
		d = parent
	}
}

// ProjectDir returns the directory a project's configuration and bound
// files are read from for dir: the directory holding the governing
// .aimem.json, or dir itself when none governs it.
func ProjectDir(dir string) string {
	d, _ := ConfigDir(dir)
	return d
}

func hasConfig(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, ".aimem.json"))
	return err == nil && !fi.IsDir()
}

// mainCheckout returns the main working tree of the linked worktree whose
// top is top, or "" when top is not one. A linked worktree's .git is a file
// naming its git dir (.git/worktrees/<name> in the main checkout), and that
// dir holds a commondir file naming the shared git dir. A submodule's git
// dir has no commondir file, and a bare repository's common dir is not
// named .git; neither has a main checkout to read.
func mainCheckout(top string) string {
	raw, err := os.ReadFile(filepath.Join(top, ".git"))
	if err != nil {
		return "" // a directory (an ordinary checkout) or unreadable
	}
	gitDir, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return ""
	}
	gitDir = resolveFrom(top, strings.TrimSpace(gitDir))
	raw, err = os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return ""
	}
	common := resolveFrom(gitDir, strings.TrimSpace(string(raw)))
	if filepath.Base(common) != ".git" {
		return ""
	}
	if main := filepath.Dir(common); main != top {
		return main
	}
	return ""
}

func resolveFrom(base, p string) string {
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p)
}
