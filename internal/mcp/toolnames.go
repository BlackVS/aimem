package mcp

import "sort"

// ToolNames lists, sorted and without duplicates, every tool name any of
// this binary's MCP facades can serve: the local stdio facade in each of
// its task states, the tasks-only facade and the team-mode facade. The
// documentation check compares the docs' tool names against it.
func ToolNames() []string {
	seen := map[string]bool{}
	for _, defs := range [][]map[string]any{toolDefs, taskToolDefs, scopedKnowledgeToolDefs, onboardToolDefs, processToolDefs, guidanceToolDefs, teamToolList()} {
		for _, d := range defs {
			seen[d["name"].(string)] = true
		}
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
