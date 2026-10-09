package mcp

// write_report: a team member delivers a report as a hub document (aicrew's
// control-plane design, A0c, 8.4). It is listed only in a team conversation
// and calls the hub's team-mode document write with the pinned context. The
// hub limits it to report- documents of a project the team is granted and
// keeps the revisions compare-and-swap.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"aimem/internal/store"
	"aimem/internal/teamsession"
)

const (
	writeReportTool    = "write_report"
	teamReportPrefix   = "report-"
	writeReportMaxBody = store.MaxDocBytes
)

var writeReportToolDef = map[string]any{
	"name": writeReportTool,
	"description": "Deliver a report as a shared document of a project your team is granted. The name must start with report-, " +
		"for example report-<task id>. base_rev is the revision you read with read_doc, or 0 to create the document; " +
		"a stale base_rev returns the current revision instead of writing. Any team role may write a report; " +
		"the hub records you, your team and your role as its writer.",
	"inputSchema": objSchema(map[string]any{
		"name":     prop("string", "document name, starting with report-"),
		"body":     prop("string", "the report, in Markdown"),
		"base_rev": prop("integer", "the revision you read, or 0 to create the document"),
		"project":  prop("string", "project id (defaults to the current project)"),
	}, "name", "body", "base_rev"),
}

type writeReportArgs struct {
	Name    string `json:"name"`
	Body    string `json:"body"`
	BaseRev *int64 `json:"base_rev"`
	Project string `json:"project"`
}

// writeReportArgsOf decodes and checks a write_report call without any hub
// call.
func (s *srv) writeReportArgsOf(raw json.RawMessage) (writeReportArgs, error) {
	var a writeReportArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return a, fmt.Errorf("arguments: %w", err)
	}
	switch {
	case !strings.HasPrefix(a.Name, teamReportPrefix):
		return a, fmt.Errorf("name must start with %q: a team conversation writes report documents only", teamReportPrefix)
	case a.BaseRev == nil || *a.BaseRev < 0:
		return a, errors.New("base_rev is required: the revision you read, or 0 to create the document")
	case len(a.Body) > writeReportMaxBody:
		return a, fmt.Errorf("body is %d bytes; the limit is %d", len(a.Body), writeReportMaxBody)
	}
	if a.Project == "" {
		a.Project = s.project
	}
	if a.Project == "" {
		return a, errors.New("project argument is required on this server")
	}
	return a, nil
}

// writeReport calls the hub's team-mode document write.
func (s *srv) writeReport(ctx context.Context, a writeReportArgs) (string, error) {
	if s.tasks == nil {
		return "", errors.New("write_report is not available on this server")
	}
	path := "/v1/projects/" + url.PathEscape(a.Project) + "/docs/" + url.PathEscape(a.Name)
	payload, err := json.Marshal(map[string]any{"body": a.Body, "base_rev": *a.BaseRev})
	if err != nil {
		return "", err
	}
	status, resp, err := s.tasks(ctx, "PUT", path, nil, payload)
	if err != nil {
		return "", err
	}
	if status/100 != 2 {
		if r := teamsession.ParseRefusal(status, resp); r != nil {
			return "", r
		}
		var e struct {
			Error string `json:"error"`
			Rev   int64  `json:"rev"`
		}
		json.Unmarshal(resp, &e)
		if status == 409 {
			return "", fmt.Errorf("%s is at rev %d, not %d: read it with read_doc, merge your report into it, and write again with base_rev %d", a.Name, e.Rev, *a.BaseRev, e.Rev)
		}
		if e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d", status)
		}
		return "", errors.New(e.Error)
	}
	var out struct {
		Rev int64 `json:"rev"`
	}
	json.Unmarshal(resp, &out)
	return fmt.Sprintf("%s written in %s at rev %d", a.Name, a.Project, out.Rev), nil
}
