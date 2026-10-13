package installer

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// failureHook finds the aimem failure-recall entry under PostToolUseFailure
// in a Claude Code settings.json: the last one's matcher, and how many
// there are.
func failureHook(t *testing.T, settings string) (matcher string, n int) {
	t.Helper()
	b, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("settings.json: %v\n%s", err, b)
	}
	for _, e := range s.Hooks["PostToolUseFailure"] {
		for _, h := range e.Hooks {
			if strings.Contains(h.Command, "aimem failure-recall") {
				matcher, n = e.Matcher, n+1
			}
		}
	}
	return matcher, n
}

const otherHooks = `{"hooks":{"PostToolUseFailure":[{"matcher":"Edit","hooks":[{"type":"command","command":"my-own-hook"}]}]},"theme":"dark"}`

// install.sh wires the failure recall on PostToolUseFailure for Bash once,
// next to the user's own hooks, and its uninstall removes only aimem's.
func TestShellWiresTheFailureRecallHook(t *testing.T) {
	shellOnly(t)
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("no jq")
	}
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settings, []byte(otherHooks), 0o600); err != nil {
		t.Fatal(err)
	}
	add := `add_agent_hook "$SETTINGS" PostToolUseFailure "$FAILURE_RECALL_CMD" "Recalling aimem for the failure" "aimem failure-recall" Bash`
	script := "set -euo pipefail\n" +
		"FAILURE_RECALL_CMD='command -v aimem >/dev/null 2>&1 && aimem failure-recall || true'\n" +
		extract(t, "install.sh", "claude-hooks") + add + "\n" + add + "\n"
	run := func(body string) {
		cmd := exec.Command("bash", "-c", body)
		cmd.Env = append(os.Environ(), "SETTINGS="+settings)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	run(script)
	if m, n := failureHook(t, settings); n != 1 || m != "Bash" {
		t.Fatalf("failure-recall entries %d with matcher %q, want one for Bash", n, m)
	}
	if b, _ := os.ReadFile(settings); !strings.Contains(string(b), "my-own-hook") || !strings.Contains(string(b), `"theme": "dark"`) {
		t.Fatalf("the user's own settings were lost:\n%s", b)
	}
	if !strings.Contains(read(t, "install.sh"), `add_agent_hook "$CLAUDE_SETTINGS" PostToolUseFailure "$FAILURE_RECALL_CMD"`) {
		t.Error("install.sh does not wire the failure recall into the user's Claude Code settings")
	}
	run("set -euo pipefail\n" + extract(t, "install.sh", "claude-hooks") + `remove_claude_hooks "$SETTINGS"` + "\n")
	if _, n := failureHook(t, settings); n != 0 {
		t.Fatal("uninstall left the failure-recall hook")
	}
	if b, _ := os.ReadFile(settings); !strings.Contains(string(b), "my-own-hook") {
		t.Fatalf("uninstall removed the user's own hook:\n%s", b)
	}
}

// install.ps1 writes the same entry, with the Bash matcher, once.
func TestPowerShellWiresTheFailureRecallHook(t *testing.T) {
	ps := powerShell(t)
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(settings, []byte(otherHooks), 0o600); err != nil {
		t.Fatal(err)
	}
	add := "Add-AgentHook $env:SETTINGS 'PostToolUseFailure' 'aimem failure-recall' 'Recalling aimem for the failure' 'aimem failure-recall' 'Bash'\n"
	script := "$ErrorActionPreference = 'Stop'\n" + extract(t, "install.ps1", "json-io") + add + add
	file := filepath.Join(dir, "wire.ps1")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runPowerShell(t, ps, dir, append(os.Environ(), "SETTINGS="+settings), file); err != nil {
		t.Fatalf("Add-AgentHook: %v\n%s", err, out)
	}
	if m, n := failureHook(t, settings); n != 1 || m != "Bash" {
		t.Fatalf("failure-recall entries %d with matcher %q, want one for Bash", n, m)
	}
	if b, _ := os.ReadFile(settings); !strings.Contains(string(b), "my-own-hook") {
		t.Fatalf("the user's own hook was lost:\n%s", b)
	}
	if !strings.Contains(read(t, "install.ps1"), "Add-AgentHook $ClaudeSettings 'PostToolUseFailure' $FailureRecallCmd") ||
		!strings.Contains(read(t, "install.ps1"), "'aimem submit-claude|aimem failure-recall'") {
		t.Error("install.ps1 does not wire, or does not uninstall, the failure recall")
	}
}
