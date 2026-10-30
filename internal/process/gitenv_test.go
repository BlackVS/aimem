package process

import (
	"context"
	"os"
	"strings"
	"testing"
)

// The git child sees none of the hub's settings or secrets (audit A-22):
// AIMEM_* and providers' keys stay behind, while PATH, the ssh agent and
// the prompt guards pass. The built command is inspected, not run, so the
// check does not depend on how fast a child starts.
func TestGitScrubsTheChild(t *testing.T) {
	t.Setenv("AIMEM_HTTP_TOKEN", "hub-admin-secret")
	t.Setenv("AIMEM_OPENAI_API_KEY", "sk-provider-secret")
	t.Setenv("OPENAI_API_KEY", "sk-other-secret")
	t.Setenv("SSH_AUTH_SOCK", "/run/agent.sock")

	cmd := gitCmd(context.Background(), "", "probe")
	if cmd.Env == nil {
		t.Fatal("the git child inherits the whole environment")
	}
	env := "\n" + strings.Join(cmd.Env, "\n") + "\n"
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
	if !strings.Contains(strings.ToUpper(env), "\nPATH="+strings.ToUpper(os.Getenv("PATH"))+"\n") {
		t.Errorf("the git child does not get this PATH:%s", env)
	}
}
