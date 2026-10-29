package main

// The hub's default process: `process default set|show|clear` on the hub
// host, `tasks on` applying it to a project with no process, and `process
// select` taking a bare commit only with --commit.

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/process"
)

// defaultRig serves a repository with tags v1 and v2 to the resolver and
// the Git read, with no network.
func defaultRig(t *testing.T) (repo string, tags map[string]string) {
	t.Helper()
	repo = "https://example.com/team/process.git"
	c1, c2 := strings.Repeat("d1", 20), strings.Repeat("d2", 20)
	tags = map[string]string{"v1": c1, "v2": c2}
	archives, sums := map[string]string{}, map[string]string{}
	for _, c := range []string{c1, c2} {
		archives[c], sums[c] = exportFixture(t, repo, c, process.DefaultManifest)
	}
	oldResolve, oldGit, oldPresent := resolveRef, fetchGit, gitPresent
	t.Cleanup(func() { resolveRef, fetchGit, gitPresent = oldResolve, oldGit, oldPresent })
	resolveRef = func(r, name string) (string, error) {
		if c, ok := tags[name]; ok && r == repo {
			return c, nil
		}
		return "", errors.New(`the repository has no tag or branch "` + name + `"`)
	}
	gitPresent = func() bool { return true }
	fetchGit = func(sel process.Ref) (*process.Bundle, error) {
		a, ok := archives[sel.Commit]
		if !ok || sel.Repo != repo {
			return nil, errors.New("no such commit")
		}
		return process.ReadArchiveFile(a, sums[sel.Commit], sel.Manifest)
	}
	return repo, tags
}

func TestProcessDefaultSetShowClear(t *testing.T) {
	projectService(t)
	repo, tags := defaultRig(t)
	run := func(args ...string) (string, error) {
		return stdoutOf(t, func() error { return processCmd(append([]string{"default"}, args...)) })
	}
	if out, err := run("show"); err != nil || !strings.Contains(out, "no default process is set") {
		t.Fatalf("show with none: %v\n%s", err, out)
	}
	// A tag the repository lacks is refused as select refuses it, and so is
	// a commit; nothing is stored.
	if _, err := run("set", repo, "v9"); err == nil || !strings.Contains(err.Error(), `no tag or branch "v9"`) {
		t.Fatalf("missing tag: %v", err)
	}
	if _, err := run("set", repo, tags["v1"]); err == nil || !strings.Contains(err.Error(), "a default names a tag") {
		t.Fatalf("a commit: %v", err)
	}
	if def, err := hubProcessDefault(); err != nil || def != nil {
		t.Fatalf("a refused set stored %+v %v", def, err)
	}

	out, err := run("set", repo, "v1")
	if err != nil {
		t.Fatalf("set: %v\n%s", err, out)
	}
	for _, want := range []string{"  repository  " + repo + "\n", "  tag         v1\n", "  commit      " + tags["v1"] + "\n", "  manifest    " + process.DefaultManifest + "\n", "  set         ", "  files       kept on the hub (set "} {
		if !strings.Contains(out, want) {
			t.Fatalf("set prints no %q:\n%s", want, out)
		}
	}
	if show, err := run("show"); err != nil || show != out {
		t.Fatalf("show differs from set: %v\n%s\nset:\n%s", err, show, out)
	}
	def, err := hubProcessDefault()
	if err != nil || def == nil || def.Ref != "v1" || def.Commit != tags["v1"] || len(def.Digest) != 64 {
		t.Fatalf("stored %+v %v", def, err)
	}
	// Setting again replaces it.
	if _, err := run("set", repo, "v2"); err != nil {
		t.Fatal(err)
	}
	if def, _ := hubProcessDefault(); def == nil || def.Ref != "v2" || def.Commit != tags["v2"] {
		t.Fatalf("after the second set: %+v", def)
	}
	if out, err := run("clear"); err != nil || !strings.Contains(out, "default process cleared") {
		t.Fatalf("clear: %v\n%s", err, out)
	}
	if _, err := run("clear"); err == nil || !strings.Contains(err.Error(), "no default process is set") {
		t.Fatalf("second clear: %v", err)
	}
	for _, args := range [][]string{{}, {"set", repo}, {"show", "x"}, {"clear", "x"}, {"nope"}} {
		if _, err := run(args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

// tasks on applies the default to a project with no process, records it
// like any selection and says so; a project with a selection keeps it;
// --no-process and no default leave today's output.
func TestTasksOnAppliesTheHubDefault(t *testing.T) {
	reg := projectService(t)
	for _, p := range []string{"plain", "fresh", "kept", "skipped"} {
		if _, err := reg.Open(p); err != nil {
			t.Fatal(err)
		}
	}
	repo, tags := defaultRig(t)
	tasks := func(args ...string) (string, error) {
		return stdoutOf(t, func() error { return tasksCmd(args) })
	}

	// No default: the output is today's, the gap and the next step.
	out, err := tasks("on", "-p", "plain")
	if err != nil {
		t.Fatal(err)
	}
	if want := "tasks on for project \"plain\"\n" + notReady + process.NextStep("plain", nil); out != want {
		t.Fatalf("no default:\n%s\nwant:\n%s", out, want)
	}

	if _, err := stdoutOf(t, func() error { return processCmd([]string{"default", "set", repo, "v1"}) }); err != nil {
		t.Fatal(err)
	}
	// The tag moves: the default still names the commit it was set with.
	tags["v1"] = tags["v2"]
	def, _ := hubProcessDefault()

	out, err = tasks("on", "-p", "fresh")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "applied the hub's default process: "+def.Name+" (tag v1, commit "+def.Commit+")\n") || strings.Contains(out, "not ready") {
		t.Fatalf("default applied:\n%s", out)
	}
	sel, err := currentSelection("fresh")
	if err != nil || sel == nil || sel.Repo != repo || sel.Ref != "v1" || sel.Commit != def.Commit || sel.Manifest != def.Manifest || sel.Digest != def.Digest {
		t.Fatalf("recorded %+v %v, default %+v", sel, err, def)
	}

	// A project with its own selection keeps it.
	if _, err := stdoutOf(t, func() error { return processCmd([]string{"select", repo, "v2", "-p", "kept"}) }); err != nil {
		t.Fatal(err)
	}
	before, _ := currentSelection("kept")
	if out, err := tasks("on", "-p", "kept"); err != nil || strings.Contains(out, "applied") || strings.Contains(out, "not ready") {
		t.Fatalf("kept: %v\n%s", err, out)
	}
	if after, _ := currentSelection("kept"); after == nil || *after != *before {
		t.Fatalf("the selection changed: %+v -> %+v", before, after)
	}

	// --no-process selects nothing and prints today's gap.
	out, err = tasks("on", "-p", "skipped", "--no-process")
	if err != nil || strings.Contains(out, "applied") || !strings.Contains(out, notReady) {
		t.Fatalf("--no-process: %v\n%s", err, out)
	}
	if sel, _ := currentSelection("skipped"); sel != nil {
		t.Fatalf("--no-process selected %+v", sel)
	}
	if _, err := tasks("off", "-p", "skipped", "--no-process"); err == nil {
		t.Fatal("tasks off took --no-process")
	}

	// The hub keeps the default's set, so it is applied with neither Git
	// nor the forge reachable.
	noNetwork(t)
	out, err = tasks("on", "-p", "plain")
	if err != nil || !strings.Contains(out, "applied the hub's default process: "+def.Name+" (tag v1, commit "+def.Commit+")\n") {
		t.Fatalf("offline apply: %v\n%s", err, out)
	}
	if sel, _ := currentSelection("plain"); sel == nil || sel.Commit != def.Commit || sel.Digest != def.Digest {
		t.Fatalf("offline apply recorded %+v", sel)
	}
}

// noNetwork makes every read of a set or a tag fail, as on a hub host
// with no network: no tag resolves, Git reads nothing and the forge is
// unreachable.
func noNetwork(t *testing.T) {
	t.Helper()
	oldResolve, oldGit, oldForge := resolveRef, fetchGit, forgeClient
	t.Cleanup(func() { resolveRef, fetchGit, forgeClient = oldResolve, oldGit, oldForge })
	resolveRef = func(string, string) (string, error) { return "", errors.New("no network in this test") }
	fetchGit = func(process.Ref) (*process.Bundle, error) { return nil, errors.New("no network in this test") }
	forgeClient = &http.Client{Transport: failingTransport{}}
}

// A default stored by an older command (the digest, no files) is applied
// from the repository as before, and not applied when the repository
// cannot be read: tasks stay on and the gap is printed with the error.
func TestTasksOnAppliesADigestOnlyDefaultFromGit(t *testing.T) {
	reg := projectService(t)
	for _, p := range []string{"one", "two"} {
		if _, err := reg.Open(p); err != nil {
			t.Fatal(err)
		}
	}
	repo, tags := defaultRig(t)
	sel := process.Ref{Repo: repo, Ref: "v1", Commit: tags["v1"], Manifest: process.DefaultManifest}
	b, err := fetchGit(sel)
	if err != nil {
		t.Fatal(err)
	}
	sel.Digest = b.Digest()
	raw, _ := json.Marshal(sel)
	var stored map[string]any
	if err := localJSON(http.MethodPut, "/v1/process-default", raw, &stored); err != nil {
		t.Fatal(err)
	}
	if show, err := stdoutOf(t, func() error { return processCmd([]string{"default", "show"}) }); err != nil || !strings.Contains(show, "  files       not kept on the hub") {
		t.Fatalf("show: %v\n%s", err, show)
	}
	reads := 0
	git := fetchGit
	fetchGit = func(s process.Ref) (*process.Bundle, error) { reads++; return git(s) }
	tasks := func(args ...string) (string, error) {
		return stdoutOf(t, func() error { return tasksCmd(args) })
	}
	if out, err := tasks("on", "-p", "one"); err != nil || !strings.Contains(out, "applied the hub's default process") || reads != 1 {
		t.Fatalf("digest-only apply (%d Git reads): %v\n%s", reads, err, out)
	}
	if got, _ := currentSelection("one"); got == nil || got.Commit != sel.Commit || got.Digest != sel.Digest {
		t.Fatalf("recorded %+v", got)
	}

	noNetwork(t)
	out, err := tasks("on", "-p", "two")
	if err == nil || !strings.Contains(err.Error(), "tasks are on, but the hub's default process was not applied") || !strings.Contains(out, notReady) {
		t.Fatalf("failed apply: %v\n%s", err, out)
	}
	if got, _ := currentSelection("two"); got != nil {
		t.Fatalf("a failed apply selected %+v", got)
	}
}

// default set --from-archive reads nothing over the network: the set is
// the verified archive's and the commit the one it records. An archive
// its sums do not vouch for, or one that records no commit, is refused;
// clear removes the kept set.
func TestProcessDefaultSetFromArchiveOffline(t *testing.T) {
	reg := projectService(t)
	if _, err := reg.Open("fresh"); err != nil {
		t.Fatal(err)
	}
	const repo = "https://example.com/team/process.git"
	commit := strings.Repeat("e1", 20)
	archive, sums := exportFixture(t, repo, commit, process.DefaultManifest)
	noNetwork(t)
	run := func(args ...string) (string, error) {
		return stdoutOf(t, func() error { return processCmd(append([]string{"default"}, args...)) })
	}

	bad := filepath.Join(t.TempDir(), "SHA256SUMS")
	if err := os.WriteFile(bad, []byte(strings.Repeat("0", 64)+"  "+filepath.Base(archive)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run("set", repo, "v1", "--from-archive", archive, "--sums", bad); err == nil || !strings.Contains(err.Error(), "archive") {
		t.Fatalf("a mismatched sums file: %v", err)
	}
	// An archive that records no commit says how to get one that does.
	b, err := process.ReadArchiveFile(archive, sums, process.DefaultManifest)
	if err != nil {
		t.Fatal(err)
	}
	plain, plainSums, err := writeExport(b, process.Ref{Repo: repo, Commit: strings.Repeat("e2", 20), Manifest: process.DefaultManifest}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	process.WriteArchive(&buf, b)
	if err := os.WriteFile(plain, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plainSums, []byte(process.SumsLine([]byte(buf.String()), filepath.Base(plain))), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run("set", repo, "v1", "--from-archive", plain); err == nil || !strings.Contains(err.Error(), "records no commit") {
		t.Fatalf("an archive without its commit: %v", err)
	}
	if def, err := hubProcessDefault(); err != nil || def != nil {
		t.Fatalf("a refused set stored %+v %v", def, err)
	}

	out, err := run("set", repo, "v1", "--from-archive", archive, "--sums", sums)
	if err != nil {
		t.Fatalf("offline set: %v\n%s", err, out)
	}
	if !strings.Contains(out, "  commit      "+commit+"\n") || !strings.Contains(out, "  files       kept on the hub (set ") {
		t.Fatalf("offline set prints:\n%s", out)
	}
	def, err := hubProcessDefault()
	if err != nil || def == nil || def.Ref != "v1" || def.Commit != commit || def.Digest != b.Digest() {
		t.Fatalf("stored %+v %v", def, err)
	}
	if out, err := stdoutOf(t, func() error { return tasksCmd([]string{"on", "-p", "fresh"}) }); err != nil || !strings.Contains(out, "applied the hub's default process") {
		t.Fatalf("offline apply: %v\n%s", err, out)
	}

	kept := filepath.Join(reg.Root(), "process-default-set.json")
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("the kept set: %v", err)
	}
	if _, err := run("clear"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Fatalf("clear left the kept set: %v", err)
	}
}

// failingTransport answers every request with an error, so a test never
// reaches a forge.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no network in this test")
}

// A bare commit is selected only with --commit; the message names it.
func TestProcessSelectBareCommitNeedsTheFlag(t *testing.T) {
	reg := projectService(t)
	if _, err := reg.Open("example"); err != nil {
		t.Fatal(err)
	}
	repo, tags := defaultRig(t)
	c := tags["v1"]
	sel := func(args ...string) error {
		_, err := stdoutOf(t, func() error { return processCmd(append(append([]string{"select"}, args...), "-p", "example")) })
		return err
	}
	if err := sel(repo, c); err == nil || !strings.Contains(err.Error(), "is a commit; select by a tag") || !strings.Contains(err.Error(), "--commit") {
		t.Fatalf("bare commit: %v", err)
	}
	if cur, _ := currentSelection("example"); cur != nil {
		t.Fatalf("a refused select stored %+v", cur)
	}
	if err := sel(repo, "v1", "--commit"); err == nil || !strings.Contains(err.Error(), "--commit takes a full") {
		t.Fatalf("--commit with a tag: %v", err)
	}
	if err := sel(repo, c, "--commit"); err != nil {
		t.Fatalf("--commit: %v", err)
	}
	if cur, _ := currentSelection("example"); cur == nil || cur.Commit != c || cur.Ref != "" {
		t.Fatalf("selected %+v", cur)
	}
}
