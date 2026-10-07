package installer

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const repoRoot = "../.."

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// newestRelease is the newest released version in the CHANGELOG, as a tag.
func newestRelease(t *testing.T) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`).FindStringSubmatch(read(t, "CHANGELOG.md"))
	if m == nil {
		t.Fatal("CHANGELOG.md has no released version heading")
	}
	return "v" + m[1]
}

// Every boot script installs the release named in its RELEASE line, and
// that is the newest release in the CHANGELOG: rolling the CHANGELOG into
// a version at release time without bumping the pins fails here.
func TestBootScriptsPinTheNewestRelease(t *testing.T) {
	want := newestRelease(t)
	pins := map[string]*regexp.Regexp{
		"boot.sh":        regexp.MustCompile(`(?m)^RELEASE=(\S+)$`),
		"install-hub.sh": regexp.MustCompile(`(?m)^RELEASE=(\S+)$`),
		"boot.ps1":       regexp.MustCompile(`(?m)^\$release = '([^']+)'$`),
	}
	for file, re := range pins {
		m := re.FindAllStringSubmatch(read(t, file), -1)
		if len(m) != 1 {
			t.Errorf("%s: want exactly one release pin, found %d", file, len(m))
			continue
		}
		if m[0][1] != want {
			t.Errorf("%s pins %s; the newest CHANGELOG release is %s", file, m[0][1], want)
		}
	}
}

// The documented one-liners fetch the boot script from the pinned release
// tag, never from a branch, so the script and the binary it installs come
// from the same release.
func TestOneLinersNameTheReleaseTag(t *testing.T) {
	want := newestRelease(t)
	files := []string{"README.md", "boot.sh", "boot.ps1", "install-hub.sh"}
	docs, err := filepath.Glob(filepath.Join(repoRoot, "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range docs {
		rel, _ := filepath.Rel(repoRoot, d)
		files = append(files, rel)
	}
	url := regexp.MustCompile(`raw\.githubusercontent\.com/BlackVS/aimem/([^/\s]+)/(boot\.sh|boot\.ps1|install-hub\.sh)`)
	seen := 0
	for _, f := range files {
		for _, m := range url.FindAllStringSubmatch(read(t, f), -1) {
			seen++
			if m[1] != want {
				t.Errorf("%s fetches %s from %q; want the release tag %s", f, m[2], m[1], want)
			}
		}
	}
	if seen == 0 {
		t.Fatal("found no one-liner to check")
	}
}
