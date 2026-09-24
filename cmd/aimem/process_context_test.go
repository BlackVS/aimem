package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/process"
	"aimem/internal/taskcred"
)

// process_context through the real `aimem mcp` stdio server, started in a
// checkout that is not the aimem repository and bound to a hub with a
// project-local credential: the complete unit and a template arrive with
// their terminators, every argument other than a template kind is refused,
// and the checkout is left as it was.
func TestProcessContextThroughLocalMCPInABoundCheckout(t *testing.T) {
	secret := "aimem_user_" + strings.Repeat("e", 64)
	ref := process.Ref{Repo: "https://127.0.0.1:1/process.git", Commit: strings.Repeat("cd", 20), Manifest: "proc/manifest.json"}
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/access/identity":
			json.NewEncoder(w).Encode(map[string]any{"scope": "project", "task_write": true, "tasks_enabled": true})
		case "/v1/projects/alpha/process":
			json.NewEncoder(w).Encode(map[string]any{"project": "alpha", "current": ref})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer hub.Close()

	dir, state := t.TempDir(), t.TempDir()
	if err := adapter.SaveHubs(state, map[string]*adapter.HubConfig{"hub": {URL: hub.URL, Token: "checkpoint"}}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aimem.json"), []byte(`{"project":"alpha"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := taskcred.Set(context.Background(), dir, state, secret); err != nil {
		t.Fatal(err)
	}
	// This machine already holds the exact selected commit, as a finished
	// fetch leaves it; the selection's address is unreachable, so nothing
	// but that cache can answer.
	cacheDir := process.CacheDir(state, ref)
	for p, body := range map[string]string{
		ref.Manifest:       `{"version":1,"handbook":"proc/handbook.md","templates":{"task":"proc/task.json"}}`,
		"proc/handbook.md": "# Handbook\n\nOnly READY tasks are picked up.\n",
		"proc/task.json":   `{"title":"","objective":""}`,
		".complete":        ref.Manifest + "\n",
	} {
		fp := filepath.Join(cacheDir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(fp), 0o700)
		if err := os.WriteFile(fp, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := entries(t, dir)

	calls := []map[string]any{
		{"name": "process_context", "arguments": map[string]any{}},
		{"name": "process_context", "arguments": map[string]any{"template": "task"}},
		{"name": "process_context", "arguments": map[string]any{"template": "../proc/task.json"}},
		{"name": "process_context", "arguments": map[string]any{"path": "proc/handbook.md"}},
		{"name": "process_context", "arguments": map[string]any{"commit": ref.Commit}},
		{"name": "process_context", "arguments": map[string]any{"project": "beta"}},
	}
	var in strings.Builder
	in.WriteString(`{"jsonrpc":"2.0","id":0,"method":"tools/list"}` + "\n")
	for i, c := range calls {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": "tools/call", "params": c})
		in.Write(append(b, '\n'))
	}
	stdout, stderr, err := runAimem(t, dir, state, in.String(), "mcp")
	if err != nil {
		t.Fatalf("aimem mcp: %v\n%s", err, stderr)
	}
	results := map[int]map[string]any{}
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var msg struct {
			ID     int            `json:"id"`
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			t.Fatalf("not JSON-RPC: %q", sc.Text())
		}
		results[msg.ID] = msg.Result
	}
	var names []string
	for _, tl := range results[0]["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if !slices.Contains(names, "process_context") {
		t.Fatalf("process_context not listed in a bound checkout: %v", names)
	}
	text := func(id int) (string, bool) {
		content, _ := results[id]["content"].([]any)
		if len(content) == 0 {
			t.Fatalf("call %d: no content: %v", id, results[id])
		}
		isErr, _ := results[id]["isError"].(bool)
		return content[0].(map[string]any)["text"].(string), isErr
	}
	unit, isErr := text(1)
	if isErr || !strings.Contains(unit, "Only READY tasks are picked up.") || !strings.Contains(unit, "state ready") ||
		!strings.Contains(lastLine(unit), "=== end aimem process context unit project alpha commit "+ref.Commit) {
		t.Fatalf("unit: %v\n%s", isErr, unit)
	}
	tmpl, isErr := text(2)
	if isErr || !strings.Contains(tmpl, `{"title":"","objective":""}`) || !strings.HasPrefix(lastLine(tmpl), "=== end aimem process context template task project alpha") {
		t.Fatalf("template: %v\n%s", isErr, tmpl)
	}
	for id, want := range map[int]string{3: "kinds: task", 4: `unknown field "path"`, 5: `unknown field "commit"`, 6: `unknown field "project"`} {
		if msg, isErr := text(id); !isErr || !strings.Contains(msg, want) {
			t.Errorf("call %d: want an error with %q, got %v %q", id, want, isErr, msg)
		}
	}
	if after := entries(t, dir); !slices.Equal(before, after) {
		t.Errorf("the checkout changed: before %v, after %v", before, after)
	}
	for _, e := range entries(t, state) {
		if strings.HasPrefix(e, "team-sessions") {
			t.Errorf("state root gained %s", e)
		}
	}
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n")
	return s[strings.LastIndexByte(s, '\n')+1:]
}
