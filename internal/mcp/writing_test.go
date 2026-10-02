package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// keptTextExempt are the listed tools that store no free text, each with
// the reason. Every listed tool is either here or in keptTextTools.
var keptTextExempt = map[string]string{
	// reads
	"recall_memory": "read", "review_memories": "read", "search_journal": "read", "get_design_doc": "read",
	"list_docs": "read", "read_doc": "read", "list_records": "read", "get_record": "read",
	"list_tasks": "read", "get_task": "read", "get_task_history": "read", "list_task_comments": "read",
	"get_task_comment": "read", "list_epics": "read", "get_epic": "read", "team_members": "read",
	"team_messages": "read", "team_inbox": "read", "team_list": "read", "team_assignment": "read",
	"team_reserved": "read", "task_reservation_status": "read", "task_reservation_receipt": "read",
	"process_context": "read", "team_context": "read", writingTool: "read", sessionContextTool: "read",
	// writes without free text
	"forget_memory": "an id only", "confirm_memory": "an id only",
	"team_heartbeat": "availability state", "team_resume": "a session step", "team_leave": "a session step",
	"team_ack": "message ids", "team_accept": "an attempt id",
	"team_join": "a structured profile (label, platform, model)", "team_profile": "a structured profile (label, platform, model)",
	"team_setup": "local onboarding; stores no prose", "team_continue": "local onboarding; stores no prose",
}

// Every tool any facade lists is classified, and every write tool that
// stores free text ends with the fixed sentence pointing to writing_rule.
func TestEveryWriteToolCarriesTheKeptTextNote(t *testing.T) {
	all := [][]map[string]any{toolDefs, taskToolDefs, scopedKnowledgeToolDefs, onboardToolDefs, processToolDefs, guidanceToolDefs, teamToolList()}
	seen := map[string]bool{}
	for _, defs := range all {
		for _, d := range defs {
			name := d["name"].(string)
			seen[name] = true
			desc := d["description"].(string)
			_, exempt := keptTextExempt[name]
			switch {
			case keptTextTools[name] && exempt:
				t.Errorf("%s is both a kept-text tool and exempt", name)
			case keptTextTools[name]:
				if !strings.HasSuffix(desc, " "+keptTextNote) || strings.Count(desc, keptTextNote) != 1 {
					t.Errorf("%s: the description does not end with the kept-text sentence exactly once", name)
				}
			case exempt:
				if strings.Contains(desc, keptTextNote) {
					t.Errorf("%s is exempt but carries the kept-text sentence", name)
				}
			default:
				t.Errorf("tool %s is not classified: add it to keptTextTools (it stores free text) or to keptTextExempt with the reason", name)
			}
		}
	}
	for name := range keptTextTools {
		if !seen[name] {
			t.Errorf("keptTextTools names %s, which no facade lists", name)
		}
	}
	for name := range keptTextExempt {
		if !seen[name] {
			t.Errorf("keptTextExempt names %s, which no facade lists", name)
		}
	}
}

// What a client receives: tools/list on the personal facade and on the
// hub's facade carries the sentence on a write tool and lists writing_rule,
// and writing_rule answers with the whole rule.
func TestFacadesListTheWritingRule(t *testing.T) {
	f := newHub(t)
	var personal struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal((&srv{project: "alpha"}).handle(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)), &personal); err != nil {
		t.Fatal(err)
	}
	for label, result := range map[string]map[string]any{
		"personal": personal.Result,
		"hub":      f.rpc(t, f.alice, "tools/list", map[string]any{}),
	} {
		var rule, comment bool
		tools, _ := result["tools"].([]any)
		for _, raw := range tools {
			tl := raw.(map[string]any)
			name, desc := tl["name"].(string), tl["description"].(string)
			rule = rule || name == writingTool
			comment = comment || (name == "add_task_comment" && strings.HasSuffix(desc, keptTextNote))
		}
		if !rule || !comment {
			t.Fatalf("%s facade: writing_rule listed %v, add_task_comment carries the sentence %v", label, rule, comment)
		}
	}
	text, isErr := toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": writingTool, "arguments": map[string]any{}}))
	if isErr || !strings.Contains(text, "# Writing text that is kept") || !strings.Contains(text, "=== end aimem-writing-rule") {
		t.Fatalf("writing_rule on the hub facade: %v %.300s", isErr, text)
	}
	if text, isErr := toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": writingTool, "arguments": map[string]any{"x": 1}})); !isErr || !strings.Contains(text, "takes no arguments") {
		t.Fatalf("writing_rule accepted an argument: %v %s", isErr, text)
	}
}
