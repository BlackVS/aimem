package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWireCmdFlagsAroundTheDirectoryAndCheck(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", filepath.Join(base, "state"))
	t.Setenv("HOME", filepath.Join(base, "home"))
	t.Setenv("USERPROFILE", filepath.Join(base, "home"))
	dir := filepath.Join(base, "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := wireCmd([]string{"--check", dir}, &out); err == nil || !strings.Contains(err.Error(), "not fully wired") {
		t.Fatalf("--check on an empty directory = %v, want not fully wired", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("--check wrote %d entries", len(entries))
	}
	out.Reset()
	if err := wireCmd([]string{"-p", "demo", dir, "--groups", ""}, &out); err != nil {
		t.Fatalf("wire: %v\n%s", err, out.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".aimem.json"))
	if err != nil || !strings.Contains(string(raw), `"project": "demo"`) || !strings.Contains(string(raw), `"groups": []`) {
		t.Fatalf(".aimem.json = %s, %v", raw, err)
	}
	out.Reset()
	if err := wireCmd([]string{dir, "--check", "-p", "demo"}, &out); err != nil {
		t.Fatalf("--check after wiring: %v\n%s", err, out.String())
	}
	if err := wireCmd([]string{dir, dir}, &out); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("two directories = %v, want usage", err)
	}
}
