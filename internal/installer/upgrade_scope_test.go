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

// TestUpgradeScopeReadsTheRealDiff runs the decision on a real repository's
// diff, as CI does: an installer moved or deleted counts by its old path
// (rename detection is off), a copy changes nothing the upgrade runs, and a
// base that cannot be read fails the run instead of reading as a skip.
func TestUpgradeScopeReadsTheRealDiff(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, rel), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write(filepath.Join("scripts", "upgrade-scope.sh"), read(t, filepath.Join("scripts", "upgrade-scope.sh")))
	// Enough lines that a move is detected as a rename by default.
	write("install.sh", strings.Repeat("echo installing\n", 40))
	write("README.md", "readme\n")
	git("init", "-q", "-b", "master")
	git("add", ".")
	git("commit", "-q", "-m", "base")
	base := git("rev-parse", "HEAD")

	branch := func(name string, change func()) string {
		t.Helper()
		git("checkout", "-q", "-b", name, base)
		change()
		git("add", "-A")
		git("commit", "-q", "-m", name)
		head := git("rev-parse", "HEAD")
		git("checkout", "-q", base)
		return head
	}
	moved := branch("moved", func() { git("mv", "install.sh", "install-client.sh") })
	deleted := branch("deleted", func() { git("rm", "-q", "install.sh") })
	copied := branch("copied", func() { write("install-copy.sh", strings.Repeat("echo installing\n", 40)) })
	other := branch("other", func() { write("README.md", "changed\n") })

	run := func(baseSHA, headSHA string) (string, string, error) {
		out := filepath.Join(t.TempDir(), "output")
		cmd := exec.Command(bash, filepath.Join(repo, "scripts", "upgrade-scope.sh"))
		cmd.Env = append(os.Environ(), "EVENT=pull_request", "HEAD_REF=fix/x", "SAME_REPO=true",
			"BASE_SHA="+baseSHA, "HEAD_SHA="+headSHA, "GITHUB_OUTPUT="+out)
		log, err := cmd.CombinedOutput()
		got, _ := os.ReadFile(out)
		return string(log), string(got), err
	}
	for _, c := range []struct {
		name, head, want, reason string
	}{
		{"moved installer", moved, "run=true\n", "  install.sh"},
		{"deleted installer", deleted, "run=true\n", "  install.sh"},
		{"copied installer", copied, "run=false\n", "skip: the pull request changes none"},
		{"unrelated change", other, "run=false\n", "skip: the pull request changes none"},
	} {
		log, got, err := run(base, c.head)
		if err != nil {
			t.Fatalf("%s: upgrade-scope.sh: %v\n%s", c.name, err, log)
		}
		if got != c.want || !strings.Contains(log, c.reason) {
			t.Errorf("%s: output %q, log %q; want %q and %q", c.name, got, log, c.want, c.reason)
		}
	}
	// A base the checkout does not hold: the diff fails, and so does the run.
	log, got, err := run(strings.Repeat("0", 40), other)
	if err == nil || got != "" {
		t.Errorf("an unreadable base: exit %v, output %q, log %q; want a failure and no output", err, got, log)
	}
}
