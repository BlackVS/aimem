package mcp

import (
	"slices"
	"testing"
)

// TestToolNamesCoverEveryFacade pins ToolNames to what the facades really
// list: a tool added to any of them must reach the documentation check.
func TestToolNamesCoverEveryFacade(t *testing.T) {
	names := ToolNames()
	if !slices.IsSorted(names) || len(slices.Compact(slices.Clone(names))) != len(names) {
		t.Fatalf("ToolNames is not sorted and unique: %v", names)
	}
	for _, defs := range [][]map[string]any{toolDefs, taskToolDefs, scopedKnowledgeToolDefs, onboardToolDefs, processToolDefs, guidanceToolDefs, teamToolList()} {
		for _, d := range defs {
			if n := d["name"].(string); !slices.Contains(names, n) {
				t.Errorf("tool %q is served but missing from ToolNames", n)
			}
		}
	}
	for _, n := range []string{"recall_memory", "create_task", "team_setup", "task_reservation_claim", "writing_rule"} {
		if !slices.Contains(names, n) {
			t.Errorf("ToolNames lacks %q", n)
		}
	}
}
