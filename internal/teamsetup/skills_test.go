package teamsetup

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aimem/internal/process"
	"aimem/internal/processctx"
)

// fakeClients puts executables named claude and codex on PATH, so the
// per-client skill check runs; they are never executed.
func fakeClients(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	for _, name := range []string{"claude", "codex"} {
		if runtime.GOOS == "windows" {
			os.WriteFile(filepath.Join(bin, name+".cmd"), []byte("@echo off\r\n"), 0o755)
		} else {
			os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nexit 0\n"), 0o755)
		}
	}
	t.Setenv("PATH", bin)
}

func writeSkill(t *testing.T, dir, name, record string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, name, "SKILL.md"), []byte("---\nname: "+name+"\n---\n"), 0o644)
	os.WriteFile(filepath.Join(dir, process.InstalledRecordName), []byte(record), 0o644)
}

func processCheck(rep *Report) Check {
	for _, c := range rep.Checks {
		if c.Name == "process" {
			return c
		}
	}
	return Check{}
}

// The process check judges each client by the copy that client reads: one
// client's current copy never answers for another client's outdated one,
// and the same version from the other source is a mismatch.
func TestProcessCheckMatchesThePinPerClient(t *testing.T) {
	ts := identityHub(t)
	repo, root := checkout(t, ts)
	fakeClients(t)
	claude := filepath.Join(repo, ".claude", "skills")
	codex := filepath.Join(repo, ".agents", "skills")
	writeSkill(t, claude, "oh-code-review", `{"version":"2.0","skills":["oh-code-review"],"installed_at":"x","versions":{"oh-code-review":"2.0"},"sources":{"oh-code-review":"release"}}`)
	writeSkill(t, codex, "oh-code-review", `{"version":"1.0","skills":["oh-code-review"],"installed_at":"x"}`)
	ref := process.Ref{Repo: "https://127.0.0.1:1/p.git", Commit: strings.Repeat("ab", 20), Manifest: "m.json"}
	set := &process.Set{Ref: ref, Manifest: process.Manifest{Version: 2, Handbook: "h.md", Skills: []process.SkillPin{{Name: "oh-code-review", Version: "2.0", Source: "release"}}}, Handbook: "# H\n"}
	ready := &processctx.Result{State: processctx.Ready, Project: "alpha", Ref: &ref, Source: processctx.SourceHub, Set: set, Unit: "# H\n"}
	env := Env{Dir: repo, Root: root, Version: "v0.7.3", TeamCall: (&availabilityCaller{}).call, Process: func(string, string) *processctx.Result { return ready }}

	c := processCheck(Run(env, workerOpts()))
	if c.Level != "warn" || !strings.Contains(c.Detail, "oh-code-review for codex (installed 1.0)") || strings.Contains(c.Detail, "for claude") || !strings.Contains(c.Fix, "aimem skills install") {
		t.Fatalf("mixed clients: %+v", c)
	}
	// The same version from the other source is another skill.
	writeSkill(t, codex, "oh-code-review", `{"version":"2.0","skills":["oh-code-review"],"installed_at":"x","versions":{"oh-code-review":"2.0"},"sources":{"oh-code-review":"instance"}}`)
	c = processCheck(Run(env, workerOpts()))
	if c.Level != "warn" || !strings.Contains(c.Detail, "oh-code-review for codex (installed 2.0 (instance))") || strings.Contains(c.Detail, "for claude") {
		t.Fatalf("same version, other source: %+v", c)
	}
	// Both at the pin: ok, and the pin is named.
	writeSkill(t, codex, "oh-code-review", `{"version":"2.0","skills":["oh-code-review"],"installed_at":"x","versions":{"oh-code-review":"2.0"},"sources":{"oh-code-review":"release"}}`)
	c = processCheck(Run(env, workerOpts()))
	if c.Level != "ok" || !strings.Contains(c.Detail, "oh-code-review 2.0 (release)") || !strings.Contains(c.Detail, "found for every client on PATH") {
		t.Fatalf("both at the pin: %+v", c)
	}
	// A copy without a record is installed, version unknown: not a mismatch.
	os.Remove(filepath.Join(codex, process.InstalledRecordName))
	if c = processCheck(Run(env, workerOpts())); c.Level != "ok" {
		t.Fatalf("unknown version: %+v", c)
	}
}
