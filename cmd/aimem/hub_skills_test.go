package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"aimem/internal/process"
)

// stderrOf runs fn and returns what it wrote to standard error.
func stderrOf(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	err = fn()
	os.Stderr = old
	w.Close()
	return <-done, err
}

// A process whose required skills the hub lacks is reported where the
// admin selects and inspects it, with the command that imports each one,
// until the admin imports them (observed 2026-10-26: a hub with no skills
// set at all reported a ready project).
func TestHubWithoutRequiredSkillsSaysHowToImportThem(t *testing.T) {
	reg := projectService(t)
	if _, err := reg.Open("example"); err != nil {
		t.Fatal(err)
	}
	run := func(fn func() error) string {
		t.Helper()
		out, err := stdoutOf(t, fn)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
	}
	run(func() error { return tasksCmd([]string{"on", "-p", "example"}) })

	// The process requires a release skill and a skill of the instance's
	// own, unpinned (a version 1 manifest).
	commit := strings.Repeat("cd", 20)
	b := &process.Bundle{Version: 1, Manifest: "process/manifest.json", Files: map[string][]byte{
		"process/manifest.json": []byte(`{"version":1,"handbook":"process/HANDBOOK.md","skills":["oh-code-review","house-style"]}`),
		"process/HANDBOOK.md":   []byte("# Handbook\n"),
	}}
	archive, _, err := writeExport(b, process.Ref{Repo: "https://example.com/process.git", Commit: commit, Manifest: "process/manifest.json"}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const (
		manual   = " (docs/ADMIN-MANUAL.md#importing-the-skills-sets)"
		release  = "oh-code-review: no skill of that name in the hub's sets; the admin imports the skills release on the hub host: `aimem skills import --from-archive aiskills-<version>.tar.gz`" + manual
		instance = "house-style: no skill of that name in the hub's sets; it is not in the skills release, so it must come from the instance set: the admin imports the instance set on the hub host: `aimem skills import --from-archive <file> --source instance --version <version>`" + manual
	)
	var stdout string
	stderr, err := stderrOf(t, func() error {
		var err error
		stdout, err = stdoutOf(t, func() error {
			return processCmd([]string{"select", "https://example.com/process.git", commit, "process/manifest.json", "--commit", "--from-archive", archive, "-p", "example"})
		})
		return err
	})
	if err != nil {
		t.Fatalf("select: %v\n%s\n%s", err, stdout, stderr)
	}
	if !strings.Contains(stderr, "warning: "+hubLacksSkills+":\n  "+release+"\n  "+instance+"\n") {
		t.Fatalf("select names no missing skill and its fix:\n%s", stderr)
	}
	show := func() string {
		return run(func() error { return projectNamespaceCmd([]string{"show", "-p", "example"}) })
	}
	list := func() string { return run(func() error { return projectNamespaceCmd([]string{"list"}) }) }
	if out := show(); !strings.Contains(out, "warning:    "+hubLacksSkills+":\n  "+release+"\n  "+instance+"\n") {
		t.Fatalf("show:\n%s", out)
	}
	if out := list(); !strings.Contains(out, "example  not ready for work: "+hubLacksSkills+"\n  "+release+"\n  "+instance+"\n") {
		t.Fatalf("list:\n%s", out)
	}

	// The admin imports both: nothing is missing any more.
	put := func(source, version, name string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"version": version, "files": map[string][]byte{name + "/SKILL.md": []byte("---\nname: " + name + "\n---\n")}})
		var answer map[string]any
		if err := localJSON(http.MethodPut, "/v1/skills/"+source, body, &answer); err != nil {
			t.Fatal(err)
		}
	}
	put("release", "1.34.0", "oh-code-review")
	if out := show(); !strings.Contains(out, hubLacksSkills+":\n  "+instance+"\n") || strings.Contains(out, "oh-code-review:") {
		t.Fatalf("show, the release imported:\n%s", out)
	}
	put("instance", "site-1", "house-style")
	if out := show(); strings.Contains(out, "warning") {
		t.Fatalf("show, both imported:\n%s", out)
	}
	if out := list(); !strings.Contains(out, "example  tasks on, process selected\n") {
		t.Fatalf("list, both imported:\n%s", out)
	}
	stderr, err = stderrOf(t, func() error {
		_, err := stdoutOf(t, func() error {
			return processCmd([]string{"select", "https://example.com/process.git", commit, "process/manifest.json", "--commit", "--from-archive", archive, "-p", "example", "--expect", commit})
		})
		return err
	})
	if err != nil || strings.Contains(stderr, "warning") {
		t.Fatalf("select, both imported: %v\n%s", err, stderr)
	}
}
