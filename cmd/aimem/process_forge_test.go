package main

// On a hub host without git, `aimem process refresh` reads a selection's
// set from the forge's tarball of the commit over https; with git, Git is
// read first and the forge is not asked.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"aimem/internal/process"
)

// forgeTarball is a forge's tarball of commit holding files: git archive's
// pax global header naming the commit, then every file below prefix.
func forgeTarball(t *testing.T, commit, prefix string, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": commit}, Format: tar.FormatPAX}); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: prefix + p, Mode: 0o644, Size: int64(len(files[p]))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(files[p])
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestProcessRefreshReadsTheForgeTarballWithoutGit(t *testing.T) {
	reg := projectService(t)
	const manifest = "process/manifest.json"
	c1, c2 := strings.Repeat("ab", 20), strings.Repeat("cd", 20)

	served := map[string][]byte{}
	hits := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		b, ok := served[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer srv.Close()
	repo := srv.URL + "/team/process.git"

	present, gitCalls := false, 0
	var gitBundle *process.Bundle
	oldPresent, oldGit, oldClient := gitPresent, fetchGit, forgeClient
	gitPresent = func() bool { return present }
	fetchGit = func(sel process.Ref) (*process.Bundle, error) {
		gitCalls++
		if gitBundle == nil {
			return nil, errors.New("unavailable: git unreachable")
		}
		return gitBundle, nil
	}
	forgeClient = srv.Client()
	t.Cleanup(func() { gitPresent, fetchGit, forgeClient = oldPresent, oldGit, oldClient })

	legacy := func(project, commit string) {
		t.Helper()
		db, err := reg.Open(project)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(process.Ref{Repo: repo, Ref: "main", Commit: commit, Manifest: manifest, SelectedAt: "2026-10-01T00:00:00Z"})
		if err := db.SetMeta("process", string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	selection := func(project string) process.Ref {
		t.Helper()
		ref, err := currentSelection(project)
		if err != nil || ref == nil {
			t.Fatalf("%s: %v %v", project, ref, err)
		}
		return *ref
	}
	refresh := func(project string) string {
		t.Helper()
		out, err := stdoutOf(t, func() error { return processCmd([]string{"refresh", "-p", project}) })
		if err != nil {
			t.Fatalf("refresh %s: %v\n%s", project, err, out)
		}
		return out
	}
	// The set the archive import gives for these files: the forge tarball
	// must give the same one.
	archive, sums := exportFixture(t, repo, c1, manifest)
	want, err := process.ReadArchiveFile(archive, sums, manifest)
	if err != nil {
		t.Fatal(err)
	}

	// No git on this host: the GitHub layout, then the Gitea one.
	for i, prefix := range []string{"process-" + c1 + "/", "process/"} {
		project := []string{"github", "gitea"}[i]
		legacy(project, c1)
		served["/team/process/archive/"+c1+".tar.gz"] = forgeTarball(t, c1, prefix, want.Files)
		out := refresh(project)
		if !strings.Contains(out, "refreshed (set "+want.Digest()[:12]) || !strings.Contains(out, "from the forge tarball") {
			t.Fatalf("refresh without git, %s layout:\n%s", project, out)
		}
		if got := selection(project); got.Digest != want.Digest() || got.Commit != c1 {
			t.Fatalf("%s layout stored %+v", project, got)
		}
	}
	if gitCalls != 0 {
		t.Fatalf("git was called %d times on a host without it", gitCalls)
	}

	// A tarball that records another commit is refused; nothing is stored.
	legacy("other", c2)
	served["/team/process/archive/"+c2+".tar.gz"] = forgeTarball(t, c1, "process/", want.Files)
	out := refresh("other")
	if !strings.Contains(out, "needs an archive") || !strings.Contains(out, "git is not installed on this host") || !strings.Contains(out, `records commit "`+c1+`", not `+c2) {
		t.Fatalf("another commit's tarball:\n%s", out)
	}
	if got := selection("other"); got.Digest != "" {
		t.Fatalf("a refused tarball was stored: %+v", got)
	}

	// With git present, Git is read first and the forge is not asked.
	present, gitBundle = true, want
	legacy("withgit", c1)
	before := hits
	out = refresh("withgit")
	if !strings.Contains(out, "from Git") || gitCalls != 1 || hits != before {
		t.Fatalf("refresh with git (%d git calls, %d forge requests):\n%s", gitCalls, hits-before, out)
	}
	if got := selection("withgit"); got.Digest != want.Digest() {
		t.Fatalf("with git stored %+v", got)
	}
	// Git present but failing: the forge tarball is the fallback.
	gitBundle = nil
	legacy("gitfails", c1)
	out = refresh("gitfails")
	if !strings.Contains(out, "from the forge tarball") || gitCalls != 2 {
		t.Fatalf("refresh when git fails (%d git calls):\n%s", gitCalls, out)
	}
}
