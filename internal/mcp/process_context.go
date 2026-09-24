package mcp

// process_context delivers the project's selected process through the
// checkout-bound local MCP: the complete unit or one template by kind, from
// processctx.Load for this process's checkout and state root, with the
// checkout's own credential selected strictly. It takes no path, URL,
// repository, commit or project: the selection is the hub's and the
// project is the checkout's. The hub facade has no checkout and neither
// lists nor serves it.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"aimem/internal/processctx"
)

var processToolDefs = []map[string]any{{
	"name": "process_context",
	"description": "Read this project's selected process (handbook, checklists gating task states, required skills, template kinds) as the complete unit the session-start hook would inject in full, or one template by kind. " +
		"Runs in this checkout-bound MCP process with the checkout's own task credential (no fallback to another), so no shell is needed; reads only the selection the hub names for this checkout's project and this machine's exact-commit cache or Git at that commit. Changes nothing and grants no permission; the process is the authority for project policy. " +
		"The result ends with a terminator line; text without it was cut by the client. It is delivered in state ready, or in state last_observed (the hub is unreachable: the exact cached commit of the last observed selection, marked as such, never authorization for a task write); every other state is an error naming the state, the cause and the fix: disabled, not_selected, denied, unavailable, too_large. Takes only an optional template kind, never a path, URL, repository, commit, project or command.",
	"inputSchema": objSchema(map[string]any{
		"template": prop("string", "a template kind the process manifest names (e.g. task); omit for the complete unit"),
	}),
}}

func isProcessTool(name string) bool { return name == "process_context" }

// processTool answers process_context for the bound checkout.
func (s *srv) processTool(raw json.RawMessage) (string, error) {
	if s.local == nil {
		return "", errors.New("process_context exists only on the checkout-bound local MCP server (aimem mcp, started by the client in the checkout), never on the hub")
	}
	var a struct {
		Template string `json:"template"`
	}
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if len(raw) > 4<<10 {
		return "", errors.New("arguments must be a JSON object of at most 4 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return "", errors.New("process_context takes only an optional template kind: " + err.Error())
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return "", errors.New("expected one JSON object")
	}
	r := processctx.Load(s.local.dir, s.local.root, "")
	if a.Template != "" {
		return r.Template(a.Template)
	}
	return r.Deliver()
}
