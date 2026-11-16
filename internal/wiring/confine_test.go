package wiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/confine/linktest"
)

// A repair never reads or writes a client file through a link below the
// checkout: each such file is reported as failed with the refusal, and the
// files the links point at keep their content.
func TestRepairRefusesFilesBehindALink(t *testing.T) {
	base := t.TempDir()
	dir, outside := filepath.Join(base, "checkout"), filepath.Join(base, "outside")
	os.MkdirAll(dir, 0o755)
	for _, d := range []string{"claude", "codex", "docs"} {
		write(t, filepath.Join(outside, d, "keep.txt"), "outside")
	}
	write(t, filepath.Join(outside, "codex", "hooks.json"), "{}")
	linktest.Dir(t, filepath.Join(dir, ".claude"), filepath.Join(outside, "claude"))
	linktest.Dir(t, filepath.Join(dir, ".codex"), filepath.Join(outside, "codex"))
	linktest.Dir(t, filepath.Join(dir, "docs"), filepath.Join(outside, "docs"))

	r := Check(dir, Options{Repair: true, Commands: true})
	by := findingsByFile(r)
	for _, file := range []string{".claude/settings.json", ".codex/hooks.json"} {
		fs := by[file]
		if len(fs) == 0 || fs[0].Level != "fail" || !strings.Contains(fs[0].Detail, "symbolic link or junction") {
			t.Errorf("%s: %+v", file, fs)
		}
	}
	refused := 0
	for _, f := range r.Findings {
		if f.Repaired && strings.Contains(f.File, "claude") {
			t.Errorf("a file under the linked .claude was written: %+v", f)
		}
		if f.Level == "fail" && strings.Contains(f.Detail, "symbolic link or junction") {
			refused++
		}
	}
	if refused < 4 { // the two hook files, the handoff and the Claude command assets
		t.Errorf("only %d refusals: %+v", refused, r.Findings)
	}
	// The handoff under the linked docs directory fails with the fix, not
	// "missing" or "present".
	if fs := by["docs/SESSION-STATE.md"]; len(fs) == 0 || fs[0].Level != "fail" ||
		!strings.Contains(fs[0].Fix, "replace the link with a real directory and file") {
		t.Errorf("docs/SESSION-STATE.md: %+v", fs)
	}
	for _, d := range []string{"claude", "codex", "docs"} {
		es, _ := os.ReadDir(filepath.Join(outside, d))
		want := 1
		if d == "codex" {
			want = 2
		}
		if len(es) != want {
			t.Errorf("files appeared in %s outside the checkout: %v", d, es)
		}
	}
	if read(t, filepath.Join(outside, "codex", "hooks.json")) != "{}" {
		t.Error("the hooks file behind the link was rewritten")
	}
	// The plain files beside the links are still repaired.
	if _, err := os.Stat(filepath.Join(dir, ".mcp.json")); err != nil {
		t.Errorf(".mcp.json was not written: %v", err)
	}
}

// Only a link gives the link finding. A regular file named docs makes the
// walk fail with a plain error on Unix (ENOTDIR), which must fall through to
// the ordinary "missing" finding instead of advice about a link.
func TestCheckHandoffOtherErrorIsNoLinkFinding(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "docs"), "a file, not a directory\n")
	f := checkHandoff(dir, false)
	if f.Level != "warn" || strings.Contains(f.Detail, "symbolic link") || strings.Contains(f.Fix, "link") {
		t.Fatalf("a plain error read as a link: %+v", f)
	}
}

// A handoff that exists behind a link is no "present" handoff: the
// checkpoint publisher refuses it, so the check fails, names the link and
// says how to fix it, with and without repair.
func TestCheckHandoffFailsForAHandoffBehindALink(t *testing.T) {
	for _, linkFile := range []bool{false, true} {
		base := t.TempDir()
		dir, outside := filepath.Join(base, "checkout"), filepath.Join(base, "outside")
		write(t, filepath.Join(outside, "SESSION-STATE.md"), "# handoff outside\n")
		if linkFile {
			os.MkdirAll(filepath.Join(dir, "docs"), 0o755)
			linktest.File(t, filepath.Join(dir, "docs", "SESSION-STATE.md"), filepath.Join(outside, "SESSION-STATE.md"))
		} else {
			os.MkdirAll(dir, 0o755)
			linktest.Dir(t, filepath.Join(dir, "docs"), outside)
		}
		for _, repair := range []bool{false, true} {
			f := checkHandoff(dir, repair)
			if f.Level != "fail" || !strings.Contains(f.Detail, "symbolic link or junction") ||
				!strings.Contains(f.Fix, "replace the link") || f.Repaired {
				t.Errorf("linked file %v, repair %v: %+v", linkFile, repair, f)
			}
		}
		if read(t, filepath.Join(outside, "SESSION-STATE.md")) != "# handoff outside\n" {
			t.Errorf("linked file %v: the handoff behind the link was rewritten", linkFile)
		}
	}
}
