package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Personal session start always carries the rule-for-kept-text line, also
// in a checkout with no handoff, no hub and no process: it covers saved
// Markdown, which no write tool's description reaches.
func TestSessionStartCarriesTheWritingRuleLine(t *testing.T) {
	out, errOut, err := runAimemEnv(t, t.TempDir(), t.TempDir(), "", nil, "session-start")
	if err != nil {
		t.Fatalf("session-start: %v %s", err, errOut)
	}
	var hook struct {
		Specific struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &hook); err != nil {
		t.Fatalf("output is not the hook's JSON: %v %q", err, out)
	}
	if hook.Specific.Event != "SessionStart" || strings.TrimLeft(hook.Specific.Context, "\n") != strings.TrimLeft(writingNotice, "\n") {
		t.Fatalf("session start without a handoff: %+v", hook.Specific)
	}
	if !strings.Contains(writingNotice, "writing_rule MCP tool") || !strings.Contains(writingNotice, "Markdown files you save") {
		t.Fatalf("the line no longer names the saved Markdown and the tool: %q", writingNotice)
	}
}
