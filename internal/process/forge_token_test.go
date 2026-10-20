package process

// A forge token goes only to the repository's own host, and a tag resolves
// without Git through the forge's ref advertisement to the commit git
// ls-remote gives.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const testForgeToken = "forge-secret-0123456789"

// tagFixture is a repository with annotated, lightweight and nested tags,
// a branch and a branch named like a tag, and its first and second commit.
func tagFixture(t *testing.T) (dir, first, second string) {
	t.Helper()
	dir, first = makeRepo(t, fixtureFiles())
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("tag", "v1-light")
	run("tag", "-a", "v1", "-m", "release v1")
	run("commit", "-q", "--allow-empty", "-m", "second")
	second = run("rev-parse", "HEAD")
	run("tag", "-a", "v2", "-m", "release v2")
	// A tag whose last component is v1, on another commit: only the exact
	// name refs/tags/v1 may select.
	run("tag", "-a", "nested/v1", "-m", "not v1")
	run("branch", "feature", first)
	run("branch", "v1-light", second) // a branch named like a tag, on another commit
	if tagObject := run("rev-parse", "v1"); tagObject == first {
		t.Fatal("the fixture's v1 must be an annotated tag object")
	}
	return dir, first, second
}

// advertisement is what a smart-HTTP server answers to
// GET <repo>/info/refs?service=git-upload-pack for dir: the service line,
// a flush, then git upload-pack's own advertisement.
func advertisement(t *testing.T, dir string) []byte {
	t.Helper()
	out, err := exec.Command("git", "upload-pack", "--stateless-rpc", "--advertise-refs", dir).Output()
	if err != nil {
		t.Fatalf("git upload-pack: %v", err)
	}
	return append([]byte("001e# service=git-upload-pack\n0000"), out...)
}

func TestResolveForgeMatchesLsRemote(t *testing.T) {
	dir, _, _ := tagFixture(t)
	adv := advertisement(t, dir)
	var auths []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "token "+testForgeToken {
			http.NotFound(w, r) // a private repository, as GitHub and Gitea answer it
			return
		}
		switch {
		case r.URL.Query().Get("service") != "git-upload-pack":
			http.Error(w, "dumb protocol not served", http.StatusForbidden)
		case r.URL.Path == "/team/process.git/info/refs", r.URL.Path == "/team/process/info/refs":
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			w.Write(adv)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	ctx := context.Background()

	// The GitHub form of the URL (.git) and the Gitea one (without): every
	// name resolves exactly as git ls-remote resolves it, refusals included.
	for _, repo := range []string{srv.URL + "/team/process.git", srv.URL + "/team/process/"} {
		for _, name := range []string{"v1", "v1-light", "v2", "nested/v1", "feature", "main", "v9"} {
			want, wantErr := resolveNoValidate(ctx, dir, name)
			got, err := ResolveForge(ctx, srv.Client(), repo, name, testForgeToken)
			if got != want || (err == nil) != (wantErr == nil) || (err != nil && err.Error() != wantErr.Error()) {
				t.Fatalf("%s %s: forge %q %v, git ls-remote %q %v", repo, name, got, err, want, wantErr)
			}
		}
	}
	if got, _ := resolveNoValidate(ctx, dir, "v1"); got == "" {
		t.Fatal("the fixture must resolve v1")
	}

	// Without the token the private repository is not found, and the error
	// says so without naming any token.
	_, err := ResolveForge(ctx, srv.Client(), srv.URL+"/team/process.git", "v1", "")
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("without the token: %v", err)
	}
	if auths[len(auths)-1] != "" {
		t.Fatalf("an Authorization header was sent without a token: %q", auths[len(auths)-1])
	}
	// What a selection could not hold is refused before anything is asked.
	before := len(auths)
	for _, c := range []struct{ repo, name, want string }{
		{"git@example.com:team/process.git", "v1", "not an https URL"},
		{srv.URL + "/team/process.git", "v1..v2", "plain branch or tag"},
		{"file:///tmp/process", "v1", "repo must start with"},
	} {
		if _, err := ResolveForge(ctx, srv.Client(), c.repo, c.name, testForgeToken); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("ResolveForge(%q, %q): %v", c.repo, c.name, err)
		}
	}
	if len(auths) != before {
		t.Fatalf("a refused name or repository reached the forge (%d requests)", len(auths)-before)
	}
}

func TestAdvertisedRefsRefusesWhatIsNotOne(t *testing.T) {
	for name, data := range map[string]string{
		"empty":           "",
		"dumb info/refs":  strings.Repeat("ab", 20) + "\trefs/tags/v1\n",
		"no service line": "003a" + strings.Repeat("ab", 20) + " refs/tags/v1\n" + "0000",
		"truncated":       "001e# service=git-upload-pack\n00",
		"overlong length": "ffff# service=git-upload-pack\n",
		"short length":    "0003",
	} {
		if _, err := advertisedRefs([]byte(data)); err == nil || !strings.Contains(err.Error(), "not a Git ref advertisement") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestForgeTokenGoesOnlyToTheRepositoryHost(t *testing.T) {
	repo, sha := makeRepo(t, fixtureFiles())
	const manifest = "process/manifest.json"
	tarball := gitArchive(t, repo, sha, "process/")

	// The archive's other host, as codeload.github.com is GitHub's: it
	// records what it receives and serves the tarball to anyone.
	var elsewhere []string
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere = append(elsewhere, r.Header.Get("Authorization"))
		w.Write(tarball)
	}))
	defer other.Close()
	// The repository's host: a private repository, found only with the
	// token. Under /same/ it redirects on its own host, under /team/ to the
	// other host.
	var home []string
	forge := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		home = append(home, r.Header.Get("Authorization"))
		switch {
		case r.Header.Get("Authorization") != "token "+testForgeToken:
			http.NotFound(w, r)
		case r.URL.Path == "/same/process/archive/"+sha+".tar.gz":
			http.Redirect(w, r, "/served/"+sha+".tar.gz", http.StatusFound)
		case r.URL.Path == "/served/"+sha+".tar.gz":
			w.Write(tarball)
		case r.URL.Path == "/team/process/archive/"+sha+".tar.gz":
			http.Redirect(w, r, other.URL+"/team/process/tar.gz/"+sha, http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer forge.Close()
	ctx := context.Background()
	ref := Ref{Repo: forge.URL + "/team/process.git", Commit: sha, Manifest: manifest}

	// With the token: the forge sees it, the other host does not, and the
	// set is read.
	b, err := FetchForgeArchive(ctx, forge.Client(), ref, testForgeToken)
	if err != nil {
		t.Fatalf("with the token: %v", err)
	}
	if len(b.Files) == 0 || len(home) != 1 || home[0] != "token "+testForgeToken {
		t.Fatalf("with the token: %d files, forge saw %q", len(b.Files), home)
	}
	if len(elsewhere) != 1 || elsewhere[0] != "" {
		t.Fatalf("the redirect target on another host received %q", elsewhere)
	}
	// A redirect on the repository's own host keeps it.
	same := ref
	same.Repo = forge.URL + "/same/process.git"
	if _, err := FetchForgeArchive(ctx, forge.Client(), same, testForgeToken); err != nil {
		t.Fatalf("a redirect on the same host: %v", err)
	}
	if got := home[len(home)-1]; got != "token "+testForgeToken {
		t.Fatalf("the same host's redirect target received %q", got)
	}
	// The client's own redirect policy still applies.
	strict := *forge.Client()
	strict.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if _, err := FetchForgeArchive(ctx, &strict, ref, testForgeToken); err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("a client that does not follow redirects: %v", err)
	}
	// Without the token the private repository is not found, and nothing
	// carries a token.
	before := len(elsewhere)
	_, err = FetchForgeArchive(ctx, forge.Client(), ref, "")
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("without the token: %v", err)
	}
	if home[len(home)-1] != "" || len(elsewhere) != before {
		t.Fatalf("without the token: forge saw %q, other host %d requests", home[len(home)-1], len(elsewhere)-before)
	}
	// No error names the token, whatever fails.
	for _, r := range []Ref{ref, {Repo: forge.URL + "/team/missing.git", Commit: sha, Manifest: manifest}, {Repo: forge.URL + "/team/process.git", Commit: sha, Manifest: "process/none.json"}} {
		if _, err := FetchForgeArchive(ctx, &strict, r, testForgeToken); err != nil && strings.Contains(err.Error(), testForgeToken) {
			t.Fatalf("an error names the token: %v", err)
		}
	}
}
