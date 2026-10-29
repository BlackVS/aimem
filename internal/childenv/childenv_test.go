package childenv

import (
	"strings"
	"testing"
)

func TestMinimal(t *testing.T) {
	t.Setenv("AIMEM_HTTP_TOKEN", "admin-secret")
	t.Setenv("AIMEM_OPENAI_API_KEY", "sk-provider")
	t.Setenv("OPENAI_API_KEY", "sk-other")
	t.Setenv("CHILDENV_EXTRA", "kept")
	t.Setenv("LC_ALL", "C.UTF-8")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	t.Setenv("CHILDENV_PRE_ONE", "prefixed")
	t.Setenv("CHILDENV_PREX", "not-prefixed")
	env := strings.Join(Minimal("CHILDENV_EXTRA", "AIMEM_HTTP_TOKEN", "AIMEM_*", "CHILDENV_PRE_*"), "\n") + "\n"
	for _, gone := range []string{"admin-secret", "sk-provider", "sk-other", "not-prefixed"} {
		if strings.Contains(env, gone) {
			t.Errorf("%q passed:\n%s", gone, env)
		}
	}
	for _, kept := range []string{"CHILDENV_EXTRA=kept\n", "CHILDENV_PRE_ONE=prefixed\n", "LC_ALL=C.UTF-8\n", "HTTPS_PROXY=http://proxy.invalid:3128\n"} {
		if !strings.Contains(env, kept) {
			t.Errorf("%q missing:\n%s", kept, env)
		}
	}
	if !strings.Contains("\n"+env, "\nPATH=") && !strings.Contains("\n"+env, "\nPath=") {
		t.Errorf("PATH missing:\n%s", env)
	}
}
