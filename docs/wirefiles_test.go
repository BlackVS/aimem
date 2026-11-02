package docs_test

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"aimem/internal/wiring"
)

// TestWireFileListsAgree holds the two documents that list what
// `aimem wire` writes to wiring.ProjectFiles, the list a wiring test holds
// Wire itself to: INSTALL-CLIENT's wiring table, with its committed or
// local column, and the quickstart's step 3.5, with its .gitignore lines.
func TestWireFileListsAgree(t *testing.T) {
	committed, local := map[string]bool{}, map[string]bool{}
	for _, f := range wiring.ProjectFiles {
		if f.Local {
			local[f.Path] = true
		} else {
			committed[f.Path] = true
		}
	}

	t.Run("INSTALL-CLIENT", func(t *testing.T) {
		got := map[string]string{}
		for _, row := range section(t, "INSTALL-CLIENT.md", "### What wiring a project adds to it\n", "\n\n") {
			cells := splitRow(row)
			if len(cells) < 3 || !strings.HasPrefix(cells[0], "`") {
				continue
			}
			for _, p := range paths(cells[0]) {
				got[p] = cells[1]
			}
		}
		for _, f := range wiring.ProjectFiles {
			want := map[bool]string{false: "committed", true: "local"}[f.Local]
			if got[f.Path] != want {
				t.Errorf("%s: the table says %q, want %q", f.Path, got[f.Path], want)
			}
			delete(got, f.Path)
		}
		for p := range got {
			t.Errorf("%s: in the table but not written by aimem wire", p)
		}
	})

	t.Run("QUICKSTART-NEW-PROJECT", func(t *testing.T) {
		var listed []string
		for _, l := range section(t, "QUICKSTART-NEW-PROJECT.md", "<!-- wire-files: committed -->\n", "<!-- wire-files: end -->") {
			listed = append(listed, paths(l)...)
		}
		for p := range committed {
			if !slices.Contains(listed, p) {
				t.Errorf("step 3.5 does not list %s as committed", p)
			}
		}
		for _, p := range listed {
			if !committed[p] {
				t.Errorf("step 3.5 lists %s as committed, which aimem wire does not write as such", p)
			}
		}
		var localListed []string
		for _, l := range section(t, "QUICKSTART-NEW-PROJECT.md", "<!-- wire-files: local -->\n", "<!-- wire-files: end -->") {
			localListed = append(localListed, paths(l)...)
		}
		for p := range local {
			if !slices.Contains(localListed, p) {
				t.Errorf("step 3.5 does not list %s as local", p)
			}
		}
		for _, p := range localListed {
			if committed[p] {
				t.Errorf("step 3.5 lists the committed %s as local", p)
			}
		}
		block := section(t, "QUICKSTART-NEW-PROJECT.md", "```gitignore\n", "```")
		if !slices.Equal(block, wiring.GitignoreLines) {
			t.Errorf("step 3.5's .gitignore lines:\n%s\nwant:\n%s", strings.Join(block, "\n"), strings.Join(wiring.GitignoreLines, "\n"))
		}
	})
}

// section returns the lines of docs/name after the first start and before
// the next end.
func section(t *testing.T, name, start, end string) []string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("%s: no %q", name, start)
	}
	s = s[i+len(start):]
	if j := strings.Index(s, end); j >= 0 {
		s = s[:j]
	}
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// splitRow splits a Markdown table row on its unescaped pipes.
func splitRow(row string) []string {
	row = strings.ReplaceAll(row, `\|`, "\x00")
	var cells []string
	for _, c := range strings.Split(strings.Trim(strings.TrimSpace(row), "|"), "|") {
		cells = append(cells, strings.TrimSpace(strings.ReplaceAll(c, "\x00", `\|`)))
	}
	return cells
}

var codeSpan = regexp.MustCompile("`([^`\\s]+)`")

// paths returns the code spans of s that name a file in the checkout: no
// command (`/join_team`), no space, no home path.
func paths(s string) []string {
	var out []string
	for _, m := range codeSpan.FindAllStringSubmatch(s, -1) {
		p := m[1]
		if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") || !strings.ContainsAny(p, "./") {
			continue
		}
		out = append(out, p)
	}
	return out
}
