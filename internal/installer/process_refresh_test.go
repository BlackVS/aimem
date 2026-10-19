package installer

// The hub installer stores the process sets of old selections after an
// upgrade (aimem process refresh), once the new hub answers its health
// check, and never fails the upgrade over it.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubInstallerRefreshesProcessSetsAfterAnUpgrade(t *testing.T) {
	s := read(t, "install-hub.sh")
	health := strings.Index(s, `/v1/health" >/dev/null; then`)
	call := strings.Index(s, `refresh_processes "$AIMEM_BIN"`)
	if health < 0 || call < 0 || call < health {
		t.Fatal("install-hub.sh must call refresh_processes after the health check")
	}
	// Inside the healthy branch, and only on an upgrade.
	between := s[health:call]
	if strings.Contains(between, "\nelse") || !strings.Contains(between, `if [ -n "$UPGRADED" ]; then`) {
		t.Fatalf("refresh_processes must run in the healthy branch, gated on an upgrade:\n%s", between)
	}
	if strings.Count(s, "UPGRADED=1") != 1 || !strings.Contains(s, "UPGRADED=1\n  upgrade_txn ") {
		t.Fatal("UPGRADED must be set only on the upgrade path, before upgrade_txn")
	}
}

func TestHubProcessRefreshReportsAndNeverFailsTheUpgrade(t *testing.T) {
	shellOnly(t)
	dir := t.TempDir()
	fake := filepath.Join(dir, "aimem")
	if err := os.WriteFile(fake, []byte("#!/usr/bin/env bash\necho \"aimem $*\"\necho \"legacy: refreshed\"\nexit \"${FAKE_RC:-0}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rc := range []string{"0", "3"} {
		script := "set -uo pipefail\nHUB_USER=hub\naimem_as() { \"$@\"; }\n" +
			extract(t, "install-hub.sh", "process-refresh") +
			"refresh_processes \"$FAKE\"\necho \"after: $?\"\n"
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = append(os.Environ(), "FAKE="+fake, "FAKE_RC="+rc)
		out, err := cmd.CombinedOutput()
		text := string(out)
		if err != nil || !strings.Contains(text, "aimem process refresh\n") || !strings.Contains(text, "legacy: refreshed") || !strings.Contains(text, "after: 0") {
			t.Fatalf("rc %s: %v\n%s", rc, err, text)
		}
		if warned := strings.Contains(text, "WARNING: aimem process refresh did not complete"); warned != (rc != "0") {
			t.Fatalf("rc %s: warning shown %v\n%s", rc, warned, text)
		}
	}
}
