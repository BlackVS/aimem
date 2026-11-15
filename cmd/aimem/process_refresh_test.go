package main

// `aimem process refresh` stores the set of a selection made before the
// hub stored sets, from its recorded values, leaves a stored one alone,
// and reports a set it cannot read as needing an archive without failing
// the run; `aimem process list` shows which selections lack a set.

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/process"
)

// copyWithSums copies archive to dst and writes the SHA256SUMS beside dst
// that names it, returning that file.
func copyWithSums(t *testing.T, archive, dst string) string {
	t.Helper()
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	return writeWithSums(t, dst, data)
}

func writeWithSums(t *testing.T, dst string, data []byte) string {
	t.Helper()
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
	sums := filepath.Join(filepath.Dir(dst), "SHA256SUMS")
	if err := os.WriteFile(sums, []byte(process.SumsLine(data, filepath.Base(dst))), 0o600); err != nil {
		t.Fatal(err)
	}
	return sums
}

// writeArchiveWithoutCommit writes a valid set for manifest at dst, with
// its SHA256SUMS beside it, as an archive that records no commit (what
// aimem process export wrote up to 0.10.16).
func writeArchiveWithoutCommit(t *testing.T, dst, manifest string) {
	t.Helper()
	dir := path.Dir(manifest)
	b := &process.Bundle{Version: 1, Manifest: manifest, Files: map[string][]byte{
		manifest:             []byte(`{"version":1,"handbook":"` + dir + `/HANDBOOK.md","templates":{"task":"` + dir + `/task.json"}}`),
		dir + "/HANDBOOK.md": []byte("# Handbook\n\nExported rules.\n"),
		dir + "/task.json":   []byte(`{"title":""}`),
	}}
	var buf bytes.Buffer
	if err := process.WriteArchive(&buf, b); err != nil {
		t.Fatal(err)
	}
	writeWithSums(t, dst, buf.Bytes())
}

func TestProcessRefreshStoresTheSetsOfOldSelections(t *testing.T) {
	reg := projectService(t)
	// Nothing listens on this address: Git can never read these sets, so
	// the test needs no network and shows the needs-an-archive path.
	const repo = "https://127.0.0.1:1/team/process.git"
	const manifest = "process/manifest.json"
	c1, c2, c3 := strings.Repeat("ab", 20), strings.Repeat("cd", 20), strings.Repeat("ef", 20)

	// The pre-0.10.7 shape: a selection with its repository, commit,
	// manifest and ref, and no set.
	legacy := func(project, commit string) {
		db, err := reg.Open(project)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(process.Ref{Repo: repo, Ref: "main", Commit: commit, Manifest: manifest, SelectedAt: "2026-10-01T00:00:00Z"})
		if err := db.SetMeta("process", string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	legacy("legacy", c1)
	legacy("nogit", c3)
	if _, err := reg.Open("none"); err != nil {
		t.Fatal(err)
	}
	// A selection made with its set.
	if _, err := reg.Open("current"); err != nil {
		t.Fatal(err)
	}
	archive2, _ := exportFixture(t, repo, c2, manifest)
	if _, err := stdoutOf(t, func() error {
		return processCmd([]string{"select", repo, c2, manifest, "--commit", "--from-archive", archive2, "-p", "current"})
	}); err != nil {
		t.Fatal(err)
	}
	selection := func(project string) process.Ref {
		t.Helper()
		ref, err := currentSelection(project)
		if err != nil || ref == nil {
			t.Fatalf("%s: %v %v", project, ref, err)
		}
		return *ref
	}
	before := selection("current")
	run := func(args ...string) string {
		t.Helper()
		out, err := stdoutOf(t, func() error { return processCmd(args) })
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	lineOf := func(out, project string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, project+" ") || strings.HasPrefix(l, project+":") {
				return l
			}
		}
		t.Fatalf("no line for %s in:\n%s", project, out)
		return ""
	}

	out := run("list")
	if l := lineOf(out, "legacy"); !strings.Contains(l, "not stored") || !strings.Contains(l, c1) || !strings.Contains(l, "main") {
		t.Fatalf("list, legacy: %q", l)
	}
	if l := lineOf(out, "current"); strings.Contains(l, "not stored") || !strings.Contains(l, "stored") {
		t.Fatalf("list, current: %q", l)
	}
	if strings.Contains(out, "\nnone ") || !strings.Contains(out, "2 selection(s) without a stored set") {
		t.Fatalf("list:\n%s", out)
	}

	// Git cannot read the sets: each is reported with the commands that
	// supply it, the stored one is left alone, and the run succeeds.
	out = run("refresh")
	for _, p := range []string{"legacy", "nogit"} {
		if l := lineOf(out, p); !strings.Contains(l, "needs an archive") || !strings.Contains(l, "aimem process export "+repo) || !strings.Contains(l, "--ref main") || !strings.Contains(l, "aimem process refresh -p "+p+" --from-archive") {
			t.Fatalf("refresh, %s: %q", p, l)
		}
	}
	if l := lineOf(out, "current"); !strings.Contains(l, "already stored") {
		t.Fatalf("refresh, current: %q", l)
	}
	if !strings.Contains(out, "0 refreshed, 1 already stored, 2 need attention") {
		t.Fatalf("refresh summary:\n%s", out)
	}
	if after := selection("current"); after != before {
		t.Fatalf("a stored selection changed:\n%+v\n%+v", before, after)
	}

	// A selection the hub cannot read is that project's outcome: the run
	// goes on to the others and succeeds. Named alone, it is the error.
	broken, err := reg.Open("broken")
	if err != nil {
		t.Fatal(err)
	}
	if err := broken.SetMeta("process", "{not json"); err != nil {
		t.Fatal(err)
	}
	out = run("refresh")
	if l := lineOf(out, "broken"); !strings.Contains(l, "selection unreadable, nothing stored") {
		t.Fatalf("refresh, broken: %q", l)
	}
	for _, p := range []string{"legacy", "nogit"} {
		if l := lineOf(out, p); !strings.Contains(l, "needs an archive") {
			t.Fatalf("refresh with a broken project, %s: %q", p, l)
		}
	}
	if !strings.Contains(out, "0 refreshed, 1 already stored, 3 need attention") {
		t.Fatalf("refresh summary with a broken project:\n%s", out)
	}
	out = run("list")
	if l := lineOf(out, "broken"); !strings.Contains(l, "unreadable") {
		t.Fatalf("list, broken: %q", l)
	}
	if !strings.Contains(out, "2 selection(s) without a stored set") || !strings.Contains(out, "1 selection(s) the hub cannot read") {
		t.Fatalf("list with a broken project:\n%s", out)
	}
	if _, err := stdoutOf(t, func() error { return processCmd([]string{"refresh", "-p", "broken"}) }); err == nil || !strings.Contains(err.Error(), "project broken") {
		t.Fatalf("refresh -p of an unreadable selection: %v", err)
	}

	// An archive is one selection's set: it needs -p, and the commit it
	// records must be the selection's, whatever the file is called.
	if _, err := stdoutOf(t, func() error { return processCmd([]string{"refresh", "--from-archive", archive2}) }); err == nil || !strings.Contains(err.Error(), "-p") {
		t.Fatalf("--from-archive without -p: %v", err)
	}
	refused := func(name, archive, want string) {
		t.Helper()
		out := run("refresh", "-p", "legacy", "--from-archive", archive)
		if l := lineOf(out, "legacy"); !strings.Contains(l, want) || !strings.Contains(l, "nothing stored") {
			t.Fatalf("%s: %q", name, l)
		}
		if selection("legacy").Digest != "" {
			t.Fatalf("%s was stored", name)
		}
	}
	refused("an archive of another commit", archive2, "records commit "+c2+", not the selected "+c1)
	// Named for the selection's commit, recording another: the name decides nothing.
	misnamed := filepath.Join(t.TempDir(), "process-"+c1[:12]+".tar.gz")
	copyWithSums(t, archive2, misnamed)
	refused("an archive named for the commit but recording another", misnamed, "records commit "+c2+", not the selected "+c1)
	// An archive that records no commit cannot be checked against the selection.
	plain := filepath.Join(t.TempDir(), "process-"+c1[:12]+".tar.gz")
	writeArchiveWithoutCommit(t, plain, manifest)
	refused("an archive that records no commit", plain, "records no commit")

	// The release's fixed name refreshes the selection whose commit it records.
	archive1, _ := exportFixture(t, repo, c1, manifest)
	fixed := filepath.Join(t.TempDir(), "aimem-process.tar.gz")
	sums1 := copyWithSums(t, archive1, fixed)
	archive1 = fixed
	out = run("refresh", "-p", "legacy", "--from-archive", archive1)
	if l := lineOf(out, "legacy"); !strings.Contains(l, "refreshed") {
		t.Fatalf("refresh from the archive: %q", l)
	}
	got := selection("legacy")
	b, err := process.ReadArchiveFile(archive1, sums1, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != b.Digest() || got.Commit != c1 || got.Ref != "main" || got.Repo != repo || got.Manifest != manifest {
		t.Fatalf("refreshed selection: %+v", got)
	}

	// A second run leaves it as it is.
	out = run("refresh", "-p", "legacy")
	if l := lineOf(out, "legacy"); !strings.Contains(l, "already stored (set "+b.Digest()[:12]) {
		t.Fatalf("second refresh: %q", l)
	}
	if again := selection("legacy"); again != got {
		t.Fatalf("a second refresh changed the selection:\n%+v\n%+v", got, again)
	}
	if _, err := stdoutOf(t, func() error { return processCmd([]string{"refresh", "-p", "none"}) }); err == nil || !strings.Contains(err.Error(), "no process selected") {
		t.Fatalf("refresh of a project without a selection: %v", err)
	}
}

// The fallback command refresh prints is pasted into a shell: a selection
// may hold characters a shell interprets (a manifest path accepts
// ' " $ ; & |), and a POSIX shell must read every argument back unchanged.
func TestProcessRefreshQuotesTheFallbackCommand(t *testing.T) {
	reg := projectService(t)
	sel := process.Ref{Repo: "https://127.0.0.1:1/team$1;x.git", Ref: "main", Commit: strings.Repeat("ab", 20), Manifest: `process/team's"$HOME;&|~x.json`, SelectedAt: "2026-10-01T00:00:00Z"}
	if err := sel.Validate(); err != nil {
		t.Fatalf("the test selection must be one the hub accepts: %v", err)
	}
	db, err := reg.Open("quoted")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(sel)
	if err := db.SetMeta("process", string(raw)); err != nil {
		t.Fatal(err)
	}
	out, err := stdoutOf(t, func() error { return processCmd([]string{"refresh", "-p", "quoted"}) })
	if err != nil {
		t.Fatalf("refresh: %v\n%s", err, out)
	}
	const before, after = "On a machine with access: aimem process export ", "; then here: aimem process refresh -p quoted --from-archive <file>"
	i, j := strings.Index(out, before), strings.LastIndex(out, after)
	if i < 0 || j < i {
		t.Fatalf("no fallback command in:\n%s", out)
	}
	args := out[i+len(before) : j]
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this machine to read the command back")
	}
	got, err := exec.Command(sh, "-c", `printf '%s\n' `+args).Output()
	if err != nil {
		t.Fatalf("sh could not read %q: %v", args, err)
	}
	want := strings.Join([]string{sel.Repo, sel.Commit, sel.Manifest, "--ref", "main"}, "\n") + "\n"
	if string(got) != want {
		t.Fatalf("sh read back\n%q\nwant\n%q", got, want)
	}
}
