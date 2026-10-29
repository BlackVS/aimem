package main

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/hubtest"
	"aimem/internal/taskcred"
)

func TestTaskTokenCLI(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	ts := hubtest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"scope":"project","task_write":true}`)) }))
	defer ts.Close()
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, Pin: hubtest.Pin(ts), TaskToken: "global"}}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".aimem.json"), []byte(`{"project":"alpha"}`), 0600); err != nil {
		t.Fatal(err)
	}
	secret := "aimem_user_" + strings.Repeat("a", 64)
	var out bytes.Buffer
	if err := runTaskToken([]string{"set"}, repo, root, strings.NewReader(secret+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	if err := runTaskToken([]string{"show-source"}, repo, root, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), secret) || !strings.Contains(out.String(), "project-local") {
		t.Fatal("unsafe/missing source output")
	}
	s, err := taskcred.Resolve(repo, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := runTaskToken([]string{"clear"}, repo, root, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	s, err = taskcred.Resolve(repo, root)
	if err != nil || s.Token != "global" {
		t.Fatal("clear failed")
	}
	for _, args := range [][]string{nil, {"set", secret}, {"bad"}} {
		if err := runTaskToken(args, repo, root, strings.NewReader(""), &out); err == nil {
			t.Fatal("accepted invalid command")
		}
	}
	if err := runTaskToken([]string{"set"}, repo, root, strings.NewReader(strings.Repeat("x", 4097)), &out); err == nil {
		t.Fatal("unbounded token input")
	}
}

// `task-token set --token-file` reads the project token as `hub task-token
// --token-file` does: from an owner-only file, refused with the same
// message otherwise, and never quoting the content; stdin stays the default.
func TestTaskTokenSetFile(t *testing.T) {
	root, repo, dir := t.TempDir(), t.TempDir(), t.TempDir()
	ts := hubtest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"scope":"project","task_write":true}`)) }))
	defer ts.Close()
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, Pin: hubtest.Pin(ts), TaskToken: "global"}}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".aimem.json"), []byte(`{"project":"alpha"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	stored := func() string {
		t.Helper()
		s, err := taskcred.Resolve(repo, root)
		if err != nil {
			t.Fatal(err)
		}
		return s.Token
	}

	secret := "aimem_user_" + strings.Repeat("b", 64)
	file := filepath.Join(dir, "project.token")
	writePrivate(t, file, secret+"\r\n")
	if err := runTaskToken([]string{"set", "--token-file", file}, repo, root, strings.NewReader("unused"), &out); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != secret {
		t.Fatalf("stored %q", got)
	}
	other := "aimem_user_" + strings.Repeat("c", 64)
	file2 := filepath.Join(dir, "project2.token")
	writePrivate(t, file2, other+"\n")
	if err := runTaskToken([]string{"set", "--token-file=" + file2}, repo, root, strings.NewReader(""), &out); err != nil || stored() != other {
		t.Fatalf("--token-file=FILE: %v", err)
	}
	if strings.Contains(out.String(), secret) || strings.Contains(out.String(), other) {
		t.Fatal("the output quotes the token")
	}

	wide := filepath.Join(dir, "wide.token")
	wideSecret := "aimem_user_" + strings.Repeat("w", 64)
	writeWide(t, wide, wideSecret+"\n")
	_, hubErr := readHubToken(wide)
	err := runTaskToken([]string{"set", "--token-file", wide}, repo, root, strings.NewReader(""), &out)
	if err == nil || hubErr == nil || err.Error() != hubErr.Error() || strings.Contains(err.Error(), wideSecret) {
		t.Fatalf("a wide file: %v, want %v", err, hubErr)
	}
	if err := runTaskToken([]string{"set", "--token-file", "-"}, repo, root, strings.NewReader(secret), &out); err == nil || !strings.Contains(err.Error(), "already the default") {
		t.Fatalf("--token-file -: %v", err)
	}
	for _, args := range [][]string{
		{"set", "--token-file"},
		{"set", "--token-file="},
		{"set", "--token-file", file, "extra"},
		{"show-source", "--token-file", file},
		{"set", "--token-file", filepath.Join(dir, "absent.token")},
	} {
		if err := runTaskToken(args, repo, root, strings.NewReader(secret), &out); err == nil {
			t.Fatalf("%q: accepted", args)
		} else if strings.Contains(err.Error(), secret) {
			t.Fatalf("%q: the error quotes the token", args)
		}
	}
	if got := stored(); got != other {
		t.Fatalf("a refused set changed the credential: %q", got)
	}

	// Without the flag, set still reads standard input.
	if err := runTaskToken([]string{"set"}, repo, root, strings.NewReader(secret+"\n"), &out); err != nil || stored() != secret {
		t.Fatalf("stdin: %v", err)
	}
}

func TestProcessBootstrapHonorsLocalRequirement(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	secret := "aimem_user_" + strings.Repeat("d", 64)
	var calls atomic.Int32
	ts := hubtest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("bootstrap used a broader credential")
		}
		w.Write([]byte(`{"scope":"project","task_write":true,"tasks_enabled":false}`))
	}))
	defer ts.Close()
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, Pin: hubtest.Pin(ts), Token: "checkpoint", TaskToken: "global"}}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".aimem.json"), []byte(`{"project":"alpha"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := taskcred.Set(context.Background(), repo, root, secret); err != nil {
		t.Fatal(err)
	}
	if text, _ := processBootstrap(repo, "", false); text != "" {
		t.Fatalf("tasks off: %s", text)
	}
	files, _ := filepath.Glob(filepath.Join(root, "task-credentials", "*.json"))
	if len(files) != 1 {
		t.Fatal("missing credential file")
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	if text, _ := processBootstrap(repo, "", false); !strings.Contains(text, "local task credential required") {
		t.Fatalf("missing override: %s", text)
	}
	if calls.Load() != before {
		t.Fatal("missing override contacted hub")
	}
}

// show-source prints the token's label and expiry: live while the hub
// accepts the credential, as recorded once it refuses it.
func TestTaskTokenShowSourceNamesTheToken(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	var expired atomic.Bool
	var label atomic.Pointer[string]
	first := "laptop-alpha"
	label.Store(&first)
	ts := hubtest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if expired.Load() {
			http.Error(w, "bearer authentication required", http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"scope":"project","task_write":true,"label":"` + *label.Load() + `","expires_at":"2026-11-30T00:00:00Z"}`))
	}))
	defer ts.Close()
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, Pin: hubtest.Pin(ts)}}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".aimem.json"), []byte(`{"project":"alpha"}`), 0600); err != nil {
		t.Fatal(err)
	}
	secret := "aimem_user_" + strings.Repeat("c", 64)
	var out bytes.Buffer
	if err := runTaskToken([]string{"set"}, repo, root, strings.NewReader(secret+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	show := func() string {
		t.Helper()
		out.Reset()
		if err := runTaskToken([]string{"show-source"}, repo, root, strings.NewReader(""), &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), secret) {
			t.Fatalf("show-source prints the secret: %s", out.String())
		}
		return out.String()
	}
	// Live: the hub's answer wins over the record.
	renamed := "laptop-alpha-renamed"
	label.Store(&renamed)
	if got := show(); !strings.Contains(got, `"token_meta":{"label":"laptop-alpha-renamed","scope":"project","expires_at":"2026-11-30T00:00:00Z"}`) || !strings.Contains(got, `"state":"active"`) {
		t.Fatalf("active: %s", got)
	}
	// Refused: the record, and the refusal naming the token.
	expired.Store(true)
	if got := show(); !strings.Contains(got, `"label":"laptop-alpha"`) || !strings.Contains(got, `"state":"refused"`) || !strings.Contains(got, "aimem task-token set --token-file FILE") {
		t.Fatalf("refused: %s", got)
	}
}
