package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The scripts piped to a shell (curl | bash, the hub's as root) must run
// nothing when the download is cut short. Each defines its functions and
// constants, and runs everything from main on its last line. Every prefix
// that ends at a line boundary before that call is run under bash -x with
// an empty PATH: its trace may show only `set` and literal assignments,
// no command substitution and no other command, and it prints nothing.
// A prefix cut inside a function body is a syntax error, which runs
// nothing either.
func TestPipedScriptsRunNothingWhenCutShort(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the piped scripts run under bash on Linux and macOS; a bash start per line is slow on Windows")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	allowed := regexp.MustCompile(`^\+TRACE (set -[a-z]+( pipefail)?|[A-Z_][A-Z0-9_]*=[^$` + "`" + `]*)$`)
	for _, name := range []string{"boot.sh", "install-hub.sh"} {
		t.Run(name, func(t *testing.T) {
			text := read(t, name)
			lines := strings.SplitAfter(strings.TrimRight(text, "\n"), "\n")
			if last := strings.TrimSpace(lines[len(lines)-1]); last != `main "$@"` {
				t.Fatalf("%s ends with %q; want the call main \"$@\" as its last line", name, last)
			}
			dir := t.TempDir()
			empty := filepath.Join(dir, "empty")
			if err := os.Mkdir(empty, 0o700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "prefix.sh")
			ran := 0
			for n := 1; n < len(lines); n++ {
				if err := os.WriteFile(file, []byte(strings.Join(lines[:n], "")), 0o600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(bash, "-x", file)
				cmd.Dir = dir
				cmd.Env = []string{"PATH=" + empty, "HOME=" + dir, "PS4=+TRACE "}
				var stdout, stderr strings.Builder
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				_ = cmd.Run() // a prefix cut inside a function is a syntax error
				if stdout.Len() > 0 {
					t.Fatalf("cut after line %d, the script printed:\n%s", n, stdout.String())
				}
				for _, l := range strings.Split(stderr.String(), "\n") {
					if strings.HasPrefix(l, "+") && !allowed.MatchString(l) {
						t.Fatalf("cut after line %d, the script ran %q", n, l)
					}
				}
				ran++
			}
			if ran < 100 {
				t.Fatalf("only %d prefixes of %s were run", ran, name)
			}
		})
	}
}
