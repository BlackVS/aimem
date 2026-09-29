package mcp

// The first pilot's knowledge tools for scoped callers (task 19d8,
// docs/DESIGN-AIFORGE-KNOWLEDGE.md): a team conversation and an ordinary
// token on the hub's /mcp. recall_memory, list_docs and read_doc call the
// hub's three read routes with the caller's own authority (the task caller:
// the pinned team context, or the request's identity), never the trusted
// local client and never a local answer. They are project-scoped: any other
// scope is refused here, before any hub call. The hub checks the grant on
// every call; nothing is cached.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"aimem/internal/store"
	"aimem/internal/teamsession"
)

// scopedKnowledgeTools are the pilot set's tool names.
var scopedKnowledgeTools = map[string]bool{"recall_memory": true, "list_docs": true, "read_doc": true}

var scopedScopeProp = propEnum("'project' (the default, and the only scope this credential may read)", "project")

var scopedKnowledgeToolDefs = []map[string]any{
	{
		"name": "recall_memory",
		"description": "Search one project's curated long-term memories (conventions, decisions, preferences). " +
			"Returns provenance and corroboration with each hit. Project scope only: personal and group spaces are not readable with this credential.",
		"inputSchema": objSchema(map[string]any{
			"query":        prop("string", "search terms"),
			"scope":        scopedScopeProp,
			"project":      prop("string", "project id (defaults to the current project)"),
			"tag":          prop("string", "only memories carrying this tag"),
			"kind":         prop("string", "only this kind: fact|decision|convention|preference|solution|reference"),
			"token_budget": prop("number", "max tokens of results to return (default 1000)"),
		}, "query"),
	},
	{
		"name":        "list_docs",
		"description": "List one project's shared documents (runbooks, notes) with revision and last writer. Project scope only.",
		"inputSchema": objSchema(map[string]any{
			"scope":   scopedScopeProp,
			"project": prop("string", "project id (defaults to the current project)"),
		}),
	},
	{
		"name":        "read_doc",
		"description": "Read one shared document of a project, whole, at its current revision. Project scope only.",
		"inputSchema": objSchema(map[string]any{
			"name":    prop("string", "document name from list_docs, e.g. RUNBOOK"),
			"scope":   scopedScopeProp,
			"project": prop("string", "project id (defaults to the current project)"),
		}, "name"),
	},
}

type knowledgeArgs struct {
	Query       string `json:"query"`
	Scope       string `json:"scope"`
	Project     string `json:"project"`
	Tag         string `json:"tag"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	TokenBudget int    `json:"token_budget"`
}

// knowledgeArgsOf decodes and checks a pilot knowledge call without any hub
// call: a scope other than the project, or no project, is refused here.
func (s *srv) knowledgeArgsOf(raw json.RawMessage) (knowledgeArgs, error) {
	var a knowledgeArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return a, err
		}
	}
	if a.Scope != "" && a.Scope != "project" {
		return a, fmt.Errorf("scope %q is not available to this credential: knowledge reads are project-scoped (scope 'project' or omitted); personal and group spaces are not readable", a.Scope)
	}
	if a.Project == "" {
		a.Project = s.project
	}
	if a.Project == "" {
		return a, errors.New("project argument is required on this server")
	}
	return a, nil
}

// scopedKnowledgeTool serves one pilot knowledge tool through s.tasks.
func (s *srv) scopedKnowledgeTool(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	a, err := s.knowledgeArgsOf(raw)
	if err != nil {
		return "", err
	}
	return s.scopedKnowledgeRead(ctx, name, a)
}

func (s *srv) scopedKnowledgeRead(ctx context.Context, name string, a knowledgeArgs) (string, error) {
	if s.tasks == nil {
		return "", errors.New("knowledge tools are not available on this server")
	}
	project := a.Project
	base := "/v1/projects/" + url.PathEscape(project)
	get := func(path string, into any) error {
		status, resp, err := s.tasks(ctx, "GET", base+path, nil, nil)
		if err != nil {
			return err
		}
		if status/100 != 2 {
			// A team-mode refusal carries the contract's envelope.
			if r := teamsession.ParseRefusal(status, resp); r != nil {
				return r
			}
			var e struct {
				Error string `json:"error"`
			}
			json.Unmarshal(resp, &e)
			if e.Error == "" {
				e.Error = fmt.Sprintf("HTTP %d", status)
			}
			return errors.New(e.Error)
		}
		return json.Unmarshal(resp, into)
	}
	switch name {
	case "recall_memory":
		if a.Query == "" {
			return "", errors.New("query is required")
		}
		budget := a.TokenBudget
		if budget <= 0 {
			budget = 1000
		}
		var res struct {
			Memories []store.Memory `json:"memories"`
		}
		q := url.Values{"q": {a.Query}, "budget": {fmt.Sprint(budget)}, "tag": {a.Tag}, "kind": {a.Kind}}
		if err := get("/memories/recall?"+q.Encode(), &res); err != nil {
			return "", err
		}
		if len(res.Memories) == 0 {
			return "no memories match", nil
		}
		trim := recallTrim{budget: budget}
		for _, m := range res.Memories {
			if !trim.add(recallLine(m, scopeName(project, s.project))) {
				break
			}
		}
		return trim.String(), nil
	case "list_docs":
		var res struct {
			Docs []store.Doc `json:"docs"`
		}
		if err := get("/docs", &res); err != nil {
			return "", err
		}
		if len(res.Docs) == 0 {
			return "no shared documents yet", nil
		}
		var b strings.Builder
		for _, d := range res.Docs {
			mark := ""
			if d.Deleted {
				mark = " [deleted]"
			}
			fmt.Fprintf(&b, "%s  rev %d  %s by %s%s\n", d.Name, d.Rev, d.UpdatedAt, d.UpdatedBy, mark)
		}
		return b.String(), nil
	case "read_doc":
		if a.Name == "" {
			return "", errors.New("name is required")
		}
		var doc store.Doc
		if err := get("/docs/"+url.PathEscape(a.Name), &doc); err != nil {
			return "", err
		}
		if doc.Deleted {
			return fmt.Sprintf("%s was deleted at rev %d by %s", a.Name, doc.Rev, doc.UpdatedBy), nil
		}
		return fmt.Sprintf("%s (rev %d, %s by %s):\n%s", a.Name, doc.Rev, doc.UpdatedAt, doc.UpdatedBy, doc.Body), nil
	}
	return "", fmt.Errorf("unknown tool %q", name)
}
