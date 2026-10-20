package main

// AIMEM_PROCESS_FORGE_TOKENS gives each forge host its own token: a hub
// whose projects span forges sends each forge only its own, the one
// AIMEM_PROCESS_FORGE_TOKEN stays the default for any other host, and no
// token is printed or stored.

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aimem/internal/process"
)

func TestProcessForgeTokensPerHost(t *testing.T) {
	reg := projectService(t)
	const manifest = "process/manifest.json"
	c1 := strings.Repeat("ab", 20)
	archive, sums := exportFixture(t, "https://example.com/team/process.git", c1, manifest)
	want, err := process.ReadArchiveFile(archive, sums, manifest)
	if err != nil {
		t.Fatal(err)
	}
	tarball := forgeTarball(t, c1, "process/", want.Files)

	// Three forges on three hosts (one address, three ports), each a
	// private repository that serves only its own token.
	var mu sync.Mutex
	seen := map[string][]string{}
	forge := func(name, token string) *httptest.Server {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[name] = append(seen[name], r.Header.Get("Authorization"))
			mu.Unlock()
			if r.Header.Get("Authorization") != "token "+token || r.URL.Path != "/team/process/archive/"+c1+".tar.gz" {
				http.NotFound(w, r)
				return
			}
			w.Write(tarball)
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	tokens := map[string]string{"alpha": "alpha-secret-0123456789", "beta": "beta-secret-0123456789", "other": "default-secret-0123456789"}
	alpha, beta, other := forge("alpha", tokens["alpha"]), forge("beta", tokens["beta"]), forge("other", tokens["other"])
	host := func(s *httptest.Server) string { u, _ := url.Parse(s.URL); return u.Host }

	oldPresent, oldClient := gitPresent, forgeClient
	gitPresent = func() bool { return false }
	forgeClient = alpha.Client() // httptest servers share one certificate
	t.Cleanup(func() { gitPresent, forgeClient = oldPresent, oldClient })
	t.Setenv(forgeTokensEnv, host(alpha)+"="+tokens["alpha"]+",\n  "+strings.ToUpper(host(beta))+"="+tokens["beta"])
	t.Setenv(forgeTokenEnv, tokens["other"])

	for project, srv := range map[string]*httptest.Server{"alpha": alpha, "beta": beta, "other": other} {
		db, err := reg.Open(project)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(process.Ref{Repo: srv.URL + "/team/process.git", Commit: c1, Manifest: manifest, SelectedAt: "2026-10-01T00:00:00Z"})
		if err := db.SetMeta("process", string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	out, err := stdoutOf(t, func() error { return processCmd([]string{"refresh"}) })
	if err != nil {
		t.Fatalf("refresh: %v\n%s", err, out)
	}
	for _, project := range []string{"alpha", "beta", "other"} {
		ref, err := currentSelection(project)
		if err != nil || ref == nil || ref.Digest != want.Digest() {
			t.Fatalf("%s: %+v %v\n%s", project, ref, err, out)
		}
		// Each forge saw its own token and no other.
		if got := seen[project]; len(got) != 1 || got[0] != "token "+tokens[project] {
			t.Fatalf("forge %s received %q", project, got)
		}
	}

	// No token in what refresh said or in any file of the hub's state.
	for _, tok := range tokens {
		if strings.Contains(out, tok) {
			t.Fatalf("refresh output names a token:\n%s", out)
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
		for _, tok := range tokens {
			if bytes.Contains(b, []byte(tok)) {
				t.Errorf("%s holds a token", p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A malformed setting fails the read, naming the entry, never a token:
	// nothing is stored and no forge is asked.
	t.Setenv(forgeTokensEnv, host(alpha)+"="+tokens["alpha"]+" https://"+host(beta)+"="+tokens["beta"])
	if db, err := reg.Open("broken"); err != nil {
		t.Fatal(err)
	} else {
		raw, _ := json.Marshal(process.Ref{Repo: alpha.URL + "/team/process.git", Commit: c1, Manifest: manifest, SelectedAt: "2026-10-01T00:00:00Z"})
		db.SetMeta("process", string(raw))
	}
	asked := len(seen["alpha"])
	out, err = stdoutOf(t, func() error { return processCmd([]string{"refresh", "-p", "broken"}) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, forgeTokensEnv+": entry 2 is not host=token") || len(seen["alpha"]) != asked {
		t.Fatalf("a malformed setting (%d requests):\n%s", len(seen["alpha"])-asked, out)
	}
	for _, tok := range tokens {
		if strings.Contains(out, tok) {
			t.Fatalf("the refusal names a token:\n%s", out)
		}
	}
}

func TestForgeTokenForMatchesTheHost(t *testing.T) {
	t.Setenv(forgeTokenEnv, "plain")
	t.Setenv(forgeTokensEnv, "github.com:443=gh git.example.org:3000=gitea , Git.Example.Org=gitea-default")
	for repo, want := range map[string]string{
		"https://github.com/team/process.git":           "gh",            // :443 implied on the entry
		"https://GitHub.com:443/team/process":           "gh",            // and on the URL; case ignored
		"https://git.example.org:3000/team/process.git": "gitea",         // the port must match
		"https://git.example.org/team/process.git":      "gitea-default", // another port is another host
		"https://codeberg.org/team/process.git":         "plain",         // no entry: the one token
		"https://github.com.evil.example/team/proc.git": "plain",         // a suffix is not the host
		"https://user@github.com/team/process.git":      "gh",            // user info is not the host
		"git@github.com:team/process.git":               "plain",         // not a URL: no host to match
	} {
		got, err := forgeTokenFor(repo)
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", repo, got, err, want)
		}
	}
	t.Setenv(forgeTokenEnv, "")
	if got, err := forgeTokenFor("https://codeberg.org/team/process.git"); err != nil || got != "" {
		t.Fatalf("no entry and no default: %q %v", got, err)
	}
	for i, bad := range []string{"github.com", "=secret-value", "github.com=", "https://github.com=secret-value", "github.com/team=secret-value"} {
		t.Setenv(forgeTokensEnv, "ok.example=fine "+bad)
		_, err := forgeTokenFor("https://ok.example/team/process.git")
		if err == nil || !strings.Contains(err.Error(), "entry 2 is not host=token") || strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "fine") {
			t.Fatalf("case %d: %v", i, err)
		}
	}
}
