package installer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A user's settings.json with non-ASCII content keeps its bytes when
// install.ps1 merges its hook into it: Windows PowerShell 5.1 read a
// BOM-less file in the ANSI code page and wrote the garbled text back as
// UTF-8. A file that starts with a BOM is read too, and written without.
func TestPowerShellHookMergeKeepsNonASCIISettings(t *testing.T) {
	ps := powerShell(t)
	block := extract(t, "install.ps1", "json-io")
	const statusLine = "echo \U0001F9E0 café — ünïcode"
	for _, bom := range []bool{false, true} {
		t.Run(map[bool]string{false: "without a BOM", true: "with a BOM"}[bom], func(t *testing.T) {
			dir := t.TempDir()
			settings := filepath.Join(dir, "settings.json")
			body := []byte(`{"statusLine":{"type":"command","command":"` + statusLine + `"}}`)
			if bom {
				body = append([]byte("\xef\xbb\xbf"), body...)
			}
			if err := os.WriteFile(settings, body, 0o600); err != nil {
				t.Fatal(err)
			}
			script := "$ErrorActionPreference = 'Stop'\n" + block +
				"Add-AgentHook $env:SETTINGS 'PreCompact' 'aimem checkpoint' 'saving' 'aimem '\n"
			file := filepath.Join(dir, "merge.ps1")
			if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := runPowerShell(t, ps, dir, append(os.Environ(), "SETTINGS="+settings), file); err != nil {
				t.Fatalf("Add-AgentHook: %v\n%s", err, out)
			}
			got, err := os.ReadFile(settings)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(got, []byte(statusLine)) {
				t.Errorf("the non-ASCII statusLine did not survive the merge:\n%s", got)
			}
			if !strings.Contains(string(got), `"aimem checkpoint"`) {
				t.Errorf("the hook was not merged:\n%s", got)
			}
			if bytes.HasPrefix(got, []byte("\xef\xbb\xbf")) {
				t.Error("the merged settings.json starts with a BOM")
			}
		})
	}
}

// Read-Text, which also reads Codex's config.toml before rewriting it,
// returns UTF-8 text unchanged.
func TestPowerShellReadTextIsUTF8(t *testing.T) {
	ps := powerShell(t)
	block := extract(t, "install.ps1", "json-io")
	dir := t.TempDir()
	in, out := filepath.Join(dir, "config.toml"), filepath.Join(dir, "copy.toml")
	text := "# \U0001F9E0 notes: café\n[mcp_servers.other]\ncommand = \"ünïcode\"\n"
	if err := os.WriteFile(in, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "$ErrorActionPreference = 'Stop'\n" + block +
		"[System.IO.File]::WriteAllText($env:OUT, (Read-Text $env:IN), $Utf8NoBom)\n"
	file := filepath.Join(dir, "copy.ps1")
	if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if o, err := runPowerShell(t, ps, dir, append(os.Environ(), "IN="+in, "OUT="+out), file); err != nil {
		t.Fatalf("Read-Text: %v\n%s", err, o)
	}
	if got, err := os.ReadFile(out); err != nil || string(got) != text {
		t.Errorf("Read-Text changed the text (%v):\n%q\nwant\n%q", err, got, text)
	}
}
