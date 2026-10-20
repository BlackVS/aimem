package main

// On a hub host without git, AIMEM_PROCESS_FORGE_TOKEN in the hub's env
// file lets refresh read a private repository's tarball and select resolve
// a tag through the forge's ref advertisement; the token is never printed
// or stored.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/process"
)

// pktLines is a smart-HTTP ref advertisement: the service line, a flush,
// then each ref line ("<commit> <ref>", the first with capabilities).
func pktLines(refs ...string) []byte {
	var b bytes.Buffer
	pkt := func(s string) { fmt.Fprintf(&b, "%04x%s", len(s)+4, s) }
	pkt("# service=git-upload-pack\n")
	b.WriteString("0000")
	for i, r := range refs {
		if i == 0 {
			r += "\x00multi_ack side-band-64k ofs-delta"
		}
		pkt(r + "\n")
	}
	b.WriteString("0000")
	return b.Bytes()
}

func TestProcessForgeTokenFromTheEnvFile(t *testing.T) {
	reg := projectService(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv(forgeTokenEnv, "") // restored after the test; loadEnvFile sets it
	os.Unsetenv(forgeTokenEnv)
	const token = "forge-secret-cli-0123456789"
	const manifest = "process/manifest.json"
	c1, c2, tagObject := strings.Repeat("ab", 20), strings.Repeat("cd", 20), strings.Repeat("ef", 20)

	archive, sums := exportFixture(t, "https://example.com/team/process.git", c1, manifest)
	want, err := process.ReadArchiveFile(archive, sums, manifest)
	if err != nil {
		t.Fatal(err)
	}
	adv := pktLines(
		c1+" refs/tags/v1-light", // lightweight: the commit itself; first, so it carries the capabilities
		c2+" refs/heads/main",
		tagObject+" refs/tags/v1",
		c1+" refs/tags/v1^{}", // annotated: peeled to its commit
	)
	var sent []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "token "+token {
			http.NotFound(w, r) // a private repository
			return
		}
		switch r.URL.Path {
		case "/team/process/archive/" + c1 + ".tar.gz":
			w.Write(forgeTarball(t, c1, "process/", want.Files))
		case "/team/process.git/info/refs", "/team/process/info/refs":
			if r.URL.Query().Get("service") != "git-upload-pack" {
				http.NotFound(w, r)
				return
			}
			w.Write(adv)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	repo := srv.URL + "/team/process.git"

	oldPresent, oldGit, oldClient := gitPresent, fetchGit, forgeClient
	gitPresent = func() bool { return false }
	fetchGit = func(process.Ref) (*process.Bundle, error) {
		t.Fatal("git called on a host without it")
		return nil, nil
	}
	forgeClient = srv.Client()
	t.Cleanup(func() { gitPresent, fetchGit, forgeClient = oldPresent, oldGit, oldClient })

	var said []string // everything the commands printed or returned
	run := func(args ...string) error {
		t.Helper()
		out, err := stdoutOf(t, func() error { return processCmd(args) })
		said = append(said, out)
		if err != nil {
			said = append(said, err.Error())
		}
		return err
	}
	legacy := func(project string) {
		t.Helper()
		db, err := reg.Open(project)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(process.Ref{Repo: repo, Commit: c1, Manifest: manifest, SelectedAt: "2026-10-01T00:00:00Z"})
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

	// No token configured: the private repository is not found, refresh
	// keeps the needs-an-archive line, and a tag does not resolve.
	legacy("legacy")
	if err := run("refresh", "-p", "legacy"); err != nil {
		t.Fatal(err)
	}
	if out := said[len(said)-1]; !strings.Contains(out, "needs an archive") || !strings.Contains(out, "HTTP 404") {
		t.Fatalf("refresh without the token:\n%s", out)
	}
	if got := selection("legacy"); got.Digest != "" {
		t.Fatalf("stored without the token: %+v", got)
	}
	if _, err := reg.Open("tagged"); err != nil {
		t.Fatal(err)
	}
	if err := run("select", repo, "v1", manifest, "-p", "tagged"); err == nil || !strings.Contains(err.Error(), "from the forge: GET") || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("select by tag without the token: %v", err)
	}
	for _, a := range sent {
		if a != "" {
			t.Fatalf("an Authorization header was sent with no token configured: %q", a)
		}
	}

	// The token in the hub's env file, as the installer writes settings.
	if err := os.MkdirAll(filepath.Join(home, ".config", "aimem"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "aimem", "env"), []byte("AIMEM_HUB=https://hub.example\n"+forgeTokenEnv+"="+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loadEnvFile()
	if forgeToken() != token {
		t.Fatal("the env file's token was not loaded")
	}

	if err := run("refresh", "-p", "legacy"); err != nil {
		t.Fatal(err)
	}
	if out := said[len(said)-1]; !strings.Contains(out, "refreshed (set "+want.Digest()[:12]) || !strings.Contains(out, "from the forge tarball") {
		t.Fatalf("refresh with the token:\n%s", out)
	}
	if got := selection("legacy"); got.Digest != want.Digest() {
		t.Fatalf("refresh with the token stored %+v", got)
	}

	// select by tag: the annotated tag through its peeled line, the
	// lightweight one directly; the GitHub form of the URL and the Gitea one.
	for project, c := range map[string]struct{ repo, tag string }{
		"tagged":    {repo, "v1"},
		"light":     {srv.URL + "/team/process", "v1-light"},
		"annotated": {srv.URL + "/team/process", "v1"},
	} {
		if _, err := reg.Open(project); err != nil {
			t.Fatal(err)
		}
		if err := run("select", c.repo, c.tag, manifest, "-p", project); err != nil {
			t.Fatalf("select %s %s: %v", c.repo, c.tag, err)
		}
		if got := selection(project); got.Commit != c1 || got.Ref != c.tag || got.Repo != c.repo {
			t.Fatalf("select %s %s stored %+v", c.repo, c.tag, got)
		}
	}
	if err := run("select", repo, "v9", manifest, "-p", "tagged"); err == nil || !strings.Contains(err.Error(), `no tag or branch "v9"`) {
		t.Fatalf("an unknown tag: %v", err)
	}

	// The token is nowhere: not in what the commands said, not in the
	// stored selections, not in any file of the hub's state.
	for _, s := range said {
		if strings.Contains(s, token) {
			t.Fatalf("output names the token:\n%s", s)
		}
	}
	err = filepath.WalkDir(os.Getenv("AIMEM_STATE_DIR"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return nil // a socket or a file the hub holds open
		}
		if bytes.Contains(b, []byte(token)) {
			t.Errorf("%s holds the token", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
