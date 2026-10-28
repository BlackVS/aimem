package main

// `aimem process show` in a directory with no hub binding, on a machine
// with no hub configured (the hub host's admin shell, where select runs),
// prints the unit from this host's stored selection and set; whatever it
// cannot show is a named error, never an empty success.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/process"
)

func TestProcessShowReadsThisHostsStoreWithoutABinding(t *testing.T) {
	reg := projectService(t)
	t.Chdir(t.TempDir()) // no .aimem.json: no hub binding
	for _, p := range []string{"example", "none"} {
		if _, err := reg.Open(p); err != nil {
			t.Fatal(err)
		}
	}
	const repo = "https://example.com/team/process.git"
	const manifest = "process/manifest.json"
	commit := strings.Repeat("ab", 20)
	archive, sums := exportFixture(t, repo, commit, manifest)
	if _, err := stdoutOf(t, func() error {
		return processCmd([]string{"select", repo, commit, manifest, "--commit", "--from-archive", archive, "-p", "example"})
	}); err != nil {
		t.Fatal(err)
	}
	show := func(args ...string) (string, error) {
		t.Helper()
		return stdoutOf(t, func() error { return processCmd(append([]string{"show"}, args...)) })
	}

	// The unit is the one a member receives for that selection.
	b, err := process.ReadArchiveFile(archive, sums, manifest)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := currentSelection("example")
	if err != nil || ref == nil {
		t.Fatalf("selection: %v %v", ref, err)
	}
	set, err := b.Set(*ref)
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want, err := process.Bootstrap(set, "example", process.SkillStateOn(".", home))
	if err != nil {
		t.Fatal(err)
	}
	out, err := show("-p", "example")
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != strings.TrimSpace(want) || !strings.Contains(out, "Exported rules.") {
		t.Fatalf("show printed:\n%s\nwant:\n%s", out, want)
	}
	out, err = show("--full", "-p", "example")
	if err != nil || !strings.Contains(out, "Exported rules.") || !strings.Contains(out, "Repository (admin view, not part of the agent's context): "+repo) {
		t.Fatalf("show --full: %v\n%s", err, out)
	}
	out, err = show("--template", "task", "-p", "example")
	if err != nil || strings.TrimSpace(out) != `{"title":""}` {
		t.Fatalf("show --template: %v\n%q", err, out)
	}

	// Nothing to show is a named error, never an empty success.
	for _, c := range []struct{ project, want string }{
		{"none", "no process reference selected"},
		{"nope", "unknown project"},
	} {
		out, err := show("-p", c.project)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "no hub binding") || strings.TrimSpace(out) != "" {
			t.Fatalf("show -p %s: %v\n%q", c.project, err, out)
		}
	}

	// A checkout bound to a hub is never read from this host's store: it
	// gets what a session there gets, and here that hub is not configured.
	bound := t.TempDir()
	if err := os.WriteFile(filepath.Join(bound, ".aimem.json"), []byte(`{"hub":"other"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(bound)
	out, err = show("-p", "example")
	if err == nil || !strings.Contains(err.Error(), "no hub is configured for this checkout") || strings.TrimSpace(out) != "" {
		t.Fatalf("show in a bound checkout: %v\n%q", err, out)
	}
}
