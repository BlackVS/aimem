package main

// `aimem process select` on the hub host sends the set with the selection
// (from Git, or from an archive verified by its SHA256SUMS), re-selects a
// kept set without an import, and refuses to select what it cannot read;
// `aimem process export` writes the archive pair that select imports.

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/process"
)

// exportFixture writes the archive and SHA256SUMS for a small set through
// the export's own writer and returns both paths.
func exportFixture(t *testing.T, repo, commit, manifest string) (archive, sums string) {
	t.Helper()
	dir := path.Dir(manifest) // a repository path, slash-separated on every platform
	b := &process.Bundle{Version: 1, Manifest: manifest, Files: map[string][]byte{
		manifest:             []byte(`{"version":1,"handbook":"` + dir + `/HANDBOOK.md","templates":{"task":"` + dir + `/task.json"}}`),
		dir + "/HANDBOOK.md": []byte("# Handbook\n\nExported rules.\n"),
		dir + "/task.json":   []byte(`{"title":""}`),
	}}
	sel := process.Ref{Repo: repo, Commit: commit, Manifest: manifest}
	archive, sums, err := writeExport(b, sel, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return archive, sums
}

func TestProcessSelectImportsTheSetAndReusesAKeptOne(t *testing.T) {
	reg := projectService(t)
	if _, err := reg.Open("example"); err != nil {
		t.Fatal(err)
	}
	const repo = "https://example.com/team/process.git"
	c1, c2 := strings.Repeat("ab", 20), strings.Repeat("cd", 20)
	archive, sums := exportFixture(t, repo, c1, "process/manifest.json")
	if filepath.Base(archive) != "process-"+c1[:12]+".tar.gz" {
		t.Fatalf("archive name: %s", archive)
	}
	// The sums file names exactly this archive; exporting again replaces
	// the line instead of adding one.
	if err := process.VerifySums(archive, sums); err != nil {
		t.Fatal(err)
	}
	b, err := process.ReadArchiveFile(archive, sums, "process/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeExport(b, process.Ref{Repo: repo, Commit: c1, Manifest: "process/manifest.json"}, filepath.Dir(archive)); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(sums); strings.Count(string(raw), "\n") != 1 {
		t.Fatalf("SHA256SUMS after a second export:\n%s", raw)
	}

	var out struct {
		Current  process.Ref `json:"current"`
		Imported bool        `json:"imported"`
	}
	selectOut := func(args ...string) (string, error) {
		return stdoutOf(t, func() error { return processCmd(append([]string{"select"}, args...)) })
	}
	// A tampered archive is refused before anything reaches the hub.
	tampered := filepath.Join(t.TempDir(), filepath.Base(archive))
	raw, _ := os.ReadFile(archive)
	if err := os.WriteFile(tampered, append(raw, 0), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := selectOut(repo, c1, "process/manifest.json", "--from-archive", tampered, "--sums", sums, "-p", "example"); err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("tampered archive: %v", err)
	}
	// The verified archive is imported with the selection.
	text, err := selectOut(repo, c1, "process/manifest.json", "--from-archive", archive, "-p", "example")
	if err != nil || json.Unmarshal([]byte(text), &out) != nil || !out.Imported || out.Current.Digest != b.Digest() || out.Current.Name != "process" {
		t.Fatalf("import: %v\n%s", err, text)
	}
	// Another version, then the first again: the hub keeps its set, so the
	// second selection of it needs no archive and no Git.
	archive2, _ := exportFixture(t, repo, c2, "process/manifest.json")
	if _, err := selectOut(repo, c2, "process/manifest.json", "--from-archive", archive2, "--expect", c1, "-p", "example"); err != nil {
		t.Fatal(err)
	}
	text, err = selectOut(repo, c1, "process/manifest.json", "--expect", c2, "-p", "example")
	if err != nil || json.Unmarshal([]byte(text), &out) != nil || out.Imported || out.Current.Digest != b.Digest() {
		t.Fatalf("re-select a kept set: %v\n%s", err, text)
	}
	// A version the hub does not keep and no archive: the set must come
	// from Git, and this address serves nothing, so nothing is selected.
	c3 := strings.Repeat("ef", 20)
	if _, err := selectOut("https://127.0.0.1:1/process.git", c3, "process/manifest.json", "--expect", c1, "-p", "example"); err == nil || !strings.Contains(err.Error(), "reading the set") {
		t.Fatalf("unreadable repository: %v", err)
	}
	text, err = stdoutOf(t, func() error { return projectNamespaceCmd([]string{"show", "-p", "example"}) })
	if err != nil || !strings.Contains(text, "process:    "+repo+" "+c1) {
		t.Fatalf("selection after a failed import: %v\n%s", err, text)
	}
	// export needs the three values and reads from Git only.
	if _, err := stdoutOf(t, func() error { return processCmd([]string{"export", repo, c1}) }); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("export without a manifest: %v", err)
	}
}
