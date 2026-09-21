package taskcred

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aimem/internal/adapter"
)

func fixture(t *testing.T) (string, string, *httptest.Server, *string) {
	t.Helper()
	root, repo := t.TempDir(), t.TempDir()
	reply := `{"scope":"project","task_write":true}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/access/identity" || r.URL.Query().Get("project") != "alpha" {
			t.Errorf("wrong identity target %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reply))
	}))
	t.Cleanup(ts.Close)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, TaskToken: "global", Token: "checkpoint"}}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".aimem.json"), []byte(`{"project":"alpha","hub":"hub","future":{"keep":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	return root, repo, ts, &reply
}

func token(ch string) string { return "aimem_user_" + strings.Repeat(ch, 64) }

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
				err = adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: "http://elsewhere.invalid", TaskToken: "global"}}, "hub")
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
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received = true }))
			defer target.Close()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
			}))
			defer ts.Close()
			if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, TaskToken: "global"}}, "hub"); err != nil {
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
