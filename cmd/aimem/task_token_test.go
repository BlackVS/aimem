package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/taskcred"
)

func TestTaskTokenCLI(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"scope":"project","task_write":true}`)) }))
	defer ts.Close()
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, TaskToken: "global"}}, "hub"); err != nil {
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

func TestProcessBootstrapHonorsLocalRequirement(t *testing.T) {
	root, repo := t.TempDir(), t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	secret := "aimem_user_" + strings.Repeat("d", 64)
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("bootstrap used a broader credential")
		}
		w.Write([]byte(`{"scope":"project","task_write":true,"tasks_enabled":false}`))
	}))
	defer ts.Close()
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, Token: "checkpoint", TaskToken: "global"}}, "hub"); err != nil {
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
