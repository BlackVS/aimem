// Package confine reads and writes files inside a checkout without
// following a link out of it.
//
// A repository can carry a symbolic link (or, on Windows, a junction) where
// a file the tools read or write is expected: a bound shared document, a
// client's settings file, a skills directory. The operating system follows
// such a link silently, so a plain os.WriteFile on "<checkout>/docs/x.md"
// can land anywhere the link points, and a plain os.ReadFile can publish a
// file from anywhere. Every checkout-scoped read and write therefore goes
// through Check, which walks each path component below the checkout root
// and refuses one that redirects elsewhere. A link that happens to point
// inside the checkout is refused too: the rule is "no link below the
// root", not "no link that leaves it", because resolving where a link
// leads is exactly what Windows junctions defeat (Go reports a junction as
// an irregular entry and filepath.EvalSymlinks fails through it).
//
// The root itself may be a link: checkouts under a linked home or work
// directory are normal. A concurrent local process that swaps a component
// for a link between the check and the write is out of scope; the threat is
// a link committed to the repository.
package confine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrLink marks a refusal: some component of the path is a link.
var ErrLink = errors.New("a symbolic link or junction")

// ErrOutside marks a path that does not lie under the root at all; a caller
// that also accepts paths outside the checkout tells it apart from ErrLink.
var ErrOutside = errors.New("not inside the root")

// LinkError names the path that was refused and the component that is a
// link. It matches ErrLink with errors.Is.
type LinkError struct {
	Path      string // the file that was to be read or written
	Component string // the first redirecting component, relative to the root
	Final     bool   // the component is the file itself
}

func (e *LinkError) Error() string {
	if e.Final {
		return fmt.Sprintf("refusing %s: it is a symbolic link or junction (files in a checkout are never read or written through a link)", e.Component)
	}
	return fmt.Sprintf("refusing %s: %s is a symbolic link or junction (files in a checkout are never read or written through a link)", e.Path, e.Component)
}

func (e *LinkError) Unwrap() error { return ErrLink }

// Check reports whether path, which must lie under root, can be read or
// written without passing through a link below root. Components that do
// not exist yet end the walk: whatever creates them creates real entries.
//
// A path inside the root spelled with a ".." element is refused. The walk
// checks the cleaned path, but the operating system resolves "a/link/../b"
// by following the link first and then its parent, which can be anywhere;
// the cleaned "a/b" says nothing about that. Paths built with
// filepath.Join are already clean, so only raw, caller-supplied spellings
// meet this refusal. The root's own spelling, when path starts with it,
// is not examined: only what lies below it can hold a link.
func Check(root, path string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return fmt.Errorf("%s is not inside %s: %w (%v)", path, root, ErrOutside, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("refusing %s: it is %w %s", path, ErrOutside, root)
	}
	below := path
	if r := filepath.Clean(root); r != "." {
		below = strings.TrimPrefix(path, r)
	}
	for _, el := range strings.Split(filepath.ToSlash(below), "/") {
		if el == ".." {
			return fmt.Errorf("refusing %s: a path inside the checkout must not contain \"..\"", path)
		}
	}
	if rel == "." {
		return nil
	}
	cur := absRoot
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if redirects(cur, fi) {
			comp, _ := filepath.Rel(absRoot, cur)
			return &LinkError{Path: path, Component: filepath.ToSlash(comp), Final: cur == absPath}
		}
	}
	return nil
}

// redirects reports whether the entry at p stands for another location.
// A symbolic link always does. On Windows, Go reports any reparse point it
// does not model as ModeIrregular: a junction redirects, but an ordinary
// file or folder under a cloud-sync or similar filter does not, so an
// irregular entry is refused only when following it leads to a different
// file than the entry itself.
func redirects(p string, fi os.FileInfo) bool {
	m := fi.Mode()
	if m&os.ModeSymlink != 0 {
		return true
	}
	return m&os.ModeIrregular != 0 && leadsElsewhere(p, fi)
}

// leadsElsewhere reports whether following the entry at p reaches a
// different file than the entry itself, or cannot be followed at all.
func leadsElsewhere(p string, fi os.FileInfo) bool {
	st, err := os.Stat(p)
	return err != nil || !os.SameFile(fi, st)
}

// ReadFile is os.ReadFile after Check.
func ReadFile(root, path string) ([]byte, error) {
	if err := Check(root, path); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// WriteFile replaces path with data after Check, creating missing parent
// directories. The data goes to a temporary file with an unpredictable
// name in the target directory, which is then renamed over the target, so
// a reader never sees a half-written file and no fixed temporary name can
// be pre-planted.
func WriteFile(root, path string, data []byte, perm os.FileMode) error {
	if err := Check(root, path); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// MkdirAll is os.MkdirAll after Check.
func MkdirAll(root, path string, perm os.FileMode) error {
	if err := Check(root, path); err != nil {
		return err
	}
	return os.MkdirAll(path, perm)
}
