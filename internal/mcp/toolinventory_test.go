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
	"maps"
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
	{"standalone", func(t *testing.T) *srv {
		return &srv{taskState: taskStateEnabled, local: &localCheckout{dir: t.TempDir(), root: t.TempDir()}}
	}},
	{"teams", func(t *testing.T) *srv {
		dir, root := joinedCheckout(t)
		return &srv{taskState: taskStateEnabled, local: &localCheckout{dir: dir, root: root}}
	}},
	{"personal, no checkout", func(*testing.T) *srv { return &srv{taskState: taskStateEnabled} }},
	{"personal, tasks disabled", func(*testing.T) *srv { return &srv{taskState: taskStateDisabled} }},
	{"tasks-only", func(*testing.T) *srv { return &srv{tasksOnly: true} }},
	{"tasks-only, tools=teams", func(*testing.T) *srv { return &srv{tasksOnly: true, teamsByURL: true} }},
	{"team member", func(*testing.T) *srv { return &srv{team: &teamMode{}} }},
}

// standaloneByteLimit is half of what a personal, checkout-bound session
// listed when the design measured it (97,069 bytes): the design's first test.
const standaloneByteLimit = 48534

// wantSets are the sets of docs/DESIGN-tool-sets.md §4 by name, built from
// the groups and the named tools, so a tool added to a group joins its set.
func wantSets() map[string]map[string]bool {
	set := func(groups []string, names ...string) map[string]bool {
		out := map[string]bool{}
		for _, n := range ToolNames() {
			if slices.Contains(groups, toolGroup(n)) {
				out[n] = true
			}
		}
		for _, n := range names {
			out[n] = true
		}
		return out
	}
	with := func(base map[string]bool, names ...string) map[string]bool {
		out := maps.Clone(base)
		for _, n := range names {
			out[n] = true
		}
		return out
	}
	entry := []string{"team_setup", "team_continue", "team_context", "team_list"}
	standalone := set([]string{"knowledge", "tasks"}, append(entry, "writing_rule", "process_context")...)
	tasksOnly := set([]string{"tasks"}, "recall_memory", "list_docs", "read_doc", "writing_rule", "team_context", "team_list",
		"task_reservation_status", "task_reservation_release", "task_reservation_finalize")
	work := teamWorkToolNames()
	return map[string]map[string]bool{
		"standalone":              standalone,
		"teams":                   with(standalone, work...),
		"tasks-only":              tasksOnly,
		"tasks-only, tools=teams": with(tasksOnly, work...),
		"team member": {"list_tasks": true, "search_tasks": true, "get_task": true, "get_task_history": true, "list_task_comments": true,
			"get_task_comment": true, "add_task_comment": true, "triage_task": true, "project_repository": true, "list_epics": true,
			"get_epic": true, "list_features": true, "recall_memory": true, "list_docs": true, "read_doc": true,
			"writing_rule": true, "session_context": true, "write_report": true},
	}
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
	want := wantSets()
	for _, c := range inventoryConfigs {
		defs := listedTools(t, c.srv(t))
		total := 0
		got := map[string]bool{}
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
			got[d.Name] = true
		}
		if w, ok := want[c.name]; ok && !maps.Equal(got, w) {
			for n := range w {
				if !got[n] {
					t.Errorf("%s: %s missing from the list", c.name, n)
				}
			}
			for n := range got {
				if !w[n] {
					t.Errorf("%s: %s listed, not in the set", c.name, n)
				}
			}
		}
		if c.name == "standalone" && total > standaloneByteLimit {
			t.Errorf("standalone lists %d bytes, more than %d", total, standaloneByteLimit)
		}
		summary = append(summary, fmt.Sprintf("| %s | %d | %d |", c.name, len(defs), total))
	}
	// A tool no configuration lists is a reservation tool, served by name
	// only (§5.2); any other is listed somewhere.
	for _, n := range ToolNames() {
		if tools[n] == nil && !reservationTools[n] {
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
