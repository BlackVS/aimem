package process

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestForgeArchiveURL(t *testing.T) {
	c := strings.Repeat("ab", 20)
	for repo, want := range map[string]string{
		"https://github.com/org/process.git":        "https://github.com/org/process/archive/" + c + ".tar.gz",
		"https://git.example.internal/org/process":  "https://git.example.internal/org/process/archive/" + c + ".tar.gz",
		"https://git.example.internal/org/process/": "https://git.example.internal/org/process/archive/" + c + ".tar.gz",
		"git@github.com:org/process.git":            "",
		"ssh://git@github.com/org/process.git":      "",
	} {
		if got := ForgeArchiveURL(repo, c); got != want {
			t.Errorf("%s: %q, want %q", repo, got, want)
		}
	}
}

// gitArchive is what a forge serves for a commit: git archive of it, with
// the forge's leading directory. A forge's git converts no line endings; a
// Windows runner's core.autocrlf would, and git show never does.
func gitArchive(t *testing.T, repo, commit, prefix string) []byte {
	t.Helper()
	out, err := exec.Command("git", "-c", "core.autocrlf=false", "-C", repo, "archive", "--format=tar.gz", "--prefix="+prefix, commit).Output()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A set read from a forge tarball, in the GitHub and the Gitea layout, is
// the set Git gives for the same commit; a tarball of another commit, one
// that records none, or one without the manifest is refused.
func TestFetchForgeArchiveReadsTheSetGitGives(t *testing.T) {
	repo, sha := makeRepo(t, fixtureFiles())
	const manifest = "process/manifest.json"
	viaGit := fetchNoValidate(context.Background(), t.TempDir(), Ref{Repo: "file://" + filepath.ToSlash(repo), Commit: sha, Manifest: manifest})
	if viaGit.Set == nil {
		t.Fatalf("git: %s %v", viaGit.Status, viaGit.Err)
	}
	want, err := BundleFromDir(viaGit.Set.Dir, viaGit.Set.Ref)
	if err != nil {
		t.Fatal(err)
	}

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
	ref := Ref{Repo: srv.URL + "/team/process.git", Commit: sha, Manifest: manifest}
	if err := ref.Validate(); err != nil {
		t.Fatalf("the test selection must be one the hub accepts: %v", err)
	}
	path := "/team/process/archive/" + sha + ".tar.gz"
	fetch := func(r Ref) (*Bundle, error) { return FetchForgeArchive(context.Background(), srv.Client(), r, "") }

	for layout, prefix := range map[string]string{"GitHub": "process-" + sha + "/", "Gitea": "process/"} {
		served[path] = gitArchive(t, repo, sha, prefix)
		got, err := fetch(ref)
		if err != nil {
			t.Fatalf("%s layout: %v", layout, err)
		}
		if got.Digest() != want.Digest() || len(got.Files) != len(want.Files) {
			t.Fatalf("%s layout: set %s (%d files), Git gives %s (%d files)", layout, got.Digest()[:12], len(got.Files), want.Digest()[:12], len(want.Files))
		}
	}

	// The tarball of another commit, served under this commit's URL.
	other := fixtureFiles()
	other["process/HANDBOOK.md"] = "# Handbook\n\nAnother version.\n"
	repo2, sha2 := makeRepo(t, other)
	served[path] = gitArchive(t, repo2, sha2, "process/")
	if _, err := fetch(ref); err == nil || !strings.Contains(err.Error(), "records commit \""+sha2+"\", not "+sha) {
		t.Fatalf("another commit's tarball: %v", err)
	}
	// An archive that records no commit (not made by git archive).
	var plain strings.Builder
	if err := WriteArchive(&plain, want); err != nil {
		t.Fatal(err)
	}
	served[path] = []byte(plain.String())
	if _, err := fetch(ref); err == nil || !strings.Contains(err.Error(), `records commit "", not `+sha) {
		t.Fatalf("a tarball without a commit: %v", err)
	}
	// The right commit, but no manifest at the selected path.
	served[path] = gitArchive(t, repo, sha, "process/")
	missing := ref
	missing.Manifest = "process/other.json"
	if _, err := fetch(missing); err == nil || !strings.Contains(err.Error(), "no process/other.json in it") {
		t.Fatalf("missing manifest: %v", err)
	}
	// Nothing at the URL.
	gone := ref
	gone.Commit = strings.Repeat("cd", 20)
	if _, err := fetch(gone); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("no tarball: %v", err)
	}
	// An ssh repository has no tarball URL, and nothing is asked.
	before := hits
	sshRef := ref
	sshRef.Repo = "git@example.com:team/process.git"
	if _, err := fetch(sshRef); err == nil || !strings.Contains(err.Error(), "not an https URL") || hits != before {
		t.Fatalf("ssh repository: %v (%d requests)", err, hits-before)
	}
}
