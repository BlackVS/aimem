package main

// `aimem process select --from-archive` takes the commit the archive
// records: by a tag, with no resolution over the network, the tag stored
// as the ref; by --commit, only the archive's own commit. An archive that
// records no commit is refused, as `process default set` refuses it.

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/process"
)

type refuseTransport struct{ t *testing.T }

func (r refuseTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Errorf("a forge read happened: %s", req.URL)
	return nil, errors.New("no forge read in this test")
}

func TestProcessSelectTakesTheArchivesCommit(t *testing.T) {
	reg := projectService(t)
	if _, err := reg.Open("example"); err != nil {
		t.Fatal(err)
	}
	// Nothing may be read from Git or the forge: the archive is the source.
	oldResolve, oldGit, oldClient := resolveRef, fetchGit, forgeClient
	t.Cleanup(func() { resolveRef, fetchGit, forgeClient = oldResolve, oldGit, oldClient })
	resolveRef = func(repo, name string) (string, error) {
		t.Errorf("the tag %s was resolved over the network", name)
		return "", errors.New("no resolution in this test")
	}
	fetchGit = func(sel process.Ref) (*process.Bundle, error) {
		t.Errorf("Git was read for %s", sel.Commit)
		return nil, errors.New("no Git in this test")
	}
	forgeClient = &http.Client{Transport: refuseTransport{t}}

	const repo = "https://github.com/BlackVS/aimem"
	const manifest = "processes/aimem/manifest.json"
	c1, c2 := strings.Repeat("a1", 20), strings.Repeat("b2", 20)
	exported, _ := exportFixture(t, repo, c1, manifest)
	// The release's fixed name, with its SHA256SUMS beside it.
	archive := filepath.Join(t.TempDir(), "aimem-process.tar.gz")
	sums := copyWithSums(t, exported, archive)

	run := func(args ...string) error {
		_, err := stdoutOf(t, func() error {
			return processCmd(append(append([]string{"select"}, args...), "--from-archive", archive, "--sums", sums, "-p", "example"))
		})
		return err
	}
	selected := func() *process.Ref {
		t.Helper()
		ref, err := currentSelection("example")
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}

	// A typed commit that is not the archive's is refused, naming both,
	// and nothing is stored.
	if err := run(repo, c2, manifest, "--commit"); err == nil || !strings.Contains(err.Error(), "records commit "+c1+", not "+c2) || !strings.Contains(err.Error(), "nothing selected") {
		t.Fatalf("a commit the archive does not record: %v", err)
	}
	if got := selected(); got != nil {
		t.Fatalf("a refused select stored %+v", got)
	}

	// By a tag: the archive's commit, the tag as the ref.
	if err := run(repo, "v0.10.25", manifest); err != nil {
		t.Fatal(err)
	}
	got := selected()
	if got == nil || got.Commit != c1 || got.Ref != "v0.10.25" || got.Repo != repo || got.Manifest != manifest || got.Digest == "" {
		t.Fatalf("select by a tag from the archive: %+v", got)
	}

	// By the archive's own commit: as before.
	if err := run(repo, c1, manifest, "--commit", "--expect", c1); err != nil {
		t.Fatalf("the archive's own commit: %v", err)
	}
	if again := selected(); again == nil || again.Commit != c1 || again.Digest != got.Digest {
		t.Fatalf("select by the archive's commit: %+v", again)
	}

	// An archive that records no commit is refused in both forms.
	plain := filepath.Join(t.TempDir(), "aimem-process.tar.gz")
	writeArchiveWithoutCommit(t, plain, manifest)
	plainSums := filepath.Join(filepath.Dir(plain), "SHA256SUMS")
	for _, args := range [][]string{{repo, "v0.10.26", manifest}, {repo, c2, manifest, "--commit"}} {
		_, err := stdoutOf(t, func() error {
			return processCmd(append(append([]string{"select"}, args...), "--from-archive", plain, "--sums", plainSums, "--expect", c1, "-p", "example"))
		})
		if err == nil || !strings.Contains(err.Error(), "records no commit") {
			t.Fatalf("%v with an archive that records no commit: %v", args, err)
		}
	}
	if after := selected(); after == nil || after.Commit != c1 {
		t.Fatalf("a refused select changed the selection: %+v", after)
	}
}
