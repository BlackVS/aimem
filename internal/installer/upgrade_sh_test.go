package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// runShellUpgrade runs install.sh's upgrade transaction in bash against the
// sandbox: the old release serves the state root (a real `aimem serve`,
// started and stopped by its own PID in place of the systemd unit), or in
// an offline sandbox nothing serves it, a marker records the state before
// the upgrade, and the new binary goes in.
func runShellUpgrade(t *testing.T, s *sandbox, newBinary, wait string, env ...string) (string, int) {
	t.Helper()
	newCopy := filepath.Join(s.bin, "aimem.new")
	b, err := os.ReadFile(newBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newCopy, b, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "set -euo pipefail\n" + extract(t, "install.sh", "upgrade-transaction") + `
SVC_PID=
svc_kill() {
  if [ -n "$SVC_PID" ]; then kill "$SVC_PID" 2>/dev/null || true; wait "$SVC_PID" 2>/dev/null || true; SVC_PID=; fi
}
# STOP_FAILS=1: a stop that leaves the service running, as a stop whose
# error svc_stop swallowed would.
svc_stop() { [ "${STOP_FAILS:-}" = 1 ] || svc_kill; }
svc_start() { [ -n "$SVC_PID" ] || { "$AIMEM_BIN" serve >>"$SANDBOX/serve.log" 2>&1 & SVC_PID=$!; }; }
aimem_as() { "$@"; }
trap svc_kill EXIT
TXN_MANAGED=1
AIMEM_BIN=$SANDBOX/bin/aimem
STATE_ROOT_DEFAULT=$HOME/.local/state/aimem
if [ "${OFFLINE:-}" = 1 ]; then
  mkdir -p "$STATE"
else
  svc_start
  AIMEM_UPGRADE_WAIT=20 txn_wait v0.0.1 || { echo "the old release did not come up"; cat "$SANDBOX/serve.log"; exit 99; }
fi
echo before > "$STATE/marker"
rc=0
upgrade_txn "$SANDBOX/bin/aimem.new" || rc=$?
exit "$rc"
`
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(append(s.env, "SANDBOX="+s.dir, "AIMEM_UPGRADE_WAIT="+wait), env...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if code == 99 {
		t.Fatalf("setup failed:\n%s", out)
	}
	return string(out), code
}

func shellOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh and install-hub.sh run on Linux and macOS; install.ps1 has its own test")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
}

func TestShellUpgradeKeepsBackupAndReportsBothVersions(t *testing.T) {
	shellOnly(t)
	for _, offline := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "stopped"}[offline], func(t *testing.T) {
			s := newSandbox(t, binary(t, "v0.0.1"), offline)
			out, code := runShellUpgrade(t, s, binary(t, "v0.0.2"), "20")
			if code != 0 {
				t.Fatalf("upgrade failed (exit %d):\n%s", code, out)
			}
			checkSuccess(t, s, out)
		})
	}
}

func TestShellUpgradeRollsBackBinaryAndState(t *testing.T) {
	shellOnly(t)
	for _, offline := range []bool{false, true} {
		t.Run(map[bool]string{false: "running", true: "stopped"}[offline], func(t *testing.T) {
			s := newSandbox(t, binary(t, "v0.0.1"), offline)
			out, code := runShellUpgrade(t, s, binary(t, "broken"), "15")
			if code == 0 {
				t.Fatalf("an upgrade to a release that cannot start reported success:\n%s", out)
			}
			checkRolledBack(t, s, out)
		})
	}
}

// On a fresh hub the service user has no configuration yet. The hub's
// service hooks run before the binary is downloaded, under the script's
// `set -euo pipefail`, so they must not depend on any of it existing.
func TestHubServiceHooksNeedNoConfiguration(t *testing.T) {
	shellOnly(t)
	script := "set -euo pipefail\nHOME_DIR=" + t.TempDir() + "/fresh\nHUB_USER=nobody\n" +
		extract(t, "install-hub.sh", "hub-service-hooks") + "echo \"reached $STATE_ROOT_DEFAULT\"\n"
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "reached ") {
		t.Fatalf("the hub's service hooks stop a fresh install: %v\n%s", err, out)
	}
}

// changingTar puts a tar in front of the real one whose first `fails`
// archive runs report what GNU tar reports when a file changes under it:
// the warning, a complete archive, and exit 1. It returns the environment
// that puts it on PATH.
func changingTar(t *testing.T, s *sandbox, fails int) []string {
	t.Helper()
	real, err := exec.LookPath("tar")
	if err != nil {
		t.Skip("no tar")
	}
	dir := filepath.Join(s.dir, "faketar")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ "$1" = -cf ]; then
  n=$(cat "` + dir + `/count" 2>/dev/null || echo 0); n=$((n + 1)); echo "$n" > "` + dir + `/count"
  if [ "$n" -le ` + strconv.Itoa(fails) + ` ]; then
    "` + real + `" "$@" || exit 2
    echo "tar: ./projects/p: file changed as we read it" >&2
    exit 1
  fi
fi
exec "` + real + `" "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "tar"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{"PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")}
}

// A process of the service's user that writes the state root during the
// copy makes tar warn and exit 1. The copy is taken again from an empty
// directory, and the upgrade goes through on a copy tar read unchanged.
func TestShellUpgradeCopiesAgainWhenTheStateChangesDuringTheCopy(t *testing.T) {
	shellOnly(t)
	s := newSandbox(t, binary(t, "v0.0.1"), false)
	out, code := runShellUpgrade(t, s, binary(t, "v0.0.2"), "20", changingTar(t, s, 2)...)
	if code != 0 {
		t.Fatalf("upgrade failed (exit %d):\n%s", code, out)
	}
	checkSuccess(t, s, out)
	for _, want := range []string{"stopping the service for the state backup (", "the copy failed (try 1 of 3)", "the copy failed (try 2 of 3)"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
}

// A copy that changes on every try is never kept, not even though tar
// only warned: the upgrade stops with tar's own message, the state root
// and the installed release are as they were, and no backup is left.
func TestShellUpgradeRefusesAStateRootThatKeepsChanging(t *testing.T) {
	shellOnly(t)
	s := newSandbox(t, binary(t, "v0.0.1"), false)
	out, code := runShellUpgrade(t, s, binary(t, "v0.0.2"), "20", changingTar(t, s, 3)...)
	if code == 0 {
		t.Fatalf("an upgrade whose state copy changed on every try reported success:\n%s", out)
	}
	checkUntouched(t, s, out)
	for _, want := range []string{"file changed as we read it", "the copy failed (try 2 of 3)", "could not copy the state root", "nothing was changed", "lsof +D "} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
}

// A stop that leaves the service answering is caught before the copy.
func TestShellUpgradeRefusesWhileTheServiceStillAnswers(t *testing.T) {
	shellOnly(t)
	s := newSandbox(t, binary(t, "v0.0.1"), false)
	out, code := runShellUpgrade(t, s, binary(t, "v0.0.2"), "20", "STOP_FAILS=1")
	if code == 0 {
		t.Fatalf("an upgrade with the service still running reported success:\n%s", out)
	}
	checkUntouched(t, s, out)
	if !strings.Contains(out, "an aimem serve still answers after the service was stopped") {
		t.Errorf("the output does not say the service did not stop:\n%s", out)
	}
}

// checkUntouched: a refused upgrade left the previous release, the state
// root as it was, and no backup.
func checkUntouched(t *testing.T, s *sandbox, out string) {
	t.Helper()
	if got := s.version(t); got != "aimem v0.0.1" {
		t.Errorf("installed binary reports %q after a refused upgrade; want aimem v0.0.1\n%s", got, out)
	}
	if b, err := os.ReadFile(filepath.Join(s.state, "marker")); err != nil || string(b) != "before\n" {
		t.Errorf("state root lost its marker: %q %v", b, err)
	}
	if b := s.siblings(t, ".backup-"); len(b) != 0 {
		t.Errorf("a refused upgrade left backups %v", b)
	}
}
