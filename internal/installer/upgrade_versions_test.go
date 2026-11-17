package installer

// scripts/upgrade-versions.sh derives the previous release from the tags
// and asks GitHub whether each one is published. Only "not found" moves
// to the tag before it; any other answer, or a failed request, is asked
// once more and then ends the run (task 01a1e9b0-4134, comment seq 562).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpgradeVersionsFallsBackOnlyOnNotFound(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	script := read(t, filepath.Join("scripts", "upgrade-versions.sh"))

	// A repository with the script, two released tags and an untagged head.
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scripts", "upgrade-versions.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "work")
	for _, tag := range []string{"v0.1.0", "v0.2.0", ""} {
		git("commit", "-q", "--allow-empty", "-m", "c"+tag)
		if tag != "" {
			git("tag", tag)
		}
	}

	// A stand-in curl: the answers for a tag are read from answers/<tag>,
	// one HTTP status per request (000 = the request failed, curl exits 7),
	// and every request is logged. sleep returns at once.
	stub := t.TempDir()
	for name, body := range map[string]string{
		"curl": `#!/bin/sh
url=
for a; do url=$a; done
tag=$(echo "$url" | sed -E 's#.*/download/([^/]+)/SHA256SUMS$#\1#')
echo "$tag" >> "$STUB/requests"
n=$(grep -cx "$tag" "$STUB/requests")
code=$(sed -n "${n}p" "$STUB/answers/$tag" 2>/dev/null)
[ -n "$code" ] || code=$(tail -n 1 "$STUB/answers/$tag")
printf '%s' "$code"
[ "$code" = 000 ] && exit 7
exit 0
`,
		"sleep": "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(filepath.Join(stub, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	for _, c := range []struct {
		name     string
		from     string            // AIMEM_UPGRADE_FROM, a manual run's named release
		answers  map[string]string // tag: statuses, one per line
		want     string            // stdout on success
		fail     string            // stderr on failure
		requests string            // the tags asked, in order
	}{
		{name: "named by hand", from: "v0.1.0", answers: map[string]string{"v0.2.0": "200", "v0.1.0": "200"}, want: "FROM=v0.1.0\nTO=v0.2.1\n", requests: "v0.2.0 v0.1.0"},
		{name: "named but not published", from: "v0.1.0", answers: map[string]string{"v0.2.0": "200", "v0.1.0": "404"}, fail: "the named release v0.1.0 is not published", requests: "v0.2.0 v0.1.0"},
		{name: "named but not a tag", from: "v0.0.9", answers: map[string]string{"v0.2.0": "200"}, fail: "v0.0.9 is not a tag reachable", requests: "v0.2.0"},
		{"published", "", map[string]string{"v0.2.0": "200"}, "FROM=v0.2.0\nTO=v0.2.1\n", "", "v0.2.0"},
		{"not found moves on", "", map[string]string{"v0.2.0": "404", "v0.1.0": "200"}, "FROM=v0.1.0\nTO=v0.1.1\n", "", "v0.2.0 v0.1.0"},
		{"a server error once is asked again", "", map[string]string{"v0.2.0": "503\n200"}, "FROM=v0.2.0\nTO=v0.2.1\n", "", "v0.2.0 v0.2.0"},
		{"a server error twice ends the run", "", map[string]string{"v0.2.0": "503", "v0.1.0": "200"}, "", "answered HTTP 503 twice", "v0.2.0 v0.2.0"},
		{"a rate limit ends the run", "", map[string]string{"v0.2.0": "429", "v0.1.0": "200"}, "", "answered HTTP 429 twice", "v0.2.0 v0.2.0"},
		{"a failed request ends the run", "", map[string]string{"v0.2.0": "000", "v0.1.0": "200"}, "", "answered HTTP 000 twice", "v0.2.0 v0.2.0"},
		{"nothing published", "", map[string]string{"v0.2.0": "404", "v0.1.0": "404"}, "", "has a published release", "v0.2.0 v0.1.0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := t.TempDir()
			if err := os.MkdirAll(filepath.Join(state, "answers"), 0o755); err != nil {
				t.Fatal(err)
			}
			for tag, a := range c.answers {
				if err := os.WriteFile(filepath.Join(state, "answers", tag), []byte(a+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(bash, "-c", `PATH="$(cd "$STUBBIN" && pwd):$PATH" STUB="$(cd "$STUB" && pwd)" exec bash scripts/upgrade-versions.sh`)
			cmd.Dir = repo
			cmd.Env = append(os.Environ(), "STUBBIN="+stub, "STUB="+state, "GITHUB_HEAD_REF=", "GITHUB_REF_NAME=", "AIMEM_UPGRADE_FROM="+c.from)
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if c.fail == "" {
				if err != nil || stdout.String() != c.want || (c.from != "") != strings.Contains(stderr.String(), "named by hand") {
					t.Fatalf("got %v, stdout %q, stderr %q; want %q", err, stdout.String(), stderr.String(), c.want)
				}
			} else {
				if err == nil || stdout.String() != "" || !strings.Contains(stderr.String(), c.fail) {
					t.Fatalf("got %v, stdout %q, stderr %q; want a failure naming %q", err, stdout.String(), stderr.String(), c.fail)
				}
				if strings.Contains(c.fail, "twice") && !strings.Contains(stderr.String(), "not falling back to an older tag") {
					t.Fatalf("the failure does not say it did not fall back: %q", stderr.String())
				}
			}
			b, _ := os.ReadFile(filepath.Join(state, "requests"))
			if got := strings.Join(strings.Fields(string(b)), " "); got != c.requests {
				t.Fatalf("asked %q, want %q", got, c.requests)
			}
		})
	}
}
