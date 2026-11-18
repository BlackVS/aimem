package installer

// The hub's hourly curation script, written by install-hub.sh, stops at
// the first step that exits 6 (the provider refused the account) and
// fails the service; any other failure is logged and the run goes on.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// curateAllScript is the curate-all.sh install-hub.sh writes.
func curateAllScript(t *testing.T) string {
	t.Helper()
	s := read(t, "install-hub.sh")
	begin := "cat > \"$HOME_DIR/.local/sbin/curate-all.sh\" <<'EOS'\n"
	i := strings.Index(s, begin)
	if i < 0 {
		t.Fatal("install-hub.sh does not write curate-all.sh")
	}
	body := s[i+len(begin):]
	j := strings.Index(body, "\nEOS\n")
	if j < 0 {
		t.Fatal("curate-all.sh has no end")
	}
	return body[:j+1]
}

func TestCurateAllStopsAtARefusal(t *testing.T) {
	shellOnly(t)
	bash, _ := exec.LookPath("bash")
	script := filepath.Join(t.TempDir(), "curate-all.sh")
	if err := os.WriteFile(script, []byte(curateAllScript(t)), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name     string
		failStep string // the aimem subcommand that fails
		failExit string
		want     string // the subcommands run, in order
		wantExit int
	}{
		{"curate refused", "curate", "6", "curate", 6},
		{"embed refused", "embed", "6", "curate embed", 6},
		{"doc refused", "doc", "6", "curate embed dedup doc", 6},
		{"curate fails otherwise", "curate", "1", "curate embed dedup doc", 0},
		{"all succeed", "", "", "curate embed dedup doc", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			bin := filepath.Join(home, ".local", "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(home, "calls")
			stub := "#!/bin/sh\necho \"$1\" >> \"" + log + "\"\n[ \"$1\" = \"$FAIL_STEP\" ] && exit \"$FAIL_EXIT\"\nexit 0\n"
			// The deep pass runs at 04 UTC: a stub date puts every step in play.
			for name, body := range map[string]string{"aimem": stub, "date": "#!/bin/sh\necho 04\n"} {
				if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(bash, script)
			cmd.Env = append(os.Environ(), "HOME="+home, "AIMEM_EMBED_MODEL=m", "AIMEM_CURATE_BACKEND=openai",
				"FAIL_STEP="+c.failStep, "FAIL_EXIT="+c.failExit)
			out, err := cmd.CombinedOutput()
			exit := 0
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				exit = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			calls, _ := os.ReadFile(log)
			if got := strings.Join(strings.Fields(string(calls)), " "); got != c.want || exit != c.wantExit {
				t.Fatalf("ran %q and exited %d; want %q and %d\n%s", got, exit, c.want, c.wantExit, out)
			}
		})
	}
}
