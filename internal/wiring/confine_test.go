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
