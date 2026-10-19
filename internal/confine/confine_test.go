package confine_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/confine"
	"aimem/internal/confine/linktest"
)

// layout makes a checkout and a sibling "outside" directory holding one
// file, the shape of the audit's reproducer.
func layout(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "checkout")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside, filepath.Join(root, "docs")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

func TestPlainPathsReadAndWrite(t *testing.T) {
	root, _ := layout(t)
	p := filepath.Join(root, "docs", "new", "RUNBOOK.md")
	if err := confine.WriteFile(root, p, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := confine.WriteFile(root, p, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := confine.ReadFile(root, p); err != nil || string(b) != "two" {
		t.Fatalf("read back %q, %v", b, err)
	}
	// The temporary file is gone after the rename.
	if es := entries(t, filepath.Dir(p)); len(es) != 1 {
		t.Fatalf("leftover files beside the target: %v", es)
	}
}

func TestALinkedDirectoryIsRefused(t *testing.T) {
	root, outside := layout(t)
	linktest.Dir(t, filepath.Join(root, "linked"), outside)
	p := filepath.Join(root, "linked", "RUNBOOK.md")
	err := confine.WriteFile(root, p, []byte("x"), 0o644)
	var le *confine.LinkError
	if !errors.As(err, &le) || !errors.Is(err, confine.ErrLink) || le.Component != "linked" {
		t.Fatalf("write through a linked directory: %v", err)
	}
	if _, err := confine.ReadFile(root, filepath.Join(root, "linked", "secret.txt")); !errors.Is(err, confine.ErrLink) {
		t.Fatalf("read through a linked directory: %v", err)
	}
	if es := entries(t, outside); len(es) != 1 || es[0] != "secret.txt" {
		t.Fatalf("something was written outside the checkout: %v", es)
	}
	if !strings.Contains(err.Error(), "linked") {
		t.Fatalf("the refusal does not name the link: %v", err)
	}
}

// A link that stays inside the checkout is refused as well: the rule is
// "no link below the root".
func TestALinkInsideTheCheckoutIsRefusedToo(t *testing.T) {
	root, _ := layout(t)
	linktest.Dir(t, filepath.Join(root, "alias"), filepath.Join(root, "docs"))
	if err := confine.WriteFile(root, filepath.Join(root, "alias", "x.md"), []byte("x"), 0o644); !errors.Is(err, confine.ErrLink) {
		t.Fatalf("write through an inside link: %v", err)
	}
}

func TestALinkedFileIsRefused(t *testing.T) {
	root, outside := layout(t)
	link := filepath.Join(root, "docs", "SESSION-STATE.md")
	linktest.File(t, link, filepath.Join(outside, "secret.txt"))
	if _, err := confine.ReadFile(root, link); !errors.Is(err, confine.ErrLink) {
		t.Fatalf("read of a linked file: %v", err)
	}
	if err := confine.WriteFile(root, link, []byte("x"), 0o644); !errors.Is(err, confine.ErrLink) {
		t.Fatalf("write of a linked file: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); string(b) != "outside" {
		t.Fatalf("the link target changed: %q", b)
	}
}

// The checkout root itself may be a link.
func TestTheRootMayBeALink(t *testing.T) {
	root, _ := layout(t)
	alias := filepath.Join(filepath.Dir(root), "checkout-alias")
	linktest.Dir(t, alias, root)
	p := filepath.Join(alias, "docs", "x.md")
	if err := confine.WriteFile(alias, p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "docs", "x.md")); err != nil || string(b) != "x" {
		t.Fatalf("write through a linked root: %q, %v", b, err)
	}
}

func TestAPathOutsideTheRootIsRefused(t *testing.T) {
	root, outside := layout(t)
	if err := confine.WriteFile(root, filepath.Join(outside, "x"), []byte("x"), 0o644); err == nil {
		t.Fatal("a path outside the root was written")
	}
	if err := confine.WriteFile(root, filepath.Join(root, "..", "outside", "x"), []byte("x"), 0o644); err == nil {
		t.Fatal("a .. path out of the root was written")
	}
}

// A raw spelling with ".." after a link cleans to a path inside the
// checkout, but the operating system follows the link before the "..".
// Such a spelling is refused whether or not a link is there.
func TestARawDotDotSpellingIsRefused(t *testing.T) {
	root, outside := layout(t)
	deep := filepath.Join(outside, "deep")
	os.MkdirAll(deep, 0o755)
	linktest.Dir(t, filepath.Join(root, "docs", "jump"), deep)
	raw := root + string(filepath.Separator) + filepath.Join("docs", "jump") + string(filepath.Separator) + ".." + string(filepath.Separator) + "secret.txt"
	if _, err := confine.ReadFile(root, raw); err == nil || !strings.Contains(err.Error(), "..") {
		t.Fatalf("a raw link/.. read was not refused: %v", err)
	}
	if err := confine.WriteFile(root, raw, []byte("x"), 0o644); err == nil {
		t.Fatal("a raw link/.. write was not refused")
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "secret.txt")); string(b) != "outside" {
		t.Fatalf("the file outside changed: %q", b)
	}
	if err := confine.Check(root, "docs/../docs/x.md"); err == nil {
		t.Fatal("a relative .. spelling was not refused")
	}
}

// A relative root spelled with ".." still works for paths joined under it.
func TestARootSpelledWithDotDotIsAllowed(t *testing.T) {
	root, _ := layout(t)
	t.Chdir(filepath.Join(root, "docs"))
	if err := confine.WriteFile("..", filepath.Join("..", "docs", "x.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "docs", "x.md")); string(b) != "x" {
		t.Fatalf("not written: %q", b)
	}
}
