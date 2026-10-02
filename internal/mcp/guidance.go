package mcp

// team_context serves the team protocol guidance built into this binary
// (internal/teamguide): public text, so it needs no session, checkout,
// project grant, hub call or Git fetch, and it is listed on both facades
// for every caller the facade already admits. It takes a role or a section
// id and nothing else.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"aimem/internal/server"
	"aimem/internal/teamguide"
	"aimem/internal/writingrule"
)

var guidanceToolDefs = []map[string]any{{
	"name": "team_context",
	"description": "Read the aimem team protocol guidance built into this aimem binary: the coordinator and worker playbooks, question and escalation rules, and the request templates, with the unit's version and SHA-256 digest. " +
		"role returns that role's complete required set (read it before team work; it ends with a terminator line, and text without that line was cut by the client: then read the sections one by one); section returns one section by id; neither returns the index of ids, sizes and digests. " +
		"Needs no team session, checkout, project grant, network or shell, and changes nothing. Public guidance: it grants no permission, and the project's selected process handbook remains the authority for project policy. Accepts only a role or a section id, never a path, URL, repository, commit or command.",
	"inputSchema": objSchema(map[string]any{
		"role":    propEnum("the role whose complete required set to return", teamguide.Roles...),
		"section": prop("string", "one section id from the index, e.g. worker or example/submit"),
	}),
}, writingToolDef}

// writingToolDef serves the rule for kept text (docs/WRITING-PERSISTED-TEXT.md).
// Every write tool's description points here, so it is listed wherever a
// write tool is, team conversations included.
var writingToolDef = map[string]any{
	"name": writingTool,
	"description": "Read aimem's rule for text that is kept: the exact text you send to a write tool (task fields and comments, documents, memories, records, team messages and reasons) and the Markdown files you save. " +
		"Returns the whole rule built into this aimem binary, with its version and SHA-256 digest; it ends with a terminator line, and text without that line was cut by the client. " +
		"Needs no session, project, network or shell, takes no arguments and changes nothing.",
	"inputSchema": objSchema(map[string]any{}),
}

const writingTool = "writing_rule"

func isGuidanceTool(name string) bool { return name == "team_context" || name == writingTool }

// writingRuleTool answers writing_rule. It takes no arguments: an empty
// object or none.
func writingRuleTool(raw json.RawMessage) (string, error) {
	if len(raw) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		var none struct{}
		if err := dec.Decode(&none); err != nil {
			return "", errors.New("writing_rule takes no arguments: " + err.Error())
		}
		if dec.Decode(&struct{}{}) != io.EOF {
			return "", errors.New("expected one JSON object")
		}
	}
	return writingrule.Text(server.Version)
}

// guidanceTool answers team_context from the embedded unit and
// writing_rule from the embedded rule. The version each names is this
// binary's build version.
func guidanceTool(name string, raw json.RawMessage) (string, error) {
	if name == writingTool {
		return writingRuleTool(raw)
	}
	var a struct {
		Role    string `json:"role"`
		Section string `json:"section"`
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
		return "", errors.New("team_context takes only role or section: " + err.Error())
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return "", errors.New("expected one JSON object")
	}
	u, err := teamguide.Embedded()
	if err != nil {
		return "", err
	}
	version := server.Version
	switch {
	case a.Role != "" && a.Section != "":
		return "", errors.New("give role or section, not both")
	case a.Role != "":
		return u.Role(a.Role, version)
	case a.Section != "":
		return u.SectionText(a.Section, version)
	}
	return u.Index(version)
}
