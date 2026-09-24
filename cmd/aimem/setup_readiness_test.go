package main

import (
	"bufio"
	"encoding/json"
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
