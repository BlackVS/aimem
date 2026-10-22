package main

// `aimem skills install` on a member machine: the required skills, at the
// versions the process manifest pins, come from the hub under the
// checkout's credential and land where the clients read them; an instance
// skill overrides the release's of the same name; a pinned earlier version
// comes from a kept import; no Git and nothing but the hub is contacted;
// the bootstrap then reports each skill's version and source.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/hubtest"
	"aimem/internal/process"
	"aimem/internal/processctx"
	"aimem/internal/skills"
	"aimem/internal/taskcred"
)

func TestSkillsInstallFromTheHubWithoutGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no git, no client on PATH
	secret := "aimem_user_" + strings.Repeat("f", 64)
	// The hub's skills: a current release, a kept earlier release and an
	// instance set that overrides one release skill by name.
	cur := map[string][]byte{"oh-code-review/SKILL.md": []byte("review 1.33.13"), "verify-delivery/SKILL.md": []byte("verify 1.33.13")}
	old := map[string][]byte{"oh-code-review/SKILL.md": []byte("review 1.33.12"), "verify-delivery/SKILL.md": []byte("verify 1.33.12")}
	inst := map[string][]byte{"oh-code-review/SKILL.md": []byte("house rules"), "house-style/SKILL.md": []byte("style")}
	sets := map[string]map[string][]byte{}
	entry := func(version string, files map[string][]byte) skills.Entry {
		d := (&skills.Set{Files: files}).Digest()
		sets[d] = files
		return skills.Entry{Version: version, Digest: d, Skills: (&skills.Set{Files: files}).Names()}
	}
	eCur, eOld, eInst := entry("1.33.13", cur), entry("1.33.12", old), entry("site-1", inst)
	index := skills.IndexAnswer{Project: "alpha", Sources: map[string]*skills.Entry{"release": &eCur, "instance": &eInst}, Kept: map[string][]skills.Entry{"release": {eCur, eOld}, "instance": {eInst}}}
	index.Effective = skills.Effective(index.Sources)
	// The process: a version 2 manifest pinning the release's current
	// verify-delivery, the kept release oh-code-review and the instance's
	// house-style.
	manifest := `{"version":2,"handbook":"proc/handbook.md","skills":[{"name":"verify-delivery","version":"1.33.13","source":"release"},{"name":"oh-code-review","version":"1.33.12","source":"release"},{"name":"house-style","version":"site-1","source":"instance"}]}`
	bundle := &process.Bundle{Version: 1, Manifest: "proc/manifest.json", Files: map[string][]byte{"proc/manifest.json": []byte(manifest), "proc/handbook.md": []byte("# Handbook\n")}}
	ref := process.Ref{Repo: "https://127.0.0.1:1/process.git", Commit: strings.Repeat("ab", 20), Manifest: "proc/manifest.json", Digest: bundle.Digest(), Name: "proc"}
	var other []string
	hub := hubtest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/access/identity":
			json.NewEncoder(w).Encode(map[string]any{"scope": "project", "task_write": true, "tasks_enabled": true})
		case "/v1/projects/alpha/process":
			json.NewEncoder(w).Encode(map[string]any{"project": "alpha", "current": ref})
		case "/v1/projects/alpha/process/set":
			json.NewEncoder(w).Encode(map[string]any{"project": "alpha", "selection": map[string]string{"commit": ref.Commit, "manifest": ref.Manifest, "digest": ref.Digest, "name": "proc"}, "set": bundle})
		case "/v1/projects/alpha/skills":
			json.NewEncoder(w).Encode(index)
		case "/v1/projects/alpha/skills/set":
			d := r.URL.Query().Get("digest")
			if files, ok := sets[d]; ok {
				json.NewEncoder(w).Encode(map[string]any{"project": "alpha", "digest": d, "files": files})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			other = append(other, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hub.Close()

	dir, state := t.TempDir(), t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", state)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := adapter.SaveHubs(state, map[string]*adapter.HubConfig{"hub": {URL: hub.URL, Pin: hubtest.Pin(hub), Token: "checkpoint"}}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aimem.json"), []byte(`{"project":"alpha"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := taskcred.Set(context.Background(), dir, state, secret); err != nil {
		t.Fatal(err)
	}

	// Before the install: the bootstrap names each pin as NOT FOUND with
	// the command, and no URL.
	unit, set := processctx.Bootstrap(dir, state, "", true)
	if set == nil || !strings.Contains(unit, "oh-code-review 1.33.12 (release; NOT FOUND, run `aimem skills install`)") || strings.Contains(unit, "127.0.0.1") {
		t.Fatalf("bootstrap before: %s", unit)
	}
	// No client on PATH: the clients must be named.
	if err := skillsInstall(dir, "", false, "", false); err == nil || !strings.Contains(err.Error(), "--tool") {
		t.Fatalf("no client: %v", err)
	}
	if err := skillsInstall(dir, "", false, "emacs", false); err == nil || !strings.Contains(err.Error(), "unknown client") {
		t.Fatalf("unknown client: %v", err)
	}
	text, err := stdoutOf(t, func() error { return skillsInstall(dir, "", false, "claude,codex", false) })
	if err != nil {
		t.Fatalf("install: %v\n%s", err, text)
	}
	for _, want := range []string{
		"installed verify-delivery 1.33.13 (release) for claude: " + filepath.Join(dir, ".claude", "skills", "verify-delivery"),
		"installed oh-code-review 1.33.12 (release) for claude:",
		"installed house-style site-1 (instance) for codex: " + filepath.Join(dir, ".agents", "skills", "house-style"),
		"3 skill(s) into 2 location(s)",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("install output lacks %q:\n%s", want, text)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, ".claude", "skills", "oh-code-review", "SKILL.md")); string(raw) != "review 1.33.12" {
		t.Fatalf("the pinned kept version was not the one installed: %q", raw)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, ".agents", "skills", "house-style", "SKILL.md")); string(raw) != "style" {
		t.Fatalf("the instance skill was not installed: %q", raw)
	}
	if len(other) != 0 {
		t.Fatalf("something besides the hub's routes was asked: %v", other)
	}
	// The cache holds the three sets by digest; the hub's own index did
	// not need a second read of them.
	for d := range sets {
		if !skills.NewStore(state).Has(d) {
			t.Fatalf("set %s not cached", d[:12])
		}
	}
	// After the install: every pin reads installed at its version.
	unit, _ = processctx.Bootstrap(dir, state, "", true)
	for _, want := range []string{"verify-delivery 1.33.13 (release; installed)", "oh-code-review 1.33.12 (release; installed)", "house-style site-1 (instance; installed)"} {
		if !strings.Contains(unit, want) {
			t.Fatalf("bootstrap after lacks %q:\n%s", want, unit)
		}
	}
	// list shows the effective set with the sources and what is here;
	// the effective oh-code-review is the instance's, this machine holds
	// the pinned release one.
	text, err = stdoutOf(t, func() error { return skillsList(dir, "") })
	if err != nil || !strings.Contains(text, "release set: 1.33.13") || !strings.Contains(text, "instance set: site-1") || !strings.Contains(text, "oh-code-review site-1 (instance; installed here at 1.33.12)") || !strings.Contains(text, "house-style site-1 (instance; installed here)") {
		t.Fatalf("list: %v\n%s", err, text)
	}
	// A pin the hub cannot serve (the instance set moved on and the pinned
	// import is no longer kept) is named, and nothing is installed for it.
	eInst.Version = "site-2"
	index.Sources["instance"] = &eInst
	index.Kept["instance"] = []skills.Entry{eInst}
	if err := skillsInstall(dir, "", false, "claude", false); err == nil || !strings.Contains(err.Error(), "house-style site-1 (instance) is not on the hub: the current instance set is site-2") {
		t.Fatalf("unservable pin: %v", err)
	}
	// --all installs every effective skill, the override included, for
	// the user instead of the checkout.
	eInst.Version = "site-1"
	index.Kept["instance"] = []skills.Entry{eInst}
	text, err = stdoutOf(t, func() error { return skillsInstall(dir, "", true, "claude", true) })
	if err != nil || !strings.Contains(text, "installed oh-code-review site-1 (instance) for claude: "+filepath.Join(home, ".claude", "skills", "oh-code-review")) {
		t.Fatalf("install --all --user: %v\n%s", err, text)
	}
	// A revoked credential is a denial, in words, with no URL.
	secret = "aimem_user_" + strings.Repeat("0", 64)
	if err := skillsInstall(dir, "", false, "claude", false); err == nil || strings.Contains(err.Error(), "127.0.0.1") || !strings.Contains(strings.ToLower(err.Error()), "denied") {
		t.Fatalf("revoked: %v", err)
	}
}
