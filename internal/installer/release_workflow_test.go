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

// workflowJobs splits a workflow into its jobs: the text under each
// two-space key below `jobs:`.
func workflowJobs(t *testing.T, wf string) map[string]string {
	t.Helper()
	_, body, ok := strings.Cut(wf, "\njobs:\n")
	if !ok {
		t.Fatal("the workflow has no jobs")
	}
	jobs := map[string]string{}
	key := regexp.MustCompile(`(?m)^  ([A-Za-z0-9_-]+):\n`)
	idx := key.FindAllStringSubmatchIndex(body, -1)
	for i, m := range idx {
		end := len(body)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		jobs[body[m[2]:m[3]]] = body[m[0]:end]
	}
	return jobs
}

// stepRun is the `run: |` script of the step named name in job, dedented.
func stepRun(t *testing.T, job, name string) string {
	t.Helper()
	i := strings.Index(job, "      - name: "+name+"\n")
	if i < 0 {
		t.Fatalf("no step %q", name)
	}
	step := job[i+len("      - name: "+name+"\n"):]
	if j := strings.Index(step, "\n      - "); j >= 0 {
		step = step[:j+1]
	}
	_, script, ok := strings.Cut(step, "        run: |\n")
	if !ok {
		t.Fatalf("step %q has no run block", name)
	}
	var out strings.Builder
	for _, l := range strings.SplitAfter(script, "\n") {
		out.WriteString(strings.TrimPrefix(l, "          "))
	}
	return out.String()
}

// The release's tests and build run in a job that can write nothing and
// cannot sign; the job that holds those permissions checks out nothing,
// so no code from the repository runs beside the OIDC token.
func TestReleaseBuildsWithoutWritePermissions(t *testing.T) {
	jobs := workflowJobs(t, read(t, filepath.Join(".github", "workflows", "release.yml")))
	build, publish := jobs["build"], jobs["publish"]
	if build == "" || publish == "" {
		t.Fatalf("release.yml has no build or publish job: %v", jobs)
	}
	perms := regexp.MustCompile(`(?m)^    permissions:\n((?:      .*\n)*)`)
	if m := perms.FindStringSubmatch(build); m == nil || strings.TrimSpace(m[1]) != "contents: read" {
		t.Errorf("the build job's permissions are not contents: read alone: %q", m)
	}
	for _, s := range []string{"go test ./...", "go build", "sha256sum * > SHA256SUMS", "merge-base --is-ancestor", "does not pin"} {
		if !strings.Contains(build, s) {
			t.Errorf("the build job does not run %q", s)
		}
	}
	for _, s := range []string{"actions/checkout@", "setup-go", "go ", "bash scripts/", "sh scripts/"} {
		if strings.Contains(publish, s) {
			t.Errorf("the publish job runs %q: it must check out and run nothing from the repository", s)
		}
	}
	if !strings.HasPrefix(jobs["ref"], "  ref:\n") || !strings.Contains(jobs["ref"], "permissions: {}") {
		t.Error("the guard job for a manual run is not first, or holds permissions")
	}
	if !strings.Contains(publish, "needs: [ref, macos, build]") {
		t.Error("the publish job does not wait for the guard, macOS and the build")
	}
	// The checks run in this order, before anything is attested or published.
	order := []string{
		"actions/download-artifact@",
		"- name: SHA256SUMS and the notes are the ones build produced",
		"- name: Every file matches SHA256SUMS",
		"- name: dist holds exactly the files SHA256SUMS lists",
		"actions/attest-build-provenance@",
		"softprops/action-gh-release@",
	}
	at := -1
	for _, s := range order {
		i := strings.Index(publish, s)
		if i <= at {
			t.Fatalf("publish: %q is missing or out of order", s)
		}
		at = i
	}
}

// Every checkout drops the token from its git configuration, and every
// action is named by a full commit SHA.
func TestWorkflowsPinActionsAndKeepNoCredential(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(repoRoot, ".github", "workflows", "*.yml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows: %v", err)
	}
	uses := regexp.MustCompile(`(?m)^\s*(?:- )?uses: (\S+)`)
	pinned := regexp.MustCompile(`^[\w.-]+/[\w./-]+@[0-9a-f]{40}$`)
	keeps := regexp.MustCompile(`(?m)^          persist-credentials: false(\s+#.*)?$`)
	for _, f := range files {
		wf := read(t, filepath.Join(".github", "workflows", filepath.Base(f)))
		for _, m := range uses.FindAllStringSubmatch(wf, -1) {
			if !pinned.MatchString(m[1]) {
				t.Errorf("%s: %s is not pinned to a full commit SHA", filepath.Base(f), m[1])
			}
		}
		for _, s := range strings.Split(wf, "\n      - ") {
			if strings.HasPrefix(s, "uses: actions/checkout@") && !keeps.MatchString(s) {
				t.Errorf("%s: a checkout keeps the token: %s", filepath.Base(f), strings.SplitN(s, "\n", 2)[0])
			}
		}
	}
}

// The publish job's three checks, run as release.yml has them against a
// handed-over artifact: a good one passes all three, and each tampering
// stops at the check that names it.
func TestPublishChecksRefuseAnArtifactBuildDidNotProduce(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the checks run on the ubuntu runner (GNU coreutils and find)")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	publish := workflowJobs(t, read(t, filepath.Join(".github", "workflows", "release.yml")))["publish"]
	digests := stepRun(t, publish, "SHA256SUMS and the notes are the ones build produced")
	sums := stepRun(t, publish, "Every file matches SHA256SUMS")
	exact := stepRun(t, publish, "dist holds exactly the files SHA256SUMS lists")

	run := func(dir, script string, env ...string) (string, error) {
		cmd := exec.Command(bash, "-e", "-c", script)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// build: the artifact, as the build job lays it out, and its outputs.
	build := func(t *testing.T) (root string, env []string) {
		root = t.TempDir()
		dist := filepath.Join(root, "dist")
		if err := os.Mkdir(dist, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"aimem-linux-amd64": "binary\n", "LICENSE": "license\n", "boot.sh": "#!/bin/sh\n"} {
			if err := os.WriteFile(filepath.Join(dist, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, "release_notes.md"), []byte("notes\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := run(root, `(cd dist && sha256sum * > SHA256SUMS)
echo "SUMS=$(sha256sum dist/SHA256SUMS | cut -d' ' -f1)"
echo "NOTES=$(sha256sum release_notes.md | cut -d' ' -f1)"`)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return root, strings.Fields(out)
	}
	type step struct{ dir, script string }
	checks := func(root string) []step {
		return []step{{root, digests}, {filepath.Join(root, "dist"), sums}, {filepath.Join(root, "dist"), exact}}
	}
	write := func(t *testing.T, p, body string) {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name   string
		tamper func(t *testing.T, root string)
		stop   int    // the check that refuses it; 3 = none
		want   string // its message
	}{
		{"as built", func(*testing.T, string) {}, 3, ""},
		{"notes changed", func(t *testing.T, r string) { write(t, filepath.Join(r, "release_notes.md"), "other\n") },
			0, "not the ones the build job produced"},
		{"sums rewritten over a changed file", func(t *testing.T, r string) {
			write(t, filepath.Join(r, "dist", "boot.sh"), "#!/bin/sh\nevil\n")
			if out, err := run(filepath.Join(r, "dist"), "sha256sum aimem-linux-amd64 LICENSE boot.sh > SHA256SUMS"); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		}, 0, "not the ones the build job produced"},
		{"a file changed", func(t *testing.T, r string) { write(t, filepath.Join(r, "dist", "LICENSE"), "changed\n") },
			1, "does not match its line in SHA256SUMS"},
		{"a file the sums do not list", func(t *testing.T, r string) { write(t, filepath.Join(r, "dist", "extra"), "x\n") },
			2, "present but not listed:\nextra"},
		{"a listed file missing", func(t *testing.T, r string) {
			if err := os.Remove(filepath.Join(r, "dist", "boot.sh")); err != nil {
				t.Fatal(err)
			}
		}, 1, "does not match its line in SHA256SUMS"},
		{"a directory the sums do not list", func(t *testing.T, r string) {
			if err := os.Mkdir(filepath.Join(r, "dist", "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, 2, "present but not listed:\nsub"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root, env := build(t)
			c.tamper(t, root)
			for i, s := range checks(root) {
				out, err := run(s.dir, s.script, env...)
				if i < c.stop {
					if err != nil {
						t.Fatalf("check %d refused it: %v\n%s", i, err, out)
					}
					continue
				}
				if err == nil || !strings.Contains(out, c.want) {
					t.Fatalf("check %d: want a refusal with %q, got %v\n%s", i, c.want, err, out)
				}
				return
			}
		})
	}
	// The exact-match check alone: a file the sums do not list passes the
	// other two, so this step is what keeps it out of the release.
	t.Run("only the exact-match check stops an unlisted file", func(t *testing.T) {
		root, env := build(t)
		write(t, filepath.Join(root, "dist", "extra"), "x\n")
		for _, s := range checks(root)[:2] {
			if out, err := run(s.dir, s.script, env...); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
		}
	})
}
