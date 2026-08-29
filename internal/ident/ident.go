// Package ident derives the stable, filesystem-safe project identity used to
// key per-project journals: an explicit name pinned in .aimem.json when set,
// else normalized git remote URL when present, else the repository root
// commit, else the absolute path — derived sources suffixed with a short
// content hash so distinct sources never collide after slugging.
package ident

import (
	"crypto/sha256"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var nonSlug = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// pinnedName validates an explicit project id from .aimem.json. It is used
// verbatim (no hash suffix) — that is the point: the same name on every
// machine and checkout dir means one shared journal and memory. Reserved
// ids (the user DB, group DBs) are refused.
var pinnedName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ProjectID computes the project identity for a directory.
func ProjectID(dir string) (string, error) {
	if pin, err := projectPin(dir); err != nil {
		return "", err
	} else if pin != "" {
		return pin, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	var source, label string
	if out, err := gitOut(abs, "remote", "get-url", "origin"); err == nil && out != "" {
		source = NormalizeRemote(out)
		label = source
	} else if out, err := gitOut(abs, "rev-list", "--max-parents=0", "--all", "--max-count=1"); err == nil && out != "" {
		source = "root-commit:" + out
		label = out[:min(12, len(out))]
	} else {
		source = "path:" + abs
		label = filepath.Base(abs)
	}
	sum := sha256.Sum256([]byte(source))
	slug := strings.Trim(nonSlug.ReplaceAllString(label, "-"), "-.")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	if slug == "" {
		slug = "project"
	}
	return slug + "-" + hex.EncodeToString(sum[:6]), nil
}

func gitOut(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// GitBranch returns the current branch of dir, or "".
func GitBranch(dir string) string {
	out, _ := gitOut(dir, "rev-parse", "--abbrev-ref", "HEAD")
	return out
}

// NormalizeRemote canonicalizes a git remote URL so HTTPS/SSH forms of the
// same repository produce the same identity.
func NormalizeRemote(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimSuffix(u, ".git")
	if after, ok := strings.CutPrefix(u, "git@"); ok {
		u = strings.Replace(after, ":", "/", 1)
	}
	for _, p := range []string{"https://", "http://", "ssh://", "git://"} {
		u = strings.TrimPrefix(u, p)
	}
	if i := strings.Index(u, "@"); i >= 0 {
		u = u[i+1:]
	}
	return strings.ToLower(u)
}
