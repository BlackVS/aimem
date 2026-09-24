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
}}

func isGuidanceTool(name string) bool { return name == "team_context" }

// guidanceTool answers team_context from the embedded unit. The version it
// names is this binary's build version.
func guidanceTool(raw json.RawMessage) (string, error) {
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
