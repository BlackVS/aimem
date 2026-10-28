package main

// `aimem process select <repo> <tag>`: the tag is resolved to its commit on
// the hub host and both are stored; the manifest defaults; --expect names
// the current selection by its tag or its commit; re-selecting the commit
// already selected needs no --expect; a full commit works as before.

import (
	"errors"
	"strings"
	"testing"

	"aimem/internal/process"
)

func TestProcessSelectByTag(t *testing.T) {
	reg := projectService(t)
	if _, err := reg.Open("example"); err != nil {
		t.Fatal(err)
	}
	const repo = "https://example.com/team/aiskills.git"
	manifest := process.DefaultManifest
	c1, c2, c3 := strings.Repeat("a1", 20), strings.Repeat("b2", 20), strings.Repeat("c3", 20)
	tags := map[string]string{"v1": c1, "v2": c2}
	var resolved []string
	old := resolveRef
	resolveRef = func(r, name string) (string, error) {
		resolved = append(resolved, name)
		if r != repo {
			t.Fatalf("resolved in %s", r)
		}
		if c, ok := tags[name]; ok {
			return c, nil
		}
		return "", errors.New(`the repository has no tag or branch "` + name + `"`)
	}
	t.Cleanup(func() { resolveRef = old })

	archives := map[string]string{}
	for _, c := range []string{c1, c2, c3} {
		archives[c], _ = exportFixture(t, repo, c, manifest)
	}
	sel := func(args ...string) error {
		_, err := stdoutOf(t, func() error {
			return processCmd(append(append([]string{"select"}, args...), "-p", "example"))
		})
		return err
	}
	current := func() process.Ref {
		t.Helper()
		ref, err := currentSelection("example")
		if err != nil || ref == nil {
			t.Fatalf("current: %v %v", ref, err)
		}
		return *ref
	}

	// A tag, the default manifest: the tag and its commit are stored.
	if err := sel(repo, "v1", "--from-archive", archives[c1]); err != nil {
		t.Fatal(err)
	}
	if got := current(); got.Ref != "v1" || got.Commit != c1 || got.Manifest != manifest || got.Digest == "" {
		t.Fatalf("selected by tag: %+v", got)
	}
	// The same tag again, the same commit: no --expect needed.
	if err := sel(repo, "v1"); err != nil {
		t.Fatalf("re-select of the selected commit: %v", err)
	}
	// The same tag with another manifest is another process set: without
	// --expect it is refused, as a change of commit is.
	other := "processes/other/manifest.json"
	otherArchive, _ := exportFixture(t, repo, c1, other)
	if err := sel(repo, "v1", other, "--from-archive", otherArchive); err == nil {
		t.Fatal("another manifest at the selected commit was accepted without --expect")
	}
	if got := current(); got.Manifest != manifest {
		t.Fatalf("the manifest changed unseen: %+v", got)
	}
	// Another commit without --expect: the hub's compare-and-swap refuses.
	if err := sel(repo, "v2", "--from-archive", archives[c2]); err == nil {
		t.Fatal("a change without --expect was accepted")
	}
	// --expect by the selected tag.
	if err := sel(repo, "v2", "--from-archive", archives[c2], "--expect", "v1"); err != nil {
		t.Fatal(err)
	}
	if got := current(); got.Ref != "v2" || got.Commit != c2 {
		t.Fatalf("after v2: %+v", got)
	}
	// --expect naming neither the selected tag nor its commit is refused
	// before anything reaches the hub.
	if err := sel(repo, "v1", "--expect", "v1"); err == nil || !strings.Contains(err.Error(), "neither the selected commit nor the selected ref") {
		t.Fatalf("a stale --expect tag: %v", err)
	}
	// --expect by the selected commit, and a tag back to a kept set.
	if err := sel(repo, "v1", "--expect", c2); err != nil {
		t.Fatal(err)
	}
	if got := current(); got.Ref != "v1" || got.Commit != c1 {
		t.Fatalf("back to v1: %+v", got)
	}
	// A full commit and an explicit manifest work as before, with --ref
	// as the fetch hint; --ref beside a tag is refused.
	if err := sel(repo, c3, manifest, "--commit", "--ref", "main", "--from-archive", archives[c3], "--expect", "v1"); err != nil {
		t.Fatal(err)
	}
	if got := current(); got.Ref != "main" || got.Commit != c3 {
		t.Fatalf("by commit: %+v", got)
	}
	if err := sel(repo, "v2", "--ref", "main", "--expect", c3); err == nil || !strings.Contains(err.Error(), "--ref goes with a commit") {
		t.Fatalf("--ref with a tag: %v", err)
	}
	// An unknown tag stores nothing.
	if err := sel(repo, "v9", "--expect", c3); err == nil || !strings.Contains(err.Error(), `no tag or branch "v9"`) {
		t.Fatalf("unknown tag: %v", err)
	}
	if got := current(); got.Commit != c3 {
		t.Fatalf("after an unknown tag: %+v", got)
	}
	// An abbreviated or uppercase commit is not a tag: the error says what
	// a commit must be.
	for _, short := range []string{c1[:12], strings.ToUpper(c1)} {
		if err := sel(repo, short, "--expect", c3); err == nil || !strings.Contains(err.Error(), "full 40-character lowercase hex id") {
			t.Fatalf("%s: %v", short, err)
		}
	}
	// A full commit is never sent to the resolver.
	for _, name := range resolved {
		if process.IsCommit(name) {
			t.Fatalf("resolved a commit: %v", resolved)
		}
	}
	// clear takes the tag too.
	if err := sel(repo, "v1", "--expect", c3); err != nil {
		t.Fatal(err)
	}
	if _, err := stdoutOf(t, func() error { return processCmd([]string{"clear", "--expect", "v1", "-p", "example"}) }); err != nil {
		t.Fatalf("clear by tag: %v", err)
	}
	if ref, err := currentSelection("example"); err != nil || ref != nil {
		t.Fatalf("after clear: %v %v", ref, err)
	}
}
