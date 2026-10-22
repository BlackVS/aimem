package taskcred

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/hubtest"
)

func fixture(t *testing.T) (string, string, *httptest.Server, *string) {
	t.Helper()
	root, repo := t.TempDir(), t.TempDir()
	reply := `{"scope":"project","task_write":true}`
	ts := hubtest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/access/identity" || r.URL.Query().Get("project") != "alpha" {
			t.Errorf("wrong identity target %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reply))
	}))
	t.Cleanup(ts.Close)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, Pin: hubtest.Pin(ts), TaskToken: "global", Token: "checkpoint"}}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".aimem.json"), []byte(`{"project":"alpha","hub":"hub","future":{"keep":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return root, repo, ts, &reply
}

func token(ch string) string { return "aimem_user_" + strings.Repeat(ch, 64) }

func TestSymlinkedConfigSelection(t *testing.T) {
	root, repo, _, _ := fixture(t)
	configPath := filepath.Join(repo, ".aimem.json")
	target := filepath.Join(t.TempDir(), "config.json")
	if err := os.Rename(configPath, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, configPath); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable: %v", err)
		}
		t.Fatal(err)
	}
	s, err := Resolve(repo, root)
	if err != nil || s.Source != "user-hub" || s.Token != "global" {
		t.Fatalf("symlinked global config: %+v %v", s, err)
	}
	if local, err := LocalRequired(repo); err != nil || local {
		t.Fatalf("global bootstrap: local=%v err=%v", local, err)
	}
	raw := []byte(`{"project":"alpha","hub":"hub","task_credential":"local"}`)
	if err := os.WriteFile(target, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if local, err := LocalRequired(repo); err != nil || !local {
		t.Fatalf("local bootstrap: local=%v err=%v", local, err)
	}
	if _, err := Resolve(repo, root); err == nil {
		t.Fatal("symlinked local requirement fell back without a credential")
	}
	if err := Clear(repo, root); err == nil {
		t.Fatal("clear replaced a symlinked config")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(raw) {
		t.Fatal("clear changed the symlink target")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := Resolve(repo, root); err == nil {
		t.Fatal("broken config symlink fell back")
	}
}

func TestLocalSelectionRotationAndClear(t *testing.T) {
	root, repo, _, reply := fixture(t)
	s, err := Resolve(repo, root)
	if err != nil || s.Source != "user-hub" || s.Token != "global" {
		t.Fatalf("global: %+v %v", s, err)
	}
	for _, secret := range []string{token("a"), token("b")} {
		if err := Set(context.Background(), repo, root, secret); err != nil {
			t.Fatal(err)
		}
		s, err = Resolve(repo, root)
		if err != nil || s.Source != "project-local" || s.Token != secret {
			t.Fatalf("local resolution failed: %v", err)
		}
		raw, _ := json.Marshal(s)
		if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "checkpoint") {
			t.Fatal("source output leaks a secret")
		}
		configRaw, err := os.ReadFile(filepath.Join(repo, ".aimem.json"))
		if err != nil || !strings.Contains(string(configRaw), "future") || strings.Contains(string(configRaw), secret) {
			t.Fatal("config lost fields or stored secret")
		}
		path, err := credentialPath(root, s.Repo, false)
		if err != nil {
			t.Fatal(err)
		}
		onDisk, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS == "windows" && strings.Contains(string(onDisk), secret) {
			t.Fatal("Windows credential is not encrypted")
		}
	}
	*reply = `{"scope":"user","task_write":true}`
	if err := Set(context.Background(), repo, root, token("c")); err == nil {
		t.Fatal("accepted user token as local override")
	}
	s, err = Resolve(repo, root)
	if err != nil || s.Token != token("b") {
		t.Fatal("failed rotation damaged old credential")
	}
	if err := s.Validate(context.Background()); err == nil {
		t.Fatal("accepted mismatched remote scope")
	}
	if err := Clear(repo, root); err != nil {
		t.Fatal(err)
	}
	s, err = Resolve(repo, root)
	if err != nil || s.Token != "global" {
		t.Fatal("explicit clear did not restore user credential")
	}
}

func TestRequiredOverrideFailsClosed(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "directory", "project", "url", "hub", "clone", "mode", "null", "permissions"} {
		t.Run(kind, func(t *testing.T) {
			root, repo, _, _ := fixture(t)
			if err := Set(context.Background(), repo, root, token("a")); err != nil {
				t.Fatal(err)
			}
			s, err := Resolve(repo, root)
			if err != nil {
				t.Fatal(err)
			}
			path, err := credentialPath(root, s.Repo, false)
			if err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(repo, ".aimem.json")
			switch kind {
			case "missing":
				err = os.Remove(path)
			case "malformed":
				err = os.WriteFile(path, []byte("broken"), 0600)
			case "directory":
				if err = os.Remove(path); err == nil {
					err = os.Mkdir(path, 0700)
				}
			case "project":
				err = os.WriteFile(cfg, []byte(`{"project":"beta","task_credential":"local"}`), 0600)
			case "url":
				err = adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: "https://elsewhere.invalid", TaskToken: "global"}}, "hub")
			case "hub":
				err = adapter.SaveHubs(root, map[string]*adapter.HubConfig{"other": s.Hub}, "other")
			case "clone":
				raw, e := os.ReadFile(cfg)
				if e != nil {
					t.Fatal(e)
				}
				repo = t.TempDir()
				err = os.WriteFile(filepath.Join(repo, ".aimem.json"), raw, 0600)
			case "mode":
				err = os.WriteFile(cfg, []byte(`{"project":"alpha","task_credential":"typo"}`), 0600)
			case "null":
				err = os.WriteFile(cfg, []byte(`{"project":"alpha","task_credential":null}`), 0600)
			case "permissions":
				if runtime.GOOS == "windows" {
					t.Skip("Windows privacy is checked by DPAPI roundtrip")
				}
				err = os.Chmod(path, 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Resolve(repo, root); err == nil {
				t.Fatal("invalid local override fell back")
			}
		})
	}
}

func TestRejectRemoteAndRedirect(t *testing.T) {
	root, repo, _, reply := fixture(t)
	for _, response := range []string{`{}`, `{"scope":"read-only","task_write":false}`, `{"scope":"project","task_write":false}`, "not-json"} {
		*reply = response
		if err := Set(context.Background(), repo, root, token("a")); err == nil {
			t.Fatalf("accepted %s", response)
		}
	}
	for _, status := range []int{401, 403, 500, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			received := false
			target := hubtest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received = true }))
			defer target.Close()
			ts := hubtest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
			}))
			defer ts.Close()
			if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, Pin: hubtest.Pin(ts), TaskToken: "global"}}, "hub"); err != nil {
				t.Fatal(err)
			}
			if err := Set(context.Background(), repo, root, token("a")); err == nil {
				t.Fatal("accepted remote refusal")
			}
			if received {
				t.Fatal("credential followed redirect")
			}
		})
	}
}

func TestStateInsideCheckoutRefused(t *testing.T) {
	root, repo, _, _ := fixture(t)
	hubs, def := adapter.LoadHubs(root)
	root = filepath.Join(repo, "state")
	if err := adapter.SaveHubs(root, hubs, def); err != nil {
		t.Fatal(err)
	}
	if err := Set(context.Background(), repo, root, token("a")); err == nil {
		t.Fatal("secret stored in checkout")
	}
}

// A subdirectory binds its checkout's credential; a linked worktree that
// takes its project from the main checkout is configured separately: a
// marked main checkout makes it fail closed, never fall back, and the
// marker is never written into it while it inherits.
func TestWorktreeAndSubdirectoryBinding(t *testing.T) {
	root, repo, _, _ := fixture(t)
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(repo, "internal", "pkg")
	wt := filepath.Join(t.TempDir(), "wt")
	gitDir := filepath.Join(repo, ".git", "worktrees", "wt")
	for _, d := range []string{sub, wt, gitDir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.ToSlash(gitDir)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "commondir"), []byte("../..\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Set(context.Background(), repo, root, token("a")); err != nil {
		t.Fatal(err)
	}
	main, err := Resolve(repo, root)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := Resolve(sub, root); err != nil || s.Source != "project-local" || s.Token != token("a") || s.Repo != main.Repo {
		t.Fatalf("subdirectory: %+v %v", s, err)
	}
	if local, err := LocalRequired(wt); err != nil || !local {
		t.Fatalf("worktree bootstrap: local=%v err=%v", local, err)
	}
	if _, err := Resolve(wt, root); err == nil || Classify(err) != ClassMissing || !strings.Contains(err.Error(), "worktree") {
		t.Fatalf("worktree inherited a local requirement but resolved: %v", err)
	}

	// Without the marker the worktree uses the per-hub credential for the
	// inherited project, as the main checkout does, and setting a local one
	// asks for the worktree's own configuration first.
	if err := Clear(repo, root); err != nil {
		t.Fatal(err)
	}
	if s, err := Resolve(wt, root); err != nil || s.Source != "user-hub" || s.Token != "global" || s.Project != "alpha" {
		t.Fatalf("unmarked worktree: %+v %v", s, err)
	}
	if err := Set(context.Background(), wt, root, token("b")); err == nil {
		t.Fatal("set wrote a marker into an inheriting worktree")
	}
	if _, err := os.Stat(filepath.Join(wt, ".aimem.json")); !os.IsNotExist(err) {
		t.Fatalf("worktree config created: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".aimem.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".aimem.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Set(context.Background(), wt, root, token("b")); err != nil {
		t.Fatal(err)
	}
	if s, err := Resolve(wt, root); err != nil || s.Token != token("b") || s.Repo == main.Repo {
		t.Fatalf("configured worktree: %+v %v", s, err)
	}
	if s, err := Resolve(repo, root); err != nil || s.Token != "global" {
		t.Fatalf("main checkout after worktree set: %+v %v", s, err)
	}
}

// Both sources carry an individual credential, so both get the strict
// client: an http or insecure entry is refused before any request, and a
// pinned https entry validates.
func TestSelectionStrictClient(t *testing.T) {
	var hits atomic.Int32
	count := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write([]byte(`{"scope":"project","task_write":true}`))
	})
	plain := httptest.NewServer(count)
	defer plain.Close()
	secure := hubtest.NewServer(count)
	defer secure.Close()
	secret := token("s")
	for name, hub := range map[string]*adapter.HubConfig{
		"http":     {URL: plain.URL, Pin: hubtest.Pin(secure)},
		"insecure": {URL: secure.URL, Insecure: true},
	} {
		for _, source := range []string{"project-local", "user-hub"} {
			s := &Selection{Source: source, Project: "alpha", HubName: "hub", Hub: hub, Token: secret}
			_, err := s.Client()
			if !errors.Is(err, adapter.ErrUntrustedHub) || !strings.Contains(err.Error(), `hub "hub"`) || strings.Contains(err.Error(), secret) {
				t.Fatalf("%s, %s: %v", name, source, err)
			}
		}
		s := &Selection{Source: "project-local", Project: "alpha", HubName: "hub", Hub: hub, Token: secret}
		if err := s.Validate(context.Background()); !errors.Is(err, adapter.ErrUntrustedHub) {
			t.Fatalf("%s: validated: %v", name, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("a refused entry received %d requests", hits.Load())
	}
	s := &Selection{Source: "project-local", Project: "alpha", HubName: "hub", Hub: &adapter.HubConfig{URL: secure.URL, Pin: hubtest.Pin(secure)}, Token: secret}
	if err := s.Validate(context.Background()); err != nil || hits.Load() != 1 {
		t.Fatalf("pinned https: %v, %d requests", err, hits.Load())
	}
}
