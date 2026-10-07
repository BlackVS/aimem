package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runShellUpgrade runs install.sh's upgrade transaction in bash against the
// sandbox: the old release serves the state root (a real `aimem serve`,
// started and stopped by its own PID in place of the systemd unit), or in
// an offline sandbox nothing serves it, a marker records the state before
// the upgrade, and the new binary goes in.
func runShellUpgrade(t *testing.T, s *sandbox, newBinary, wait string) (string, int) {
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
svc_stop() {
  if [ -n "$SVC_PID" ]; then kill "$SVC_PID" 2>/dev/null || true; wait "$SVC_PID" 2>/dev/null || true; SVC_PID=; fi
}
svc_start() { "$AIMEM_BIN" serve >>"$SANDBOX/serve.log" 2>&1 & SVC_PID=$!; }
aimem_as() { "$@"; }
trap svc_stop EXIT
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
	cmd.Env = append(s.env, "SANDBOX="+s.dir, "AIMEM_UPGRADE_WAIT="+wait)
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
