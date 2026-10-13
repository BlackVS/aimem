package main

import (
	"encoding/json"
	"path/filepath"
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
	if hook.Specific.Event != "SessionStart" || strings.TrimLeft(hook.Specific.Context, "\n") != strings.TrimLeft(writingNotice+environmentNotice, "\n") {
		t.Fatalf("session start without a handoff: %+v", hook.Specific)
	}
	if !strings.Contains(writingNotice, "writing_rule MCP tool") || !strings.Contains(writingNotice, "Markdown files you save") {
		t.Fatalf("the line no longer names the saved Markdown and the tool: %q", writingNotice)
	}
}

// Personal session start tells the agent to recall aimem before it reports
// an environment blocker; a team conversation, where recall is off, does
// not get the line.
func TestSessionStartSendsEnvironmentBlockersToRecall(t *testing.T) {
	for _, want := range []string{"credentials, SSH, hardware keys, network, missing tools", "recall aimem with the error's text",
		"recall_memory MCP tool", "`aimem recall -q`", "apply what it finds"} {
		if !strings.Contains(environmentNotice, want) {
			t.Errorf("the line lacks %q: %q", want, environmentNotice)
		}
	}
	context := func(env ...string) string {
		t.Helper()
		out, errOut, err := runAimemEnv(t, t.TempDir(), t.TempDir(), "", env, "session-start")
		if err != nil {
			t.Fatalf("session-start: %v %s", err, errOut)
		}
		var hook struct {
			Specific struct {
				Context string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(out), &hook); err != nil {
			t.Fatalf("output is not the hook's JSON: %v %q", err, out)
		}
		return hook.Specific.Context
	}
	if ctx := context(); !strings.Contains(ctx, strings.TrimLeft(environmentNotice, "\n")) {
		t.Errorf("personal session start lacks the line:\n%s", ctx)
	}
	if ctx := context("AIMEM_TEAM_SESSION=" + filepath.Join(t.TempDir(), "team.json")); strings.Contains(ctx, "recall aimem with the error") {
		t.Errorf("a team conversation got the line:\n%s", ctx)
	}
}
