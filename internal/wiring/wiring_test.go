package wiring

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func findingsByFile(r Report) map[string][]Finding {
	m := map[string][]Finding{}
	for _, f := range r.Findings {
		m[f.File] = append(m[f.File], f)
	}
	return m
}

func TestEmptyCheckoutIsWiredAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	r := Check(dir, Options{Repair: true})
	if r.Failed() {
		t.Fatalf("%+v", r.Findings)
	}
	repaired := 0
	for _, f := range r.Findings {
		if f.Repaired {
			repaired++
		}
	}
	if repaired != 6 { // handoff, two hooks, .mcp.json, two opencode entries
		t.Fatalf("repaired %d: %+v", repaired, r.Findings)
	}
	// The written files have the installers' shapes, no BOM, a trailing newline.
	mcp := read(t, filepath.Join(dir, ".mcp.json"))
	if !strings.HasSuffix(mcp, "\n") || strings.HasPrefix(mcp, "\xef\xbb\xbf") {
		t.Fatalf("mcp.json framing: %q", mcp)
	}
	var m struct {
		Servers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal([]byte(mcp), &m) != nil || m.Servers["aimem"].Command != "aimem" || len(m.Servers["aimem"].Args) != 1 || m.Servers["aimem"].Args[0] != "mcp" {
		t.Fatalf("mcp.json: %s", mcp)
	}
	oc := read(t, filepath.Join(dir, "opencode.json"))
	if !strings.Contains(oc, `"$schema": "https://opencode.ai/config.json"`) || !strings.Contains(oc, `"docs/SESSION-STATE.md"`) || !strings.Contains(oc, `"enabled": true`) {
		t.Fatalf("opencode.json: %s", oc)
	}
	for _, f := range []string{".claude/settings.json", ".codex/hooks.json"} {
		s := read(t, filepath.Join(dir, f))
		if !strings.Contains(s, `"command": "aimem session-start"`) || !strings.Contains(s, `"SessionStart"`) {
			t.Fatalf("%s: %s", f, s)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "docs", "SESSION-STATE.md")); err != nil {
		t.Fatal(err)
	}
	// A second run finds everything present and rewrites nothing.
	before := read(t, filepath.Join(dir, "opencode.json"))
	r = Check(dir, Options{Repair: true})
	for _, f := range r.Findings {
		if f.Repaired || f.Level != "ok" {
			t.Fatalf("second run: %+v", f)
		}
	}
	if read(t, filepath.Join(dir, "opencode.json")) != before {
		t.Fatal("idempotent run rewrote opencode.json")
	}
}

func TestInstallerShapesAndForeignKeysSurvive(t *testing.T) {
	dir := t.TempDir()
	// install.sh's shell-guarded hook spelling, plus a foreign hook and setting.
	write(t, filepath.Join(dir, ".claude", "settings.json"), `{
  "permissions": {"allow": ["Bash(go test:*)"]},
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "command -v aimem >/dev/null 2>&1 && aimem session-start || true", "timeout": 10, "statusMessage": "Loading session handoff"}]},
      {"hooks": [{"type": "command", "command": "echo other"}]}
    ],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "lint"}]}]
  }
}`)
	// A BOM-prefixed .mcp.json with another server, and an opencode.json
	// with a foreign instruction and a foreign mcp entry, missing aimem.
	write(t, filepath.Join(dir, ".mcp.json"), "\xef\xbb\xbf"+`{"mcpServers": {"other": {"command": "x", "args": ["y"], "env": {"K": "V"}}}}`)
	write(t, filepath.Join(dir, "opencode.json"), `{"$schema": "https://opencode.ai/config.json", "instructions": ["AGENTS.md"], "mcp": {"other": {"type": "remote", "url": "https://example.invalid"}}, "theme": "dark"}`)
	write(t, filepath.Join(dir, "docs", "SESSION-STATE.md"), "# x\n")
	r := Check(dir, Options{Repair: true})
	if r.Failed() {
		t.Fatalf("%+v", r.Findings)
	}
	by := findingsByFile(r)
	if f := by[".claude/settings.json"]; len(f) != 1 || f[0].Level != "ok" || f[0].Repaired {
		t.Fatalf("claude settings: %+v", f)
	}
	s := read(t, filepath.Join(dir, ".claude", "settings.json"))
	if !strings.Contains(s, "Bash(go test:*)") || !strings.Contains(s, "echo other") || !strings.Contains(s, "PreToolUse") {
		t.Fatalf("settings rewritten: %s", s)
	}
	mcp := read(t, filepath.Join(dir, ".mcp.json"))
	if strings.HasPrefix(mcp, "\xef\xbb\xbf") || !strings.Contains(mcp, `"other"`) || !strings.Contains(mcp, `"K": "V"`) || !strings.Contains(mcp, `"aimem"`) {
		t.Fatalf("mcp.json: %s", mcp)
	}
	oc := read(t, filepath.Join(dir, "opencode.json"))
	if !strings.Contains(oc, `"AGENTS.md"`) || !strings.Contains(oc, `"docs/SESSION-STATE.md"`) || !strings.Contains(oc, `"other"`) || !strings.Contains(oc, `"theme": "dark"`) || !strings.Contains(oc, `"enabled": true`) {
		t.Fatalf("opencode.json: %s", oc)
	}
	if f := by["opencode.json"]; len(f) != 2 || !f[0].Repaired || !f[1].Repaired {
		t.Fatalf("opencode findings: %+v", f)
	}
}

func TestDifferentEntriesAreReportedNotReplaced(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers": {"aimem": {"command": "/opt/aimem", "args": ["mcp", "--verbose"]}}}`)
	write(t, filepath.Join(dir, "opencode.json"), `{"instructions": ["docs/SESSION-STATE.md"], "mcp": {"aimem": {"type": "local", "command": ["aimem", "mcp"], "enabled": false}}}`)
	before := read(t, filepath.Join(dir, ".mcp.json"))
	r := Check(dir, Options{Repair: true})
	by := findingsByFile(r)
	if f := by[".mcp.json"]; len(f) != 1 || f[0].Level != "warn" || !strings.Contains(f[0].Detail, "differs") {
		t.Fatalf("%+v", f)
	}
	if read(t, filepath.Join(dir, ".mcp.json")) != before {
		t.Fatal("differing entry was rewritten")
	}
	if f := by["opencode.json"]; len(f) != 2 || f[1].Level != "warn" || !strings.Contains(f[1].Detail, "differs") {
		t.Fatalf("%+v", f)
	}
}

func TestInvalidJSONIsNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers": `)
	write(t, filepath.Join(dir, ".claude", "settings.json"), `["not", "an", "object"]`)
	write(t, filepath.Join(dir, "opencode.json"), `{"instructions": "not-an-array"}`)
	r := Check(dir, Options{Repair: true})
	by := findingsByFile(r)
	for _, f := range []string{".mcp.json", ".claude/settings.json", "opencode.json"} {
		if fs := by[f]; len(fs) != 1 || fs[0].Level != "fail" {
			t.Fatalf("%s: %+v", f, fs)
		}
	}
	if read(t, filepath.Join(dir, ".mcp.json")) != `{"mcpServers": ` {
		t.Fatal("invalid file was rewritten")
	}
	if !r.Failed() {
		t.Fatal("invalid files must fail the check")
	}
}

func TestProjectStopHooksBlockUnlessAllowed(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".codex", "hooks.json"), `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "aimem submit-codex"}]}], "SessionStart": [{"hooks": [{"type": "command", "command": "aimem session-start"}]}]}}`)
	r := Check(dir, Options{Repair: false})
	by := findingsByFile(r)
	f := by[".codex/hooks.json"]
	if len(f) != 2 || f[0].Level != "fail" || !strings.Contains(f[0].Detail, "Stop") || f[1].Level != "ok" {
		t.Fatalf("%+v", f)
	}
	if !r.Failed() {
		t.Fatal("must fail")
	}
	r = Check(dir, Options{Repair: false, AllowProjectStopHooks: true})
	if r.Failed() || findingsByFile(r)[".codex/hooks.json"][0].Level != "warn" {
		t.Fatalf("%+v", r.Findings)
	}
}

func TestEmptyStopHookArraysDoNotBlock(t *testing.T) {
	for _, body := range []string{`{"hooks":{"Stop":[ ]}}`, "{\"hooks\":{\"Stop\":[\n  \n],\"PreCompact\":null,\"StopFailure\":[]}}"} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, ".claude", "settings.json"), body)
		r := Check(dir, Options{Repair: false})
		if r.Failed() {
			t.Fatalf("%q blocked: %+v", body, r.Findings)
		}
		for _, f := range findingsByFile(r)[".claude/settings.json"] {
			if strings.Contains(f.Detail, "journal") {
				t.Fatalf("%q: %+v", body, f)
			}
		}
	}
}

func TestTypeMismatchesAreDrift(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".mcp.json"), `{"mcpServers": {"aimem": {"command": "aimem", "args": "[mcp]"}}}`)
	write(t, filepath.Join(dir, "opencode.json"), `{"instructions": ["docs/SESSION-STATE.md"], "mcp": {"aimem": {"type": "local", "command": ["aimem", "mcp"], "enabled": "true"}}}`)
	r := Check(dir, Options{Repair: true})
	by := findingsByFile(r)
	if f := by[".mcp.json"]; len(f) != 1 || f[0].Level != "warn" || !strings.Contains(f[0].Detail, "differs") {
		t.Fatalf("string args accepted: %+v", f)
	}
	if f := by["opencode.json"]; len(f) != 2 || f[1].Level != "warn" || !strings.Contains(f[1].Detail, "differs") {
		t.Fatalf("string enabled accepted: %+v", f)
	}
}

func TestOpenCodeInvalidMCPLeavesFileAndClaimsNoRepair(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "opencode.json"), `{"mcp": []}`)
	r := Check(dir, Options{Repair: true})
	f := findingsByFile(r)["opencode.json"]
	if len(f) != 1 || f[0].Level != "fail" || f[0].Repaired {
		t.Fatalf("%+v", f)
	}
	if read(t, filepath.Join(dir, "opencode.json")) != `{"mcp": []}` {
		t.Fatal("invalid file was rewritten")
	}
}

func TestNoRepairOnlyReports(t *testing.T) {
	dir := t.TempDir()
	r := Check(dir, Options{Repair: false})
	if r.Failed() {
		t.Fatalf("%+v", r.Findings)
	}
	for _, f := range r.Findings {
		if f.Level != "warn" || f.Repaired {
			t.Fatalf("%+v", f)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("no-repair wrote files: %v", entries)
	}
}

func TestSkillReportPerClientRequiresSkillMD(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	// A directory without SKILL.md does not count; one with it does.
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "skills", "oh-code-review"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, ".agents", "skills", "oh-code-review", "SKILL.md"), "---\nname: oh-code-review\n---\n")
	clients := []Client{{Name: "claude"}, {Name: "codex"}, {Name: "opencode"}}
	got := skillReport(dir, home, clients, []string{"oh-code-review", "Bad Name"})
	want := map[string]bool{"oh-code-review/claude": false, "oh-code-review/codex": true, "oh-code-review/opencode": false, "Bad Name/claude": false, "Bad Name/codex": false, "Bad Name/opencode": false}
	if len(got) != len(want) {
		t.Fatalf("%+v", got)
	}
	for _, s := range got {
		if (s.Path != "") != want[s.Skill+"/"+s.Client] {
			t.Fatalf("%+v", s)
		}
		if s.Path == "" && s.Fix == "" {
			t.Fatalf("missing fix: %+v", s)
		}
	}
}

func TestBothHookSpellingsSatisfy(t *testing.T) {
	for _, cmd := range []string{"aimem session-start", "command -v aimem >/dev/null 2>&1 && aimem session-start || true", "cat docs/SESSION-STATE.md"} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, ".claude", "settings.json"), `{"hooks": {"SessionStart": [{"hooks": [{"type": "command", "command": `+jsonString(cmd)+`}]}]}}`)
		r := Check(dir, Options{Repair: false})
		if f := findingsByFile(r)[".claude/settings.json"]; len(f) != 1 || f[0].Level != "ok" {
			t.Fatalf("%q: %+v", cmd, f)
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
