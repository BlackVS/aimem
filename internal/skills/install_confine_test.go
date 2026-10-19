package skills

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aimem/internal/confine"
	"aimem/internal/confine/linktest"
)

// A project install never writes, replaces or removes anything through a
// link below the checkout; a user-level directory reached through a link
// (linked dotfiles) installs as before.
func TestInstallRefusesALinkBelowTheCheckout(t *testing.T) {
	files := releaseFiles()
	digest := (&Set{Files: files}).Digest()
	read := func(string) (map[string][]byte, error) { return files, nil }
	picks := []Skill{{Name: "verify-delivery", Version: "1.33.13", Source: SourceRelease, Digest: digest}}
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)

	setup := func(t *testing.T) (proj, outside string) {
		base := t.TempDir()
		proj, outside = filepath.Join(base, "checkout"), filepath.Join(base, "outside")
		os.MkdirAll(proj, 0o755)
		os.MkdirAll(filepath.Join(outside, "skills", "verify-delivery"), 0o755)
		os.WriteFile(filepath.Join(outside, "skills", "verify-delivery", "mine.txt"), []byte("outside"), 0o644)
		return proj, outside
	}
	untouched := func(t *testing.T, outside string) {
		t.Helper()
		if b, _ := os.ReadFile(filepath.Join(outside, "skills", "verify-delivery", "mine.txt")); string(b) != "outside" {
			t.Fatalf("content behind the link was replaced or removed: %q", b)
		}
		if es, _ := os.ReadDir(filepath.Join(outside, "skills")); len(es) != 1 {
			t.Fatalf("files appeared outside the checkout: %v", es)
		}
	}

	t.Run("linked client directory", func(t *testing.T) {
		proj, outside := setup(t)
		linktest.Dir(t, filepath.Join(proj, ".claude"), outside)
		dir := filepath.Join(proj, ".claude", "skills")
		_, err := Install([]Target{{Client: "claude", Dir: dir, Root: proj}}, picks, read, now)
		if !errors.Is(err, confine.ErrLink) {
			t.Fatalf("install through a linked .claude: %v", err)
		}
		untouched(t, outside)
	})
	t.Run("linked skill directory", func(t *testing.T) {
		proj, outside := setup(t)
		dir := filepath.Join(proj, ".claude", "skills")
		os.MkdirAll(dir, 0o755)
		linktest.Dir(t, filepath.Join(dir, "verify-delivery"), filepath.Join(outside, "skills", "verify-delivery"))
		_, err := Install([]Target{{Client: "claude", Dir: dir, Root: proj}}, picks, read, now)
		if !errors.Is(err, confine.ErrLink) {
			t.Fatalf("install over a linked skill directory: %v", err)
		}
		untouched(t, outside)
	})
	t.Run("user-level directory through a link", func(t *testing.T) {
		home, outside := setup(t)
		linktest.Dir(t, filepath.Join(home, ".claude"), outside)
		dir := filepath.Join(home, ".claude", "skills")
		if _, err := Install([]Target{{Client: "claude", Dir: dir}}, picks, read, now); err != nil {
			t.Fatalf("a user-level install through a linked home directory: %v", err)
		}
		if _, err := os.Stat(filepath.Join(outside, "skills", "verify-delivery", "verify_delivery.py")); err != nil {
			t.Fatal("the user-level install did not land")
		}
	})
}
