package mcp

import (
	"strings"
	"testing"
)

// Every task and epic read tool states the read as it is: an ordinary token
// reads the projects its user holds a grant on, and a refusal reads as not
// found, so an agent knows a missing project or task may be a missing grant.
func TestTaskReadToolsStateTheGrantedRead(t *testing.T) {
	reads := map[string]bool{
		"list_tasks": true, "search_tasks": true, "get_task": true, "get_task_history": true,
		"list_task_comments": true, "get_task_comment": true,
		"list_epics": true, "get_epic": true,
	}
	for _, td := range taskToolDefs {
		name, _ := td["name"].(string)
		if !reads[name] {
			continue
		}
		delete(reads, name)
		desc, _ := td["description"].(string)
		if !strings.Contains(desc, "reads the projects its user holds a grant on") || !strings.Contains(desc, "without a grant answers as not found") || !strings.Contains(desc, "team's granted projects") {
			t.Errorf("%s does not state the granted read: %q", name, desc)
		}
		if p, ok := td["inputSchema"].(map[string]any)["properties"].(map[string]any)["project"].(map[string]any); ok {
			if d, _ := p["description"].(string); !strings.Contains(d, "it is not a limit") {
				t.Errorf("%s: the project argument reads as a limit: %q", name, d)
			}
		}
	}
	for name := range reads {
		t.Errorf("read tool %s is missing from taskToolDefs", name)
	}
}
