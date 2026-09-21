package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/taskcred"
)

func TestConcurrentProjectCredentialsAndRevocation(t *testing.T) {
	f := newHub(t)
	admin := func(method, path string, body any) []byte {
		t.Helper()
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+f.env)
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("admin setup %s: %d %s", path, w.Code, w.Body)
		}
		return w.Body.Bytes()
	}
	admin("PUT", "/v1/projects/beta/access/user/"+f.aliceID, nil)
	raw := admin("POST", "/v1/access/tokens", map[string]any{"user_id": f.aliceID, "label": "global", "scope": "user", "expires_at": time.Now().Add(time.Hour)})
	var global struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(raw, &global); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(f.h)
	defer ts.Close()
	root, alpha, beta := t.TempDir(), t.TempDir(), t.TempDir()
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"home": {URL: ts.URL, Token: f.env, TaskToken: global.Secret}}, "home"); err != nil {
		t.Fatal(err)
	}
	for dir, project := range map[string]string{alpha: "alpha", beta: "beta"} {
		if err := os.WriteFile(filepath.Join(dir, ".aimem.json"), []byte(`{"project":"`+project+`","hub":"home"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := taskcred.Set(context.Background(), alpha, root, f.alice); err != nil {
		t.Fatal(err)
	}
	a := &srv{project: "alpha", taskSetup: func() (TaskCallFunc, error) { return taskCallerIn(alpha, root) }}
	b := &srv{project: "beta", taskSetup: func() (TaskCallFunc, error) { return taskCallerIn(beta, root) }}
	create := func(s *srv, key, project string) error {
		raw, _ := json.Marshal(map[string]any{"title": "credential check", "idempotency_key": key, "project": project})
		_, err := s.taskTool(context.Background(), "create_task", raw)
		return err
	}
	if err := create(a, "alpha-ok", "alpha"); err != nil {
		t.Fatal(err)
	}
	if err := create(b, "beta-ok", "beta"); err != nil {
		t.Fatal(err)
	}
	if err := create(a, "alpha-no-beta", "beta"); err == nil {
		t.Fatal("local token used global authority")
	}
	// Revoke the local token; the still-valid global token must not be used.
	r := httptest.NewRequest("GET", "/v1/access/identity", nil)
	r.Header.Set("Authorization", "Bearer "+f.alice)
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	var id struct {
		TokenID string `json:"token_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &id); err != nil || id.TokenID == "" {
		t.Fatal("identity missing")
	}
	admin("DELETE", "/v1/access/tokens/"+id.TokenID, nil)
	if err := create(a, "revoked", "alpha"); err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("revoked local credential: %v", err)
	}
	if err := create(b, "beta-still-ok", "beta"); err != nil {
		t.Fatal(err)
	}
	// Removing only the secret must retain the explicit local requirement.
	files, err := filepath.Glob(filepath.Join(root, "task-credentials", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("credential path missing")
	}
	if err := os.Remove(files[0]); err != nil {
		t.Fatal(err)
	}
	if err := create(a, "missing", "alpha"); err == nil {
		t.Fatal("missing secret fell back")
	}
	if err := taskcred.Clear(alpha, root); err != nil {
		t.Fatal(err)
	}
	if err := create(a, "explicit-clear", "alpha"); err != nil {
		t.Fatal(err)
	}
}
