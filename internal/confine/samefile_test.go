package confine

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"aimem/internal/confine/linktest"
)

// Both sides of the irregular-entry rule. Go on Windows reports a junction
// and any other reparse point it does not model (a cloud-sync placeholder,
// say) as ModeIrregular; only the ones that lead elsewhere are refused.
// leadsElsewhere is the deciding check, exercised here on a junction (or a
// symbolic link elsewhere) and on a plain directory and file.
func TestOnlyAnEntryThatLeadsElsewhereIsRefused(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "plain")
	file := filepath.Join(dir, "f.md")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	linktest.Dir(t, link, dir)

	for _, p := range []string{dir, file} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		if leadsElsewhere(p, fi) {
			t.Errorf("%s: a plain entry reads as leading elsewhere", p)
		}
		if redirects(p, fi) {
			t.Errorf("%s: a plain entry is refused", p)
		}
	}

	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !leadsElsewhere(link, fi) {
		t.Errorf("the directory link does not read as leading elsewhere")
	}
	if !redirects(link, fi) {
		t.Errorf("the directory link is not refused (mode %v)", fi.Mode())
	}
	// On Windows the junction must reach the refusal through the
	// irregular-entry path, not the symbolic-link one, or this test would not
	// cover the SameFile rule there.
	if runtime.GOOS == "windows" && fi.Mode()&os.ModeIrregular == 0 {
		t.Errorf("this Go no longer reports a junction as irregular (mode %v): revisit redirects", fi.Mode())
	}

	// A dangling link cannot be followed; it is refused too.
	gone := filepath.Join(base, "gone")
	if err := os.MkdirAll(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(base, "dangling")
	linktest.Dir(t, dangling, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(dangling); err != nil || !redirects(dangling, fi) {
		t.Errorf("a dangling directory link is not refused: %v", err)
	}
}
