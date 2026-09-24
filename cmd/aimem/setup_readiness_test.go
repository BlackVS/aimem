package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/teamsetup/teamsetuptest"
)

// mcpCall runs one tools/call through a fresh `aimem mcp` process (the
// client's restart between calls is a new process) and returns its text
// blocks.
func mcpCall(t *testing.T, dir, root, name string, args map[string]any) []string {
	t.Helper()
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	stdout, stderr, err := runAimem(t, dir, root, string(req)+"\n", "mcp")
	if err != nil {
		t.Fatalf("aimem mcp: %v\n%s", err, stderr)
	}
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 1<<20), 4<<20)
	var msg struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &msg) != nil || msg.Result.IsError {
		t.Fatalf("%s: %s", name, stdout)
	}
	var blocks []string
	for _, c := range msg.Result.Content {
		blocks = append(blocks, c.Text)
	}
	return blocks
}

// team_setup and then team_continue, each through the real `aimem mcp`
// process started in a bound checkout that is not the aimem repository:
// the report, then the role guidance and the project process as their own
// blocks, ready for work, one membership across the restart.
func TestSetupAndContinueDeliverContextThroughTheRealLocalMCP(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	repo, root := teamsetuptest.Checkout(t, h, ts)
	for i, call := range []struct {
		name string
		args map[string]any
	}{
		{"team_setup", map[string]any{"team": "Pilot", "role": "worker", "profile": map[string]any{"label": "w", "platform": "codex", "platform_version": "1"}}},
		{"team_continue", map[string]any{}},
	} {
		blocks := mcpCall(t, repo, root, call.name, call.args)
		var rep struct {
			Status    string `json:"status"`
			Readiness struct {
				ReadyForWork bool `json:"ready_for_work"`
				Execution    struct {
					State string `json:"state"`
				} `json:"execution"`
			} `json:"readiness"`
		}
		if len(blocks) != 3 || json.Unmarshal([]byte(blocks[0]), &rep) != nil || rep.Status != "joined" || !rep.Readiness.ReadyForWork || rep.Readiness.Execution.State != "not_verified" {
			t.Fatalf("%s: %d blocks\n%.600s", call.name, len(blocks), blocks[0])
		}
		if !strings.HasPrefix(lastLine(blocks[1]), "=== end aimem-team-guidance role worker") || !strings.HasPrefix(lastLine(blocks[2]), "=== end aimem process context unit project alpha") {
			t.Fatalf("%s: terminators %q / %q", call.name, lastLine(blocks[1]), lastLine(blocks[2]))
		}
		if h.Count("/join") != 1 {
			t.Fatalf("call %d: %d joins", i, h.Count("/join"))
		}
	}
}

// `aimem teams accept` from the checkout records the process version the
// attempt is accepted under, like the checkout-bound MCP tool.
func TestCLIAcceptRecordsTheVersion(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	h.Reserved = map[string]any{"id": "att-1", "task_id": "task-1", "state": "OFFERED"}
	repo, root := teamsetuptest.Checkout(t, h, ts)
	if out, err := runSetup(t, repo, root, "Pilot", "worker", "--platform", "codex"); err != nil {
		t.Fatal(err, out)
	}
	req := filepath.Join(t.TempDir(), "accept.json")
	if err := os.WriteFile(req, []byte(`{"session_id":"sess-1","generation":1,"attempt":"att-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := runAimem(t, repo, root, "", "teams", "accept", "alpha", "team-1", req, "cli-accept-1")
	if err != nil || !strings.Contains(out, "process version is recorded on this checkout: commit "+teamsetuptest.Selection.Commit) {
		t.Fatalf("%v\n%s\n%s", err, out, stderr)
	}
	st := readState(t, root, repo)
	if st.Accepted == nil || st.Accepted.Attempt != "att-1" || st.Accepted.Commit != teamsetuptest.Selection.Commit || !st.Accepted.Confirmed || h.Reserved["state"] != "RUNNING" {
		t.Fatalf("record %+v, reserved %v", st.Accepted, h.Reserved)
	}
}
