package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// wireSandbox is a home directory and two directories beside it: a project
// that already holds .aimem.json and a plain directory that does not.
type wireSandbox struct {
	dir, home, project, plain string
	env                       []string
}

func newWireSandbox(t *testing.T) *wireSandbox {
	t.Helper()
	dir := t.TempDir()
	w := &wireSandbox{dir: dir, home: filepath.Join(dir, "home"),
		project: filepath.Join(dir, "project"), plain: filepath.Join(dir, "plain")}
	for _, d := range []string{w.home, w.project, w.plain} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(w.project, ".aimem.json"), []byte(`{"groups":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(strings.ToUpper(k), "AIMEM_"), strings.HasPrefix(strings.ToUpper(k), "XDG_"),
			strings.EqualFold(k, "HOME"), strings.EqualFold(k, "USERPROFILE"), strings.EqualFold(k, "LOCALAPPDATA"):
			continue
		}
		w.env = append(w.env, kv)
	}
	w.env = append(w.env, "HOME="+w.home, "USERPROFILE="+w.home, "LOCALAPPDATA="+filepath.Join(dir, "localappdata"))
	return w
}

// wireCase is one bootstrap situation and whether it wires the directory.
type wireCase struct {
	name      string
	dir       func(w *wireSandbox) string
	installed bool
	userOnly  bool
	wires     bool
	reason    string
}

var wireCases = []wireCase{
	{name: "fresh install wires the current directory", dir: func(w *wireSandbox) string { return w.plain }, wires: true},
	{name: "upgrade wires a project with .aimem.json", dir: func(w *wireSandbox) string { return w.project }, installed: true, wires: true},
	{name: "upgrade leaves a directory without .aimem.json alone", dir: func(w *wireSandbox) string { return w.plain }, installed: true, reason: "has no .aimem.json"},
	{name: "fresh install never wires the home", dir: func(w *wireSandbox) string { return w.home }, reason: "is the home directory"},
	{name: "upgrade never wires the home", dir: func(w *wireSandbox) string { return w.home }, installed: true, reason: "is the home directory"},
	{name: "AIMEM_USER_ONLY wires nothing", dir: func(w *wireSandbox) string { return w.project }, installed: true, userOnly: true, reason: "AIMEM_USER_ONLY=1"},
	{name: "AIMEM_USER_ONLY on a fresh install wires nothing", dir: func(w *wireSandbox) string { return w.plain }, userOnly: true, reason: "AIMEM_USER_ONLY=1"},
}

func checkWireDecision(t *testing.T, c wireCase, out string) {
	t.Helper()
	out = strings.TrimSpace(out)
	if c.wires && out != "" {
		t.Errorf("want the directory wired, got skip reason %q", out)
	}
	if !c.wires && !strings.Contains(out, c.reason) {
		t.Errorf("want a skip reason containing %q, got %q", c.reason, out)
	}
}

func TestShellBootstrapWireDecision(t *testing.T) {
	shellOnly(t)
	block := extract(t, "install.sh", "wire-decision")
	for _, c := range wireCases {
		t.Run(c.name, func(t *testing.T) {
			w := newWireSandbox(t)
			installed := map[bool]string{false: "0", true: "1"}[c.installed]
			cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+block+`wire_skip_reason "$1" "$2"`, "bash", c.dir(w), installed)
			cmd.Dir = w.dir
			cmd.Env = w.env
			if c.userOnly {
				cmd.Env = append(cmd.Env, "AIMEM_USER_ONLY=1")
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("wire_skip_reason: %v\n%s", err, out)
			}
			checkWireDecision(t, c, string(out))
		})
	}
}

// An explicit `install.sh project ~` is refused before anything is written.
func TestShellProjectRefusesHome(t *testing.T) {
	shellOnly(t)
	w := newWireSandbox(t)
	installer, err := filepath.Abs(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", installer, "project", w.home)
	cmd.Dir = w.dir
	cmd.Env = w.env
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "refusing to wire the home directory") {
		t.Fatalf("install.sh project accepted the home directory (%v):\n%s", err, out)
	}
	checkHomeUntouched(t, w)
}

// checkHomeUntouched: nothing a project wiring writes is in the home.
// (PowerShell itself creates AppData under an empty profile.)
func checkHomeUntouched(t *testing.T, w *wireSandbox) {
	t.Helper()
	for _, f := range []string{"CLAUDE.md", "AGENTS.md", "docs", ".aimem.json", ".mcp.json",
		"opencode.json", ".opencode", ".claude", ".codex"} {
		if exists(filepath.Join(w.home, f)) {
			t.Errorf("the refused run wrote %s into the home directory", f)
		}
	}
}

func powerShell(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 runs on Windows only; the Windows test job runs this")
	}
	for _, name := range []string{"powershell", "pwsh"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Fatal("no PowerShell on a Windows runner")
	return ""
}

// runPowerShell runs a script file in a sandbox, with the working
// directory and PowerShell's module analysis cache inside it.
func runPowerShell(t *testing.T, ps, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(ps, append([]string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(env, "PSModuleAnalysisCachePath="+filepath.Join(dir, "ps-module-analysis-cache"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestPowerShellBootstrapWireDecision(t *testing.T) {
	ps := powerShell(t)
	block := extract(t, "install.ps1", "wire-decision")
	for _, c := range wireCases {
		t.Run(c.name, func(t *testing.T) {
			w := newWireSandbox(t)
			script := "$ErrorActionPreference = 'Stop'\n" + block +
				"Write-Output (Get-WireSkipReason $env:WIRE_DIR ($env:WIRE_INSTALLED -eq '1'))\n"
			file := filepath.Join(w.dir, "decide.ps1")
			if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			env := append(w.env, "WIRE_DIR="+c.dir(w), "WIRE_INSTALLED="+map[bool]string{false: "0", true: "1"}[c.installed])
			if c.userOnly {
				env = append(env, "AIMEM_USER_ONLY=1")
			}
			out, err := runPowerShell(t, ps, w.dir, env, file)
			if err != nil {
				t.Fatalf("Get-WireSkipReason: %v\n%s", err, out)
			}
			checkWireDecision(t, c, out)
		})
	}
}

// boot.ps1 passes -Bootstrap to an install.ps1 that declares it, and
// leaves it out for an older release's install.ps1 (AIMEM_VERSION), which
// would refuse the unknown parameter. The installer under test is a stub
// that prints the arguments it was given.
func TestBootPs1PassesBootstrapWhenTheInstallerHasIt(t *testing.T) {
	ps := powerShell(t)
	block := extract(t, "boot.ps1", "run-installer")
	for _, c := range []struct {
		name, param, want string
	}{
		{"this release", "  [switch]$Bootstrap,\n", "args: -Bootstrap -Target "},
		{"an older release", "", "args: -Target "},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "dest")
			if err := os.MkdirAll(dest, 0o700); err != nil {
				t.Fatal(err)
			}
			stub := "param(\n  [string]$Target,\n" + c.param + "  [switch]$UserOnly\n)\nWrite-Output \"args: $([Environment]::GetCommandLineArgs() | Select-Object -Skip 6)\"\n"
			if err := os.WriteFile(filepath.Join(dest, "install.ps1"), []byte(stub), 0o600); err != nil {
				t.Fatal(err)
			}
			script := "$ErrorActionPreference = 'Stop'\n$dest = $env:DEST\n" + block
			file := filepath.Join(dir, "run.ps1")
			if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := runPowerShell(t, ps, dir, append(os.Environ(), "DEST="+dest), file)
			if err != nil || !strings.Contains(out, c.want) {
				t.Fatalf("want %q (%v):\n%s", c.want, err, out)
			}
		})
	}
}

// An explicit `install.ps1 -Target ~` is refused before anything is
// installed or written.
func TestPowerShellTargetRefusesHome(t *testing.T) {
	ps := powerShell(t)
	w := newWireSandbox(t)
	installer, err := filepath.Abs(filepath.Join("..", "..", "install.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := runPowerShell(t, ps, w.dir, w.env, installer, "-Target", w.home)
	if err == nil || !strings.Contains(out, "refusing to wire the home directory") {
		t.Fatalf("install.ps1 -Target accepted the home directory (%v):\n%s", err, out)
	}
	checkHomeUntouched(t, w)
	if exists(filepath.Join(w.dir, "localappdata")) {
		t.Error("the refused run installed aimem")
	}
}

// The one-liners run the installers' bootstrap entry points, where the
// wire decision applies; AIMEM_USER_ONLY reaches them in the environment.
func TestBootScriptsRunTheBootstrapEntryPoints(t *testing.T) {
	if s := read(t, "boot.sh"); !strings.Contains(s, `install.sh" bootstrap "$PWD"`) {
		t.Error("boot.sh does not run install.sh bootstrap")
	}
	for _, f := range []string{"boot.sh", "boot.ps1"} {
		if !strings.Contains(read(t, f), "AIMEM_USER_ONLY=1") {
			t.Errorf("%s does not document AIMEM_USER_ONLY=1", f)
		}
	}
}
