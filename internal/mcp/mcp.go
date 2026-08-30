// Package mcp is the Model Context Protocol facade over the aimem API:
// the interactive-recall door for agents, deliberately separate from the
// automatic checkpoint transport (hooks/plugins), which never depends on a
// model choosing to call a tool. Speaks JSON-RPC 2.0 over stdio
// (newline-delimited), the MCP stdio transport.
package mcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"aimem/internal/adapter"
	"aimem/internal/ident"
	"aimem/internal/redact"
	"aimem/internal/store"
)

const protocolVersion = "2024-11-05"

// Serve runs the MCP server on stdin/stdout. api dials the local aimem
// HTTP API; projectID scopes project memories and journal search; groups
// are the shared knowledge scopes the project has opted into.
func Serve(api *http.Client, projectID string, groups []string) error {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	out := bufio.NewWriter(os.Stdout)
	s := &srv{api: api, project: projectID, groups: groups}
	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		if resp := s.handle([]byte(line)); resp != nil {
			out.Write(append(resp, '\n'))
			out.Flush()
		}
	}
	return in.Err()
}

type srv struct {
	api     *http.Client
	project string // "" on the hub: tools must pass an explicit project
	groups  []string
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func reply(id json.RawMessage, result any, rpcErr *rpcError) []byte {
	if id == nil {
		return nil // notification
	}
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if rpcErr != nil {
		msg["error"] = rpcErr
	} else {
		msg["result"] = result
	}
	b, _ := json.Marshal(msg)
	return b
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *srv) handle(raw []byte) []byte {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil
	}
	switch req.Method {
	case "initialize":
		return reply(req.ID, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "aimem", "version": "0.1.0"},
		}, nil)
	case "notifications/initialized", "notifications/cancelled":
		return nil // notifications get no response
	case "ping":
		return reply(req.ID, map[string]any{}, nil)
	case "tools/list":
		return reply(req.ID, map[string]any{"tools": toolDefs}, nil)
	case "tools/call":
		return s.toolCall(req)
	default:
		return reply(req.ID, nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method})
	}
}

var toolDefs = []map[string]any{
	{
		"name": "recall_memory",
		"description": "Search curated long-term memories (project conventions, user preferences, decisions). " +
			"Returns provenance and corroboration with each hit. Use before asking the user something that may already be recorded.",
		"inputSchema": objSchema(map[string]any{
			"query": prop("string", "search terms"),
			"scope": prop("string", "memory scope: 'project' (default), 'user' (cross-project), "+
				"'both' (project + declared groups + user), or 'group:<name>' for one shared group"),
			"project":      prop("string", "project id override (required on the hub server; defaults to the local project otherwise)"),
			"tag":          prop("string", "only memories carrying this tag"),
			"kind":         prop("string", "only this kind: fact|decision|convention|preference|solution|reference"),
			"token_budget": prop("number", "max tokens of results to return (default 1000)"),
		}, "query"),
	},
	{
		"name": "remember",
		"description": "Store one durable curated fact (a preference, convention, decision, or recurring solution). " +
			"Keep it one self-contained sentence. Use scope 'user' for cross-project facts about the user or machine, " +
			"or 'group:<name>' for knowledge shared with the projects in that group (see .aimem.json). " +
			"Record state, not just technique: if a fix is applied, say so with machine/date. " +
			"When an existing fact's state changes, pass its id as 'supersedes' instead of adding a near-duplicate.",
		"inputSchema": objSchema(map[string]any{
			"text": prop("string", "the fact, one self-contained sentence"),
			"scope": prop("string", "memory scope: 'project' (default), 'user', or 'group:<name>' "+
				"(the project must declare the group in .aimem.json)"),
			"supersedes": prop("string", "id of an existing fact this replaces (updated state of the "+
				"same fact); preserves lineage instead of creating a duplicate"),
			"project": prop("string", "project id override (required on the hub server)"),
			"kind":    prop("string", "fact|decision|convention|preference|solution|reference"),
			"tags":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "entity tags (paths, components, topics)"},
		}, "text"),
	},
	{
		"name": "forget_memory",
		"description": "Retire a curated memory by id (bi-temporal expiry; history is preserved). " +
			"Use when a recalled fact is wrong or obsolete.",
		"inputSchema": objSchema(map[string]any{
			"id":      prop("string", "memory id from recall_memory"),
			"scope":   propEnum("memory scope", "project", "user"),
			"project": prop("string", "project id override (required on the hub server)"),
		}, "id"),
	},
	{
		"name": "review_memories",
		"description": "List stale knowledge needing review: active, unpinned facts that are old, " +
			"thinly corroborated, and untouched since. For each, decide deliberately: still true -> " +
			"confirm_memory; state changed -> remember with supersedes; wrong or obsolete -> " +
			"forget_memory. Never confirm mechanically - check the fact against the repo or the user first.",
		"inputSchema": objSchema(map[string]any{
			"days":    prop("number", "age window in days (default 30): only facts untouched this long"),
			"limit":   prop("number", "max entries (default 20)"),
			"scope":   prop("string", "'project' (default), 'user', or 'group:<name>'"),
			"project": prop("string", "project id override (required on the hub server)"),
		}),
	},
	{
		"name": "confirm_memory",
		"description": "Record a review verdict that a fact is STILL TRUE: an audited touch that " +
			"removes it from the review queue for another age window, plus a modest confidence " +
			"reinforcement. Only after actually verifying it - a wrong confirmation launders stale knowledge.",
		"inputSchema": objSchema(map[string]any{
			"id":      prop("string", "memory id from review_memories or recall_memory"),
			"scope":   propEnum("memory scope", "project", "user"),
			"project": prop("string", "project id override (required on the hub server)"),
		}, "id"),
	},
	{
		"name": "search_journal",
		"description": "Full-text search this project's session journal (completed turns from OpenCode and Claude Code) " +
			"AND its shared documents (runbooks, notes - hits name the doc to fetch whole with read_doc). " +
			"Use to recover what was done in earlier or crashed sessions, or to find which runbook covers a topic.",
		"inputSchema": objSchema(map[string]any{
			"query":   prop("string", "search terms"),
			"limit":   prop("number", "max events (default 5)"),
			"project": prop("string", "project id override (required on the hub server)"),
		}, "query"),
	},
	{
		"name": "get_design_doc",
		"description": "Fetch the synthesized design document of a shared knowledge base this project " +
			"belongs to (chapters become sections, facts are cited). Use it to load the big picture " +
			"of a system before diving into its code. Defaults to the project's first declared group.",
		"inputSchema": objSchema(map[string]any{
			"group": prop("string", "group name (default: the project's first declared group)"),
		}),
	},
	{
		"name":        "list_docs",
		"description": "List the shared documents (whole authored files: runbooks, notes) this project and its knowledge groups keep on the hub, with revision and last writer. Distinct from memories: fetched whole by name, never ranked.",
		"inputSchema": objSchema(map[string]any{
			"scope": prop("string", "'project' (default) or 'group:<name>'"),
		}),
	},
	{
		"name":        "read_doc",
		"description": "Read one shared document from the hub, whole, with its revision. Note the revision: update_doc needs it as base_rev.",
		"inputSchema": objSchema(map[string]any{
			"name":  prop("string", "document name from list_docs, e.g. RUNBOOK"),
			"scope": prop("string", "'project' (default) or 'group:<name>'"),
		}, "name"),
	},
	{
		"name": "update_doc",
		"description": "Update a shared document with compare-and-swap: pass the complete new body and the base_rev you read. " +
			"ON CONFLICT the current revision, writer and body come back: re-read, merge both versions deliberately, and retry " +
			"with the new base_rev - never resubmit your own version unchanged. SESSION-STATE is file-only: edit " +
			"docs/SESSION-STATE.md instead; it publishes automatically on the next checkpoint.",
		"inputSchema": objSchema(map[string]any{
			"name":     prop("string", "document name (base_rev 0 creates a new one)"),
			"body":     prop("string", "the COMPLETE new document body, not a fragment"),
			"base_rev": prop("number", "the revision this edit is based on"),
			"scope":    prop("string", "'project' (default) or 'group:<name>'"),
		}, "name", "body", "base_rev"),
	},
	{
		"name": "list_records",
		"description": "List a structured collection's records (small JSON entries with slash-path ids forming a tree, " +
			"e.g. an API surface: api/messages/create). Shows id, revision, writer. Read one with get_record; " +
			"the local markdown rendered from a collection is GENERATED - edit the record, never the file.",
		"inputSchema": objSchema(map[string]any{
			"collection": prop("string", "collection name (this project's .aimem.json \"collections\" names the bound ones)"),
			"scope":      prop("string", "'group:<name>' for a shared group collection (default: the binding's scope, else this project)"),
		}, "collection"),
	},
	{
		"name":        "get_record",
		"description": "Read one record of a structured collection: its JSON body and revision. Note the revision: put_record needs it as base_rev.",
		"inputSchema": objSchema(map[string]any{
			"collection": prop("string", "collection name"),
			"id":         prop("string", "record id (slash path, e.g. api/messages/create)"),
			"scope":      prop("string", "'group:<name>' for a shared group collection"),
		}, "collection", "id"),
	},
	{
		"name": "put_record",
		"description": "Write ONE record of a structured collection with compare-and-swap: pass the complete JSON object body " +
			"and the base_rev you read (0 creates). The CAS unit is the record - writers on different records never conflict. " +
			"ON CONFLICT the current record comes back: re-read it, re-apply your change to it, retry with its rev.",
		"inputSchema": objSchema(map[string]any{
			"collection": prop("string", "collection name"),
			"id":         prop("string", "record id (slash path; creates the tree position it names)"),
			"body":       prop("string", "the COMPLETE record as one JSON object, e.g. {\"method\":\"POST\",\"summary\":\"...\"}"),
			"base_rev":   prop("number", "the revision this edit is based on (0 to create)"),
			"scope":      prop("string", "'group:<name>' for a shared group collection"),
		}, "collection", "id", "body", "base_rev"),
	},
}

func prop(t, desc string) map[string]any {
	return map[string]any{"type": t, "description": desc}
}

func propEnum(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": vals}
}

func objSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	// Only emit "required" when non-empty: a nil slice marshals to
	// "required": null, which is invalid JSON Schema — OpenCode rejects
	// the whole tools list over it ("Failed to get tools").
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

type toolParams struct {
	Name      string `json:"name"`
	Arguments struct {
		Query       string   `json:"query"`
		Scope       string   `json:"scope"`
		Text        string   `json:"text"`
		ID          string   `json:"id"`
		Project     string   `json:"project"`
		Group       string   `json:"group"`
		Kind        string   `json:"kind"`
		Tag         string   `json:"tag"`
		Tags        []string `json:"tags"`
		Limit       int      `json:"limit"`
		TokenBudget int      `json:"token_budget"`
		Supersedes  string   `json:"supersedes"`
		Name        string   `json:"name"`
		Body        string   `json:"body"`
		BaseRev     int64    `json:"base_rev"`
		Days        int      `json:"days"`
		Collection  string   `json:"collection"`
	} `json:"arguments"`
}

func (s *srv) toolCall(req rpcRequest) []byte {
	var p toolParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		return reply(req.ID, nil, &rpcError{Code: -32602, Message: err.Error()})
	}
	text, err := s.run(&p)
	if err != nil {
		// Tool-level errors go back as content with isError, per MCP.
		return reply(req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "error: " + err.Error()}},
			"isError": true,
		}, nil)
	}
	return reply(req.ID, map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}, nil)
}

// scopeProjects resolves a tool's scope to backing project ids. explicit
// is the tool's project argument; it is required in hub mode (s.project
// empty — a remote server has no cwd to derive from).
func (s *srv) scopeProjects(scope, explicit string) ([]string, error) {
	project := s.project
	if explicit != "" {
		project = explicit
	}
	hubMode := s.project == ""
	if project == "" && scope != "user" && !strings.HasPrefix(scope, "group:") {
		return nil, fmt.Errorf("project argument is required on this server")
	}
	switch {
	case scope == "user":
		return []string{store.UserScopeProject}, nil
	case scope == "both":
		out := append([]string{project}, s.groups...)
		return append(out, store.UserScopeProject), nil
	case strings.HasPrefix(scope, "group:"):
		id, err := ident.GroupProject(strings.TrimPrefix(scope, "group:"))
		if err != nil {
			return nil, err
		}
		if hubMode {
			return []string{id}, nil // token holder is trusted hub-wide
		}
		for _, g := range s.groups {
			if g == id {
				return []string{id}, nil
			}
		}
		return nil, fmt.Errorf("this project has not declared group %q in .aimem.json", strings.TrimPrefix(scope, "group:"))
	default:
		return []string{project}, nil
	}
}

func (s *srv) run(p *toolParams) (string, error) {
	a := p.Arguments
	switch p.Name {
	case "recall_memory":
		if a.Query == "" {
			return "", fmt.Errorf("query is required")
		}
		projs, err := s.scopeProjects(a.Scope, a.Project)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		total := 0
		for _, proj := range projs {
			var res struct {
				Memories []store.Memory `json:"memories"`
			}
			if err := s.get(fmt.Sprintf("/v1/projects/%s/memories/recall?q=%s&budget=%d&tag=%s&kind=%s",
				url.PathEscape(proj), url.QueryEscape(a.Query), a.TokenBudget,
				url.QueryEscape(a.Tag), url.QueryEscape(a.Kind)), &res); err != nil {
				return "", err
			}
			for _, m := range res.Memories {
				total++
				tags := ""
				if len(m.Tags) > 0 {
					tags = " #" + strings.Join(m.Tags, " #")
				}
				fmt.Fprintf(&b, "[%s] (%s %s conf=%.1f corroborated %dx since %s%s) %s\n",
					m.ID, scopeName(proj, s.project), m.Kind, m.Confidence,
					m.Corroboration, m.CreatedAt[:10], tags, m.Text)
			}
		}
		if total == 0 {
			return "no memories match", nil
		}
		return b.String(), nil

	case "remember":
		if a.Text == "" {
			return "", fmt.Errorf("text is required")
		}
		projs, err := s.scopeProjects(a.Scope, a.Project)
		if err != nil {
			return "", err
		}
		proj := projs[0]
		var res struct {
			ID         string `json:"id"`
			Reasserted bool   `json:"reasserted"`
			Superseded string `json:"superseded"`
		}
		if a.Supersedes != "" {
			if err := s.post(fmt.Sprintf("/v1/projects/%s/memories/%s/supersede",
				url.PathEscape(proj), url.PathEscape(a.Supersedes)),
				map[string]any{"text": a.Text, "actor": "mcp", "kind": a.Kind, "tags": a.Tags}, &res); err != nil {
				return "", err
			}
			return fmt.Sprintf("superseded %s -> %s (scope %s)", res.Superseded, res.ID, scopeName(proj, s.project)), nil
		}
		if err := s.post(fmt.Sprintf("/v1/projects/%s/memories", url.PathEscape(proj)),
			map[string]any{"text": a.Text, "actor": "mcp", "kind": a.Kind, "tags": a.Tags}, &res); err != nil {
			return "", err
		}
		if res.Reasserted {
			return fmt.Sprintf("already known; corroboration added (id %s)", res.ID), nil
		}
		return fmt.Sprintf("remembered (id %s, scope %s)", res.ID, scopeName(proj, s.project)), nil

	case "forget_memory":
		if a.ID == "" {
			return "", fmt.Errorf("id is required")
		}
		projs, err := s.scopeProjects(a.Scope, a.Project)
		if err != nil {
			return "", err
		}
		proj := projs[0]
		if err := s.post(fmt.Sprintf("/v1/projects/%s/memories/%s/forget",
			url.PathEscape(proj), url.PathEscape(a.ID)), map[string]any{"actor": "mcp"}, &struct{}{}); err != nil {
			return "", err
		}
		return "forgotten " + a.ID, nil

	case "review_memories":
		projs, err := s.scopeProjects(a.Scope, a.Project)
		if err != nil {
			return "", err
		}
		days := a.Days
		if days <= 0 {
			days = store.DefaultReviewAgeDays
		}
		limit := a.Limit
		if limit <= 0 {
			limit = 20
		}
		var res struct {
			Items  []store.ReviewItem `json:"items"`
			Cutoff string             `json:"cutoff"`
		}
		if err := s.get(fmt.Sprintf("/v1/projects/%s/memories/review?days=%d&limit=%d",
			url.PathEscape(projs[0]), days, limit), &res); err != nil {
			return "", err
		}
		if len(res.Items) == 0 {
			return fmt.Sprintf("review queue is clear (nothing unreviewed since %s)", res.Cutoff), nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%d fact(s) unreviewed since %s. Verify each against reality, then confirm_memory (still true), remember+supersedes (state changed), or forget_memory (obsolete):\n",
			len(res.Items), res.Cutoff)
		for _, it := range res.Items {
			fmt.Fprintf(&b, "- [%s] %s (kind %s, confidence %.2f, corroboration %d, last seen %s)\n",
				it.ID, clip(it.Text, 300), it.Kind, it.Confidence, it.Corroboration, it.LastSeen)
		}
		return b.String(), nil

	case "confirm_memory":
		if a.ID == "" {
			return "", fmt.Errorf("id is required")
		}
		projs, err := s.scopeProjects(a.Scope, a.Project)
		if err != nil {
			return "", err
		}
		if err := s.post(fmt.Sprintf("/v1/projects/%s/memories/%s/confirm",
			url.PathEscape(projs[0]), url.PathEscape(a.ID)), map[string]any{"actor": "mcp"}, &struct{}{}); err != nil {
			return "", err
		}
		return "confirmed " + a.ID + " (audited; re-queued after the age window)", nil

	case "search_journal":
		if a.Query == "" {
			return "", fmt.Errorf("query is required")
		}
		project := s.project
		if a.Project != "" {
			project = a.Project
		}
		if project == "" {
			return "", fmt.Errorf("project argument is required on this server")
		}
		limit := a.Limit
		if limit <= 0 {
			limit = 5
		}
		var res struct {
			Events  []store.StoredEvent `json:"events"`
			Docs    []store.DocMatch    `json:"docs"`
			Records []store.ColMatch    `json:"records"`
		}
		if err := s.get(fmt.Sprintf("/v1/projects/%s/search?q=%s&limit=%d",
			url.PathEscape(project), url.QueryEscape(a.Query), limit), &res); err != nil {
			return "", err
		}
		if len(res.Events) == 0 && len(res.Docs) == 0 && len(res.Records) == 0 {
			return "no journal events, shared documents, or wiki records match", nil
		}
		var b strings.Builder
		if len(res.Docs) > 0 {
			b.WriteString("Shared documents matching (fetch whole with read_doc):\n")
			for _, d := range res.Docs {
				fmt.Fprintf(&b, "- %s (rev %d, %s): %s\n", d.Name, d.Rev, d.UpdatedAt, d.Snippet)
			}
			b.WriteString("\n")
		}
		if len(res.Records) > 0 {
			b.WriteString("Wiki records matching (fetch with get_record):\n")
			for _, r := range res.Records {
				fmt.Fprintf(&b, "- %s/%s (rev %d, %s): %s\n", r.Collection, r.ID, r.Rev, r.UpdatedAt, r.Snippet)
			}
			b.WriteString("\n")
		}
		for _, e := range res.Events {
			fmt.Fprintf(&b, "--- %s %s session=%s turn=%s (%s)\nuser: %s\nassistant: %s\n",
				e.TS, e.Client, e.SessionID, e.TurnID, e.Outcome,
				clip(e.UserRequest, 400), clip(e.AssistantReply, 800))
		}
		return b.String(), nil

	case "get_design_doc":
		gid := ""
		if a.Group != "" {
			id, err := ident.GroupProject(a.Group)
			if err != nil {
				return "", err
			}
			if !slices.Contains(s.groups, id) {
				return "", fmt.Errorf("this project has not declared group %q in .aimem.json", a.Group)
			}
			gid = id
		} else if len(s.groups) > 0 {
			gid = s.groups[0]
		} else {
			return "", fmt.Errorf("this project declares no groups (add {\"groups\":[...]} to .aimem.json)")
		}
		var res struct {
			Value string `json:"value"`
		}
		if err := s.get(fmt.Sprintf("/v1/projects/%s/meta/design_doc", url.PathEscape(gid)), &res); err != nil {
			return "", err
		}
		if res.Value == "" {
			return fmt.Sprintf("no design document stored for %s yet (enable feature \"doc\" on the group, or run `aimem doc %s`)",
				gid, strings.TrimPrefix(gid, "group-")), nil
		}
		return res.Value, nil
	case "list_docs", "read_doc", "update_doc":
		return s.docTool(p)
	case "list_records", "get_record", "put_record":
		return s.colTool(p)
	}
	return "", fmt.Errorf("unknown tool %q", p.Name)
}

func scopeName(proj, current string) string {
	if proj == store.UserScopeProject {
		return "user"
	}
	if proj == current {
		return "project"
	}
	if strings.HasPrefix(proj, "group-") {
		return "group:" + strings.TrimPrefix(proj, "group-")
	}
	return proj
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (s *srv) get(path string, into any) error {
	resp, err := s.api.Get("http://aimem" + path)
	if err != nil {
		return fmt.Errorf("aimem service unreachable: %w", err)
	}
	defer resp.Body.Close()
	return decodeAPI(resp, into)
}

func (s *srv) post(path string, body, into any) error {
	b, _ := json.Marshal(body)
	resp, err := s.api.Post("http://aimem"+path, "application/json", strings.NewReader(string(b)))
	if err != nil {
		return fmt.Errorf("aimem service unreachable: %w", err)
	}
	defer resp.Body.Close()
	return decodeAPI(resp, into)
}

func decodeAPI(resp *http.Response, into any) error {
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("%s", e.Error)
	}
	return json.Unmarshal(raw, into)
}

// DefaultProject resolves the MCP server's project scope and declared
// knowledge groups from its working directory (clients spawn MCP servers in
// the project directory).
func DefaultProject() (string, []string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", nil, err
	}
	id, err := ident.ProjectID(cwd)
	if err != nil {
		return "", nil, err
	}
	groups, err := ident.ProjectGroups(cwd)
	if err != nil {
		return "", nil, err
	}
	return id, groups, nil
}

// NewHTTPHandler returns the streamable-HTTP MCP endpoint used on the hub:
// each POST carries one JSON-RPC message and receives a JSON response (this
// server never needs the SSE upgrade — all tools are quick request/reply).
// Auth happens outside (bearer middleware on the TCP listener). Runs in hub
// mode: tools must pass an explicit project argument.
func NewHTTPHandler(api *http.Client) http.Handler {
	s := &srv{api: api}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		resp := s.handle(raw)
		if resp == nil {
			w.WriteHeader(http.StatusAccepted) // notification
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(resp)
	})
}

// docProject resolves a doc tool's scope to (projectID, hub). Shared
// documents are hub-authoritative - the local service holds none - so
// unlike the other tools these talk to the project's hub directly and
// refuse plainly when offline: CAS needs the hub, and the bound file plus
// the checkpoint publisher already cover the offline case.
func (s *srv) docProject(scope string) (string, *adapter.HubConfig, error) {
	project := s.project
	if strings.HasPrefix(scope, "group:") {
		id, err := ident.GroupProject(strings.TrimPrefix(scope, "group:"))
		if err != nil {
			return "", nil, err
		}
		if s.project != "" && !slices.Contains(s.groups, id) {
			return "", nil, fmt.Errorf("this project has not declared group %q in .aimem.json", strings.TrimPrefix(scope, "group:"))
		}
		project = id
	}
	if project == "" {
		return "", nil, fmt.Errorf("a project or group scope is required on this server")
	}
	hubName, _ := ident.ProjectHubName(".")
	_, hub := adapter.ResolveHub(mcpStateRoot(), hubName)
	if hub == nil {
		return "", nil, fmt.Errorf("no hub configured - shared documents live on the project's hub; for a bound file, edit it directly and the checkpoint publisher delivers it when a hub is reachable")
	}
	return project, hub, nil
}

func (s *srv) docTool(p *toolParams) (string, error) {
	a := p.Arguments
	project, hub, err := s.docProject(a.Scope)
	if err != nil {
		return "", err
	}
	c := adapter.NewClient(mcpStateRoot())
	switch p.Name {
	case "list_docs":
		// Design §4b: the DEFAULT listing covers this project AND its
		// member groups — a group runbook has no bound file in most member
		// checkouts, so for most agents this tool is its only access. An
		// explicit scope narrows to that scope alone.
		scopes := []string{project}
		if a.Scope == "" && s.project != "" {
			scopes = append(scopes, s.groups...)
		}
		var b strings.Builder
		total := 0
		for _, sc := range scopes {
			docs, err := c.ListHubDocs(hub, sc)
			if err != nil {
				if sc == project {
					return "", err
				}
				continue // a group with no hub-side presence yet is not an error
			}
			for _, d := range docs {
				mark := ""
				if d.Deleted {
					mark = " [deleted]"
				}
				scopeNote := ""
				if sc != project {
					scopeNote = "  [group:" + strings.TrimPrefix(sc, "group-") + "]"
				}
				fmt.Fprintf(&b, "%s  rev %d  %s by %s%s%s", d.Name, d.Rev, d.UpdatedAt, d.UpdatedBy, scopeNote, mark)
				b.WriteByte(10)
			}
			total += len(docs)
		}
		if total == 0 {
			return "no shared documents yet", nil
		}
		return b.String(), nil
	case "read_doc":
		doc, err := c.GetHubDoc(hub, project, a.Name, 0)
		if err != nil {
			return "", err
		}
		if doc.Deleted {
			return fmt.Sprintf("%s was deleted at rev %d by %s", a.Name, doc.Rev, doc.UpdatedBy), nil
		}
		head := fmt.Sprintf("%s (rev %d, %s by %s):", a.Name, doc.Rev, doc.UpdatedAt, doc.UpdatedBy)
		return head + string(rune(10)) + doc.Body, nil
	case "update_doc":
		// The handoff is file-only by decision: it already has a dedicated
		// flow (edit docs/SESSION-STATE.md; the checkpoint publishes it),
		// and a second write path would race that flow.
		if a.Name == "SESSION-STATE" {
			return "", fmt.Errorf("SESSION-STATE is file-only: edit docs/SESSION-STATE.md - it publishes automatically on the next checkpoint")
		}
		host, _ := os.Hostname()
		doc, err := c.PutHubDoc(hub, project, a.Name, a.Body, host+"/mcp", a.BaseRev)
		var conflict *adapter.DocConflictError
		if errors.As(err, &conflict) {
			return "", fmt.Errorf("CONFLICT: %s is now rev %d (by %s). Re-read, merge both versions deliberately, retry with base_rev %d — or, for a doc bound to a local file, run `aimem docs merge %s` for a three-way merge against the last-synced revision. Current body:%s%s",
				a.Name, conflict.Doc.Rev, conflict.Doc.UpdatedBy, conflict.Doc.Rev, a.Name, string(rune(10)), clip(conflict.Doc.Body, 8000))
		}
		if err != nil {
			return "", err
		}
		msg := fmt.Sprintf("%s updated to rev %d", a.Name, doc.Rev)
		// DESIGN-shared-docs §4b: a successful update of a BOUND doc also
		// rewrites the local file and the sidecar bookkeeping (equivalent
		// to push followed by pull) — otherwise the next checkpoint's
		// hash-publish would fight this very write with a spurious
		// conflict, and the local file would silently go stale.
		if !strings.HasPrefix(a.Scope, "group:") {
			if rel := boundDocPath(".", a.Name); rel != "" {
				if werr := writeFileAtomic(filepath.FromSlash(rel), []byte(a.Body)); werr == nil {
					c.DocSyncRecord(project, a.Name, doc.Rev, adapter.DocBodyHash([]byte(a.Body)))
					msg += ", bound file " + rel + " rewritten"
				} else {
					msg += fmt.Sprintf(" — but bound file %s was NOT rewritten (%v); run `aimem docs pull %s`", rel, werr, a.Name)
				}
			}
		}
		return msg, nil
	}
	return "", fmt.Errorf("unknown doc tool %q", p.Name)
}

// colTool handles the structured-collection tools (DESIGN-structured-docs).
// Collections are hub-authoritative like documents; the scope defaults to
// the .aimem.json binding for the named collection, so an agent in a
// project that declared {"collections":[{"name":"api","scope":"group:fw"}]}
// addresses the shared group collection without saying so.
func (s *srv) colTool(p *toolParams) (string, error) {
	a := p.Arguments
	scope := a.Scope
	if scope == "" {
		for _, b := range ident.ProjectCollections(".") {
			if b.Name == a.Collection {
				scope = b.Scope
				break
			}
		}
	}
	project, hub, err := s.docProject(scope)
	if err != nil {
		return "", err
	}
	c := adapter.NewClient(mcpStateRoot())
	switch p.Name {
	case "list_records":
		recs, err := c.ListHubRecords(hub, project, a.Collection, false)
		if err != nil {
			return "", err
		}
		if len(recs) == 0 {
			return fmt.Sprintf("collection %q has no records yet (put_record with base_rev 0 creates one)", a.Collection), nil
		}
		var b strings.Builder
		for _, r := range recs {
			if r.Deleted {
				continue
			}
			fmt.Fprintf(&b, "%s  rev %d  %dB  %s by %s\n", r.ID, r.Rev, r.Size, r.UpdatedAt, r.UpdatedBy)
		}
		return b.String(), nil
	case "get_record":
		rec, err := c.GetHubRecord(hub, project, a.Collection, a.ID, 0)
		if err != nil {
			return "", err
		}
		if rec.Deleted {
			return fmt.Sprintf("%s/%s was deleted at rev %d by %s", a.Collection, rec.ID, rec.Rev, rec.UpdatedBy), nil
		}
		return fmt.Sprintf("%s/%s (rev %d, %s by %s):\n%s",
			a.Collection, rec.ID, rec.Rev, rec.UpdatedAt, rec.UpdatedBy, string(rec.Body)), nil
	case "put_record":
		host, _ := os.Hostname()
		rec, err := c.PutHubRecord(hub, project, a.Collection, a.ID, []byte(a.Body), host+"/mcp", a.BaseRev)
		var conflict *adapter.RecordConflictError
		if errors.As(err, &conflict) {
			return "", fmt.Errorf("CONFLICT: %s/%s is now rev %d (by %s). Re-apply your change onto the current record and retry with base_rev %d. Current body:\n%s",
				a.Collection, a.ID, conflict.Record.Rev, conflict.Record.UpdatedBy, conflict.Record.Rev, string(conflict.Record.Body))
		}
		if err != nil {
			return "", err
		}
		msg := fmt.Sprintf("%s/%s written at rev %d (a rendered markdown file, if any, is regenerated with `aimem col render %s`)",
			a.Collection, rec.ID, rec.Rev, a.Collection)
		// Softer-tier secret warning, same as documents get: the hub
		// refuses the unambiguous shapes; a softer match stores as written.
		if warn, _ := redact.ScanAuthored(a.Body); len(warn) > 0 {
			msg += fmt.Sprintf(" — WARNING: secret-shaped content (%s) stored as written", strings.Join(warn, ", "))
		}
		return msg, nil
	}
	return "", fmt.Errorf("unknown collection tool %q", p.Name)
}

// boundDocPath returns the project-relative path bound to a doc name in
// dir, or "" when the doc has no bound file in this checkout (the normal
// case for group docs).
func boundDocPath(dir, name string) string {
	for _, rel := range ident.ProjectDocs(dir) {
		if ident.DocName(rel) == name {
			return rel
		}
	}
	return ""
}

// writeFileAtomic mirrors the codebase's tmp+rename habit so a crash
// mid-write can never leave a half-written bound file.
func writeFileAtomic(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// mcpStateRoot mirrors the CLI's state-root resolution: docs tools reach
// the hub with the machine's hub config, which lives there.
func mcpStateRoot() string {
	if v := os.Getenv("AIMEM_STATE_DIR"); v != "" {
		return v
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "aimem")
}
