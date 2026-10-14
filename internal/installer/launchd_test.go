package installer

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeLaunchctl stands in for launchctl: the loaded agents are files in
// $FAKE_LAUNCHD, and loading the serve agent starts its program as a
// process whose PID is kept there, so bootout stops exactly that process.
// FAKE_NO_GUI=1 is a machine with no login session.
const fakeLaunchctl = `#!/usr/bin/env bash
d=$FAKE_LAUNCHD
mkdir -p "$d"
echo "$*" >> "$d/calls"
case "$1" in
  print)
    case "$2" in
      */*/*) [ -f "$d/${2##*/}.loaded" ] ;;
      *) [ "${FAKE_NO_GUI:-0}" != 1 ] ;;
    esac ;;
  bootstrap)
    label=$(basename "$3" .plist)
    if [ -f "$d/$label.loaded" ]; then echo "Bootstrap failed: 5: Input/output error" >&2; exit 5; fi
    cp "$3" "$d/$label.loaded"
    if grep -q '<string>serve</string>' "$3"; then
      prog=$(sed -n 's:^ *<string>\(/.*/aimem\)</string>$:\1:p' "$3" | head -n 1)
      "$prog" serve >>"$d/serve.log" 2>&1 &
      echo $! > "$d/$label.pid"
    fi ;;
  bootout)
    label=${2##*/}
    [ -f "$d/$label.loaded" ] || { echo "Boot-out failed: 3: No such process" >&2; exit 3; }
    rm -f "$d/$label.loaded"
    if [ -f "$d/$label.pid" ]; then
      pid=$(cat "$d/$label.pid")
      kill "$pid" 2>/dev/null || true
      while kill -0 "$pid" 2>/dev/null; do sleep 0.1; done
      rm -f "$d/$label.pid"
    fi ;;
  *) echo "fake launchctl: unexpected $*" >&2; exit 64 ;;
esac
`

// macSandbox is a sandbox in which install.sh sees macOS: uname says
// Darwin and launchctl is fakeLaunchctl. PATH holds only the system
// directories and jq's, so no agent CLI of the machine is reached.
type macSandbox struct {
	*sandbox
	launchd, agents string
}

func newMacSandbox(t *testing.T, installedRelease string) *macSandbox {
	t.Helper()
	jq, err := exec.LookPath("jq")
	if err != nil {
		t.Skip("no jq")
	}
	realUname, err := exec.LookPath("uname")
	if err != nil {
		t.Skip("no uname")
	}
	// "none" is a machine without aimem: the sandbox's binary is removed.
	release := installedRelease
	if release == "none" {
		release = "v0.0.1"
	}
	s := newSandbox(t, binary(t, release), false)
	if installedRelease == "none" {
		if err := os.Remove(s.installed()); err != nil {
			t.Fatal(err)
		}
	}
	m := &macSandbox{sandbox: s, launchd: filepath.Join(s.dir, "launchd"),
		agents: filepath.Join(s.dir, "home", "Library", "LaunchAgents")}
	stub := filepath.Join(s.dir, "stub")
	if err := os.MkdirAll(stub, 0o700); err != nil {
		t.Fatal(err)
	}
	uname := "#!/bin/sh\nif [ \"$1\" = -s ]; then echo Darwin; else exec " + realUname + " \"$@\"; fi\n"
	for name, body := range map[string]string{"launchctl": fakeLaunchctl, "uname": uname} {
		if err := os.WriteFile(filepath.Join(stub, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{}
	for _, kv := range s.env {
		if !strings.HasPrefix(kv, "PATH=") {
			env = append(env, kv)
		}
	}
	m.env = append(env, "PATH="+stub+":"+filepath.Dir(jq)+":/usr/bin:/bin:/usr/sbin:/sbin",
		"FAKE_LAUNCHD="+m.launchd, "AIMEM_BIN_DIR="+s.bin, "AIMEM_UPGRADE_WAIT=20")
	t.Cleanup(func() { m.stopServe() })
	return m
}

// stopServe stops the serve the fake launchd started, by its PID.
func (m *macSandbox) stopServe() {
	b, err := os.ReadFile(filepath.Join(m.launchd, "io.github.blackvs.aimem.pid"))
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
		if p, err := os.FindProcess(pid); err == nil {
			p.Signal(syscall.SIGTERM)
		}
	}
}

// run runs install.sh with args, the release binary as the prebuilt one.
func (m *macSandbox) run(t *testing.T, release string, extra []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{filepath.Join(repoRoot, "install.sh")}, args...)...)
	cmd.Env = append(append([]string{}, m.env...), extra...)
	if release != "" {
		cmd.Env = append(cmd.Env, "AIMEM_PREBUILT="+binary(t, release))
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

func (m *macSandbox) plist(label string) string { return filepath.Join(m.agents, label+".plist") }

func (m *macSandbox) loaded(label string) bool {
	return exists(filepath.Join(m.launchd, label+".loaded"))
}

func (m *macSandbox) calls() string {
	b, _ := os.ReadFile(filepath.Join(m.launchd, "calls"))
	return string(b)
}

// health returns the version of the service that answers, or "" when
// none does.
func (m *macSandbox) health() string {
	cmd := exec.Command(m.installed(), "health")
	cmd.Env = m.env
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	var h struct{ Version string }
	if json.Unmarshal(out, &h) != nil {
		return ""
	}
	return h.Version
}

// checkPlist: the file is a well-formed property list, and macOS's own
// checker accepts it where there is one.
func checkPlist(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no agent plist: %v", err)
	}
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = true
	for {
		if _, err := d.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("%s is not well-formed XML: %v\n%s", path, err, b)
		}
	}
	if plutil, err := exec.LookPath("plutil"); err == nil && runtime.GOOS == "darwin" {
		if out, err := exec.Command(plutil, "-lint", path).CombinedOutput(); err != nil {
			t.Fatalf("plutil rejects %s: %v\n%s", path, err, out)
		}
	}
	return string(b)
}

const (
	serveLabel = "io.github.blackvs.aimem"
	syncLabel  = "io.github.blackvs.aimem.sync"
)

// On macOS the installer runs aimem serve and the periodic sync as launchd
// user agents: a fresh install starts the service, a re-run upgrades it in
// place through the agents, enable-sync repoints the sync agent and an
// upgrade keeps that, and uninstall unloads and removes both.
func TestShellInstallManagesLaunchdAgents(t *testing.T) {
	shellOnly(t)
	m := newMacSandbox(t, "none")

	out, code := m.run(t, "v0.0.1", nil, "user")
	if code != 0 {
		t.Fatalf("fresh install failed (exit %d):\n%s", code, out)
	}
	serve := checkPlist(t, m.plist(serveLabel))
	for _, want := range []string{"<string>" + m.installed() + "</string>", "<string>serve</string>",
		"<key>RunAtLoad</key>", "<key>KeepAlive</key>", "<key>SuccessfulExit</key>", "<key>Umask</key>\n  <integer>63</integer>"} {
		if !strings.Contains(serve, want) {
			t.Errorf("the serve agent lacks %s:\n%s", want, serve)
		}
	}
	sync := checkPlist(t, m.plist(syncLabel))
	for _, want := range []string{"<string>sync</string>", "<string>--all-hubs</string>", "<key>StartInterval</key>\n  <integer>600</integer>"} {
		if !strings.Contains(sync, want) {
			t.Errorf("the sync agent lacks %s:\n%s", want, sync)
		}
	}
	if !m.loaded(serveLabel) || !m.loaded(syncLabel) {
		t.Fatalf("the agents are not loaded after a fresh install:\n%s\ncalls:\n%s", out, m.calls())
	}
	if !strings.Contains(out, "service healthy") || m.health() != "v0.0.1" {
		t.Fatalf("the service does not answer after a fresh install:\n%s\nhealth: %s", out, m.health())
	}

	// enable-sync points the sync agent at one hub; the options are
	// written as XML text.
	out, code = m.run(t, "", []string{"AIMEM_SSH_OPTS=-o A=1 -i k&<x>"}, "enable-sync", "aimem@hub.example")
	if code != 0 {
		t.Fatalf("enable-sync failed (exit %d):\n%s", code, out)
	}
	sync = checkPlist(t, m.plist(syncLabel))
	if !strings.Contains(sync, "<string>aimem@hub.example</string>") || !strings.Contains(sync, "k&amp;&lt;x&gt;") {
		t.Errorf("enable-sync did not write the destination and options:\n%s", sync)
	}

	out, code = m.run(t, "v0.0.2", nil, "user")
	if code != 0 {
		t.Fatalf("re-running the install failed (exit %d):\n%s\ncalls:\n%s", code, out, m.calls())
	}
	if !strings.Contains(out, "upgraded aimem v0.0.1 -> v0.0.2; service healthy at v0.0.2") {
		t.Errorf("the re-run did not upgrade through the agents:\n%s", out)
	}
	if strings.Contains(out, "does not manage") || strings.Contains(out, "started by hand") {
		t.Errorf("the re-run refused the agent's own service:\n%s", out)
	}
	if !strings.Contains(checkPlist(t, m.plist(syncLabel)), "aimem@hub.example") {
		t.Error("the upgrade replaced the sync agent enable-sync had pointed at one hub")
	}
	if !m.loaded(syncLabel) {
		t.Error("the sync agent is not loaded after the upgrade")
	}

	out, code = m.run(t, "", nil, "uninstall-user")
	if code != 0 {
		t.Fatalf("uninstall failed (exit %d):\n%s", code, out)
	}
	if m.loaded(serveLabel) || m.loaded(syncLabel) || exists(m.plist(serveLabel)) || exists(m.plist(syncLabel)) {
		t.Errorf("uninstall left an agent loaded or a plist behind\ncalls:\n%s", m.calls())
	}
	if m.health() != "" {
		t.Error("the service still answers after uninstall")
	}
}

// A serve started by hand holds the socket the agent needs. The installer
// stops nothing by name: it refuses, says how, and changes nothing.
func TestShellInstallRefusesAServeStartedByHand(t *testing.T) {
	shellOnly(t)
	m := newMacSandbox(t, "v0.0.1")
	serve := exec.Command(m.installed(), "serve")
	serve.Env = m.env
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serve.Process.Signal(syscall.SIGTERM); serve.Wait() })
	for i := 0; m.health() == ""; i++ {
		if i > 100 {
			t.Fatal("the hand-started serve did not come up")
		}
		time.Sleep(100 * time.Millisecond)
	}
	out, code := m.run(t, "v0.0.2", nil, "user")
	if code == 0 || !strings.Contains(out, "aimem serve started by hand is running") {
		t.Fatalf("the install did not refuse a serve started by hand (exit %d):\n%s", code, out)
	}
	if got := m.version(t); got != "aimem v0.0.1" {
		t.Errorf("the refused install changed the binary to %q", got)
	}
	if m.loaded(serveLabel) || exists(filepath.Join(m.bin, "aimem.new")) {
		t.Errorf("the refused install loaded the agent or left aimem.new\ncalls:\n%s", m.calls())
	}
}

// With the skip knob the installer keeps the manual mode and says so; with
// no login session it writes the agents for the next login and loads none.
func TestShellInstallLaunchdSkipAndNoSession(t *testing.T) {
	shellOnly(t)
	for _, tc := range []struct {
		name, env, say string
		plists         bool
	}{
		{"AIMEM_NO_SERVICE", "AIMEM_NO_SERVICE=1", "skipping the service manager", false},
		{"AIMEM_NO_SYSTEMD", "AIMEM_NO_SYSTEMD=1", "skipping the service manager", false},
		{"no login session", "FAKE_NO_GUI=1", "the agents load at your next login", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMacSandbox(t, "none")
			out, code := m.run(t, "v0.0.1", []string{tc.env}, "user")
			if code != 0 || !strings.Contains(out, tc.say) {
				t.Fatalf("exit %d, want %q in:\n%s", code, tc.say, out)
			}
			if exists(m.plist(serveLabel)) != tc.plists || exists(m.plist(syncLabel)) != tc.plists {
				t.Errorf("plists written: %v, want %v", exists(m.plist(serveLabel)), tc.plists)
			}
			if c := m.calls(); strings.Contains(c, "bootstrap") {
				t.Errorf("an agent was loaded:\n%s", c)
			}
			if m.health() != "" {
				t.Error("a service answers in the manual mode")
			}
		})
	}
}
