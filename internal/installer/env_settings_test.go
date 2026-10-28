package installer

// install-hub.sh's env settings: AIMEM_INTROSPECTION_TOKEN_FILE is written
// into the env file on a fresh install and on an upgrade, a line the file
// already holds is kept and never duplicated, and an unset setting leaves
// the file as it was. scripts/check-install-hub-container.sh runs the
// whole installer with the setting.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const introKey = "AIMEM_INTROSPECTION_TOKEN_FILE"

// envSetting runs env_setting on an env file holding before and returns
// the file afterwards and what was printed.
func envSetting(t *testing.T, before, value string) (after, out string) {
	t.Helper()
	shellOnly(t)
	envf := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envf, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	s := "set -euo pipefail\n" + extract(t, "install-hub.sh", "env-settings") + `env_setting "$1" ` + introKey + ` "$2"` + "\n"
	o, err := exec.Command("bash", "-c", s, "env-setting", envf, value).CombinedOutput()
	if err != nil {
		t.Fatalf("env_setting: %v\n%s", err, o)
	}
	raw, err := os.ReadFile(envf)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), string(o)
}

func TestHubEnvSettingIsWrittenOnceAndNeverClobbered(t *testing.T) {
	const base = "AIMEM_HTTP_LISTEN=:8440\nAIMEM_HTTP_TOKEN=x\n"
	const path = "/home/sessiond/.config/aimem/introspection.token"
	line := introKey + "=" + path + "\n"

	// A fresh env file, or one from an upgrade without the line: appended.
	if after, out := envSetting(t, base, path); after != base+line || !strings.Contains(out, "wrote "+introKey) {
		t.Fatalf("written:\n%s\n%s", after, out)
	}
	// A file without a final newline gets one before the line.
	if after, _ := envSetting(t, strings.TrimSuffix(base, "\n"), path); after != base+line {
		t.Fatalf("no final newline:\n%q", after)
	}
	// An upgrade that passes the same value again: kept, one line.
	if after, out := envSetting(t, base+line, path); after != base+line || !strings.Contains(out, "kept "+introKey) {
		t.Fatalf("same value:\n%s\n%s", after, out)
	}
	// A line the operator wrote, indented or with another value, is the
	// operator's: kept as it is, with a warning for a different value.
	own := base + "  " + introKey + "=/srv/other.token\n"
	if after, out := envSetting(t, own, path); after != own || !strings.Contains(out, "WARNING:") || !strings.Contains(out, "kept it, not "+path) {
		t.Fatalf("another value:\n%s\n%s", after, out)
	}
	// A commented-out line is not a setting.
	commented := base + "# " + introKey + "=/old\n"
	if after, _ := envSetting(t, commented, path); after != commented+line {
		t.Fatalf("commented line:\n%s", after)
	}
	// Unset: the file is byte for byte as before.
	if after, out := envSetting(t, base, ""); after != base || out != "" {
		t.Fatalf("unset changed the file:\n%s\n%s", after, out)
	}
}

func TestHubEnvSettingCheckRefusesWhatTheEnvFileCannotHold(t *testing.T) {
	shellOnly(t)
	check := func(v string) (string, error) {
		s := "set -euo pipefail\n" + extract(t, "install-hub.sh", "env-settings") + `env_setting_check ` + introKey + ` "$1"` + "\n"
		o, err := exec.Command("bash", "-c", s, "check", v).CombinedOutput()
		return string(o), err
	}
	for _, ok := range []string{"", "/home/sessiond/.config/aimem/introspection.token", "/srv/a_b-c.d/t"} {
		if out, err := check(ok); err != nil {
			t.Fatalf("%q refused: %v\n%s", ok, err, out)
		}
	}
	for _, bad := range []string{"relative/path", "~/token", "/with space", "/q\"uote", "/semi;colon", "/dollar$HOME", "/new\nline"} {
		if out, err := check(bad); err == nil || !strings.Contains(out, "Nothing was changed") {
			t.Fatalf("%q accepted: %v\n%s", bad, err, out)
		}
	}
	// The check runs before anything changes: right after the settings are
	// read, before the binary is staged.
	s := read(t, "install-hub.sh")
	call := strings.Index(s, "env_setting_check "+introKey)
	if call < 0 || call > strings.Index(s, "# --- binary ---") {
		t.Fatal("the setting is not checked before the binary is staged")
	}
	if strings.Count(s, `env_setting "$ENVF" `+introKey) != 1 {
		t.Fatal("the env file is not given the setting exactly once")
	}
}
