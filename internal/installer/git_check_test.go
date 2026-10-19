package installer

// The hub installer says at the start, before anything is installed or
// upgraded, when this host has no git, and goes on either way.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubInstallerChecksGitBeforeTheUpgrade(t *testing.T) {
	s := read(t, "install-hub.sh")
	check := strings.Index(s, "\ncheck_git\n")
	for _, later := range []string{"\n# --- binary ---", "upgrade_txn \"$AIMEM_BIN.new\""} {
		if i := strings.Index(s, later); check < 0 || i < 0 || check > i {
			t.Fatalf("check_git must run before %q", later)
		}
	}
}

func TestHubGitCheckWarnsOnlyWithoutGit(t *testing.T) {
	shellOnly(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	script := "set -euo pipefail\n" + extract(t, "install-hub.sh", "git-check") + "check_git\necho \"after: $?\"\n"
	for _, withGit := range []bool{false, true} {
		bin := t.TempDir()
		if withGit {
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command(bash, "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+bin)
		out, err := cmd.CombinedOutput()
		text := string(out)
		if err != nil || !strings.Contains(text, "after: 0") {
			t.Fatalf("git %v: %v\n%s", withGit, err, text)
		}
		warned := strings.Contains(text, "WARNING: git is not installed on this host") && strings.Contains(text, "forge's commit tarball over https") && strings.Contains(text, "--from-archive")
		if warned == withGit {
			t.Fatalf("git %v: warning shown %v\n%s", withGit, warned, text)
		}
	}
}
