package mcp

// The tool inventory (the tool set design, docs/DESIGN-tool-sets.md): what
// each configuration of the facade lists, read from its own tools/list
// answer, and each tool's group and size. Every run checks that each listed
// tool has a group; with AIMEM_TOOL_INVENTORY=1 it also prints the figures:
//
//	AIMEM_TOOL_INVENTORY=1 go test ./internal/mcp/ -run TestToolInventory -v

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

// toolGroup names the group a tool belongs to in the inventory. A tool
// missing here fails TestToolInventory: a new tool is classified when it
// is added.
func toolGroup(name string) string {
	switch {
	case strings.HasPrefix(name, "task_reservation_"):
		return "reservation"
	case name == "team_setup" || name == "team_continue":
		return "onboarding"
	case name == "team_context" || name == "writing_rule":
		return "guidance"
	case name == "process_context":
		return "process"
	case name == "write_report" || name == "session_context":
		return "team-mode"
	case strings.HasPrefix(name, "team_"):
		return "team"
	}
	switch name {
	case "recall_memory", "remember", "forget_memory", "review_memories", "confirm_memory", "search_journal",
		"get_design_doc", "list_docs", "read_doc", "update_doc", "list_records", "get_record", "put_record":
		return "knowledge"
	case "list_tasks", "search_tasks", "get_task", "create_task", "update_task", "project_repository", "triage_task",
		"get_task_history", "list_task_comments", "get_task_comment", "add_task_comment",
		"list_epics", "get_epic", "create_epic", "update_epic",
		"set_feature", "confirm_feature_task", "list_features":
		return "tasks"
	}
	return ""
}

// inventoryConfig is one configuration of the facade, as tools/list
// (mcp.go) branches on it.
type inventoryConfig struct {
	name string
	srv  func(t *testing.T) *srv
}

var inventoryConfigs = []inventoryConfig{
	{"personal, checkout-bound", func(t *testing.T) *srv {
		return &srv{taskState: taskStateEnabled, local: &localCheckout{dir: t.TempDir(), root: t.TempDir()}}
	}},
	{"personal, no checkout", func(*testing.T) *srv { return &srv{taskState: taskStateEnabled} }},
	{"personal, tasks disabled", func(*testing.T) *srv { return &srv{taskState: taskStateDisabled} }},
	{"tasks-only", func(*testing.T) *srv { return &srv{tasksOnly: true} }},
	{"team member", func(*testing.T) *srv { return &srv{team: &teamMode{}} }},
}

// listedTools is a configuration's tools/list answer: each definition as
// it is sent, in order.
func listedTools(t *testing.T, s *srv) []json.RawMessage {
	t.Helper()
	var out struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(s.handle(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)), &out); err != nil {
		t.Fatal(err)
	}
	return out.Result.Tools
}

func TestToolInventory(t *testing.T) {
	type tool struct {
		group   string
		bytes   int
		configs []string
	}
	tools := map[string]*tool{}
	var summary []string
	for _, c := range inventoryConfigs {
		defs := listedTools(t, c.srv(t))
		total := 0
		for _, raw := range defs {
			var d struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &d); err != nil {
				t.Fatal(err)
			}
			g := toolGroup(d.Name)
			if g == "" {
				t.Errorf("%s: listed by %q but in no group: classify it in toolGroup", d.Name, c.name)
			}
			if tools[d.Name] == nil {
				tools[d.Name] = &tool{group: g, bytes: len(raw)}
			}
			// A facade may serve its own variant of a tool (the scoped
			// knowledge reads): its size is named beside the configuration.
			listed := c.name
			if len(raw) != tools[d.Name].bytes {
				listed = fmt.Sprintf("%s (%d)", c.name, len(raw))
			}
			tools[d.Name].configs = append(tools[d.Name].configs, listed)
			total += len(raw)
		}
		summary = append(summary, fmt.Sprintf("| %s | %d | %d |", c.name, len(defs), total))
	}
	for _, n := range ToolNames() {
		if tools[n] == nil {
			t.Errorf("%s: in ToolNames but listed by no configuration of the inventory", n)
		}
	}
	if os.Getenv("AIMEM_TOOL_INVENTORY") == "" {
		return
	}

	fmt.Println("| configuration | tools | bytes |")
	fmt.Println("| --- | ---: | ---: |")
	fmt.Println(strings.Join(summary, "\n"))

	groups := map[string][2]int{}
	names := make([]string, 0, len(tools))
	for n, tl := range tools {
		names = append(names, n)
		g := groups[tl.group]
		groups[tl.group] = [2]int{g[0] + 1, g[1] + tl.bytes}
	}
	fmt.Println("\n| group | tools | bytes |")
	fmt.Println("| --- | ---: | ---: |")
	gs := make([]string, 0, len(groups))
	for g := range groups {
		gs = append(gs, g)
	}
	slices.Sort(gs)
	for _, g := range gs {
		fmt.Printf("| %s | %d | %d |\n", g, groups[g][0], groups[g][1])
	}

	slices.SortFunc(names, func(a, b string) int {
		if c := strings.Compare(tools[a].group, tools[b].group); c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})
	fmt.Println("\n| tool | group | bytes | listed by |")
	fmt.Println("| --- | --- | ---: | --- |")
	for _, n := range names {
		fmt.Printf("| `%s` | %s | %d | %s |\n", n, tools[n].group, tools[n].bytes, strings.Join(tools[n].configs, "; "))
	}
}
