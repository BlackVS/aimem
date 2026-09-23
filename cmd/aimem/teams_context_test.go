package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aimem/internal/teamguide"
)

// TestHelperMain runs the real aimem main in a child process when asked to;
// the tests below drive it as the installed binary would be driven.
func TestHelperMain(t *testing.T) {
	if os.Getenv("AIMEM_TEST_MAIN") != "1" {
		t.Skip("helper process for the team_context end-to-end tests")
	}
	os.Args = append([]string{"aimem"}, strings.Split(os.Getenv("AIMEM_TEST_ARGS"), "\x1f")...)
	main()
	os.Exit(0)
}

// runAimem runs `aimem args...` in dir with an empty home and state root:
// no checkout, no hub, no credential, no saved membership.
func runAimem(t *testing.T, dir, state, stdin string, args ...string) (string, string, error) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperMain$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AIMEM_TEST_MAIN=1", "AIMEM_TEST_ARGS="+strings.Join(args, "\x1f"),
		"AIMEM_STATE_DIR="+state, "XDG_STATE_HOME="+state, "HOME="+home, "USERPROFILE="+home,
		"AIMEM_HUB_URL=", "AIMEM_HUB_TOKEN=")
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), errOut.String(), err
	case <-time.After(60 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("aimem %v did not finish; stderr: %s", args, errOut.String())
	}
	return "", "", nil
}

// entries lists what a directory holds, recursively, relative to it.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != dir {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	return out
}

func TestTeamContextThroughLocalMCPOutsideAnyCheckout(t *testing.T) {
	u, err := teamguide.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	dir, state := t.TempDir(), t.TempDir()
	calls := []any{
		map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}},
		map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
		map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "team_context", "arguments": map[string]any{"role": "worker"}}},
		map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": map[string]any{"name": "team_context", "arguments": map[string]any{"section": "example/submit"}}},
		map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": map[string]any{"name": "team_context", "arguments": map[string]any{"section": "../TEAM-PLAYBOOKS.md"}}},
		map[string]any{"jsonrpc": "2.0", "id": 6, "method": "tools/call", "params": map[string]any{"name": "team_context", "arguments": map[string]any{"path": "docs/TEAM-PLAYBOOKS.md"}}},
		map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": "team_context", "arguments": map[string]any{"role": "worker", "section": "worker"}}},
		map[string]any{"jsonrpc": "2.0", "id": 8, "method": "tools/call", "params": map[string]any{"name": "team_context", "arguments": map[string]any{}}},
	}
	var in strings.Builder
	for _, c := range calls {
		b, _ := json.Marshal(c)
		in.Write(append(b, '\n'))
	}
	stdout, stderr, err := runAimem(t, dir, state, in.String(), "mcp")
	if err != nil {
		t.Fatalf("aimem mcp: %v\n%s", err, stderr)
	}
	results := map[float64]map[string]any{}
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var msg struct {
			ID     float64        `json:"id"`
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			t.Fatalf("not JSON-RPC: %q", sc.Text())
		}
		results[msg.ID] = msg.Result
	}
	text := func(id float64) (string, bool) {
		content, _ := results[id]["content"].([]any)
		if len(content) == 0 {
			t.Fatalf("call %v: no content: %v", id, results[id])
		}
		isErr, _ := results[id]["isError"].(bool)
		return content[0].(map[string]any)["text"].(string), isErr
	}
	listed := false
	for _, tl := range results[2]["tools"].([]any) {
		listed = listed || tl.(map[string]any)["name"] == "team_context"
	}
	if !listed {
		t.Fatalf("team_context not listed outside a checkout: %v", results[2])
	}
	role, isErr := text(3)
	if isErr || !strings.HasSuffix(role, u.Terminator("role worker", "dev")+"\n") || !strings.Contains(role, u.Digest) {
		t.Fatalf("role read: %v %q", isErr, role[max(0, len(role)-300):])
	}
	submit, _ := u.Section("example/submit")
	if sec, isErr := text(4); isErr || !strings.Contains(sec, string(submit)) {
		t.Fatalf("section read: %v %s", isErr, sec)
	}
	for id, want := range map[float64]string{5: "section must be a section id", 6: `unknown field "path"`, 7: "not both"} {
		if msg, isErr := text(id); !isErr || !strings.Contains(msg, want) {
			t.Errorf("call %v: want an error with %q, got %v %q", id, want, isErr, msg)
		}
	}
	if idx, isErr := text(8); isErr || !strings.Contains(idx, `"digest": "`+u.Digest+`"`) {
		t.Errorf("index: %v %s", isErr, idx)
	}
	// Reading changed nothing: no file in the directory, no saved membership
	// or credential in the state root.
	if got := entries(t, dir); len(got) != 0 {
		t.Errorf("the working directory gained %v", got)
	}
	for _, e := range entries(t, state) {
		if strings.HasPrefix(e, "team-sessions") || strings.HasPrefix(e, "task-credentials") {
			t.Errorf("state root gained %s", e)
		}
	}
}

func TestTeamsContextCLI(t *testing.T) {
	u, err := teamguide.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	dir, state := t.TempDir(), t.TempDir()
	out, stderr, err := runAimem(t, dir, state, "", "teams", "context", "--role", "coordinator")
	if err != nil || !strings.HasSuffix(out, u.Terminator("role coordinator", "dev")+"\n") {
		t.Fatalf("role: %v %s %q", err, stderr, out[max(0, len(out)-200):])
	}
	offer, _ := u.Section("example/offer")
	if !strings.Contains(out, string(offer)) {
		t.Error("the coordinator set lacks the offer template it links")
	}
	out, _, err = runAimem(t, dir, state, "", "teams", "context")
	var idx struct {
		Digest   string             `json:"digest"`
		Manifest teamguide.Manifest `json:"manifest"`
	}
	if err != nil || json.Unmarshal([]byte(out), &idx) != nil || idx.Digest != u.Digest || len(idx.Manifest.Sections) != len(u.Manifest.Sections) {
		t.Fatalf("index: %v %s", err, out)
	}
	for _, args := range [][]string{
		{"teams", "context", "--section", "../TEAM-PLAYBOOKS.md"},
		{"teams", "context", "--role", "admin"},
		{"teams", "context", "--role", "worker", "--section", "worker"},
		{"teams", "context", "--path", "x"},
		{"teams", "context", "extra"},
	} {
		if _, stderr, err := runAimem(t, dir, state, "", args...); err == nil || !strings.Contains(stderr, "aimem:") {
			t.Errorf("%v: want a refusal, got %v %s", args, err, stderr)
		}
	}
	if got := entries(t, dir); len(got) != 0 {
		t.Errorf("the working directory gained %v", got)
	}
}
