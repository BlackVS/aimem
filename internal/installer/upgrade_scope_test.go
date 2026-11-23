package installer

// scripts/upgrade-scope.sh decides whether a CI run runs the upgrade
// checks: always on the schedule and a manual run, on a release prepare
// pull request, and on any other pull request from this repository that
// changes the installers or the upgrade checks (task 01a208ef-65c1).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpgradeScopeDecides(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	cases := []struct {
		name, event, ref, same, files string
		run                           bool
		reason                        string
	}{
		{"schedule", "schedule", "", "true", "", true, "run: a schedule run"},
		{"manual", "workflow_dispatch", "", "true", "", true, "run: a workflow_dispatch run"},
		{"push", "push", "", "true", "install.sh", false, "skip: the upgrade checks do not run on a push event"},
		{"release prepare", "pull_request", "release/v0.12.1", "true", "CHANGELOG.md", true, "run: a release prepare pull request"},
		{"installer change", "pull_request", "fix/x", "true", "docs/A.md\ninstall.sh", true, "  install.sh"},
		{"hub installer change", "pull_request", "fix/x", "true", "install-hub.sh", true, "  install-hub.sh"},
		{"check change", "pull_request", "fix/x", "true", "scripts/check-client-upgrade.sh", true, "  scripts/check-client-upgrade.sh"},
		{"unrelated change", "pull_request", "fix/x", "true", "internal/adapter/spool.go\nCHANGELOG.md", false, "skip: the pull request changes none"},
		{"look-alike path", "pull_request", "fix/x", "true", "docs/install.sh.md\nscripts/install.sh", false, "skip: the pull request changes none"},
		{"fork", "pull_request", "release/v9.9.9", "false", "install.sh", false, "skip: a pull request from a fork"},
	}
	script, err := filepath.Abs(filepath.Join(repoRoot, "scripts", "upgrade-scope.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "output")
			cmd := exec.Command(bash, script)
			cmd.Env = append(os.Environ(),
				"EVENT="+c.event, "HEAD_REF="+c.ref, "SAME_REPO="+c.same,
				"UPGRADE_SCOPE_FILES="+c.files, "GITHUB_OUTPUT="+out)
			log, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("upgrade-scope.sh: %v\n%s", err, log)
			}
			if !strings.Contains(string(log), c.reason) {
				t.Errorf("log %q does not give the reason %q", log, c.reason)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			want := "run=false\n"
			if c.run {
				want = "run=true\n"
			}
			if string(got) != want {
				t.Errorf("output %q, want %q (log %q)", got, want, log)
			}
		})
	}
}
