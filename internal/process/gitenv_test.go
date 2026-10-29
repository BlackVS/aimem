package process

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMain doubles as a stub git: a copy of the test binary named git
// prints the environment it was started with.
func TestMain(m *testing.M) {
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == "git" {
		if len(os.Args) > 1 && os.Args[1] == "config" {
			os.Exit(1) // no core.sshCommand configured
		}
		fmt.Print(strings.Join(os.Environ(), "\n") + "\n")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The git child sees none of the hub's settings or secrets (audit A-22):
// AIMEM_* and providers' keys stay behind, while PATH, the ssh agent and
// the prompt guards pass.
func TestGitScrubsTheChild(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	stub := filepath.Join(bin, "git")
	if runtime.GOOS == "windows" {
		stub += ".exe"
	}
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(stub, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AIMEM_HTTP_TOKEN", "hub-admin-secret")
	t.Setenv("AIMEM_OPENAI_API_KEY", "sk-provider-secret")
	t.Setenv("OPENAI_API_KEY", "sk-other-secret")
	t.Setenv("SSH_AUTH_SOCK", "/run/agent.sock")

	out, err := git(context.Background(), t.TempDir(), "probe")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	env := "\n" + string(out)
	for _, gone := range []string{"AIMEM_HTTP_TOKEN", "hub-admin-secret", "sk-provider-secret", "sk-other-secret"} {
		if strings.Contains(env, gone) {
			t.Errorf("the git child sees %q:%s", gone, env)
		}
	}
	for _, kept := range []string{"\nSSH_AUTH_SOCK=/run/agent.sock\n", "\nGIT_TERMINAL_PROMPT=0\n", "\nGCM_INTERACTIVE=never\n"} {
		if !strings.Contains(env, kept) {
			t.Errorf("the git child lacks %q:%s", kept, env)
		}
	}
	if !strings.Contains(strings.ToUpper(env), "\nPATH=") {
		t.Errorf("the git child has no PATH:%s", env)
	}
}
