package mcp

// Task tools and the trust boundary they cross. The model supplies IDs,
// content and retry keys; it never supplies credentials or an actor. On
// the hub, a task tool call is dispatched in-process with the identity
// the bearer middleware authenticated for THIS request. Locally (stdio),
// it is an HTTP call to the project's configured hub with that hub's
// dedicated task credential — never the local socket, which would lend
// the operator's authority to whatever agent is talking.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/ident"
	"aimem/internal/store"
)

// TaskCallFunc performs one task-API request with the caller's own
// authority and returns the HTTP status and body.
type TaskCallFunc func(ctx context.Context, method, path string, headers map[string]string, body []byte) (status int, response []byte, err error)

// PrincipalFunc resolves, per /mcp request, the task caller and whether
// the caller is an ordinary token that may use task tools only.
type PrincipalFunc func(r *http.Request) (tasks TaskCallFunc, tasksOnly bool)

var taskToolDefs = []map[string]any{
	{
		"name": "list_tasks",
		"description": "List task summaries of a project (archived excluded unless asked). " +
			"Pages of up to 100; pass the returned next_cursor as `after`.",
		"inputSchema": objSchema(map[string]any{
			"project":          prop("string", "project id (defaults to the current project)"),
			"state":            propEnum("only this state", "BACKLOG", "READY", "IN_PROGRESS", "REVIEW", "BLOCKED", "DONE", "CANCELLED"),
			"assignee":         assigneeProp("only this assignee"),
			"include_archived": prop("boolean", "include archived tasks"),
			"after":            prop("string", "next_cursor from the previous page"),
			"limit":            prop("integer", "page size (default 20, max 100)"),
		}),
	},
	{
		"name":        "get_task",
		"description": "Read one task by id: current content, revision, owning project and links.",
		"inputSchema": objSchema(map[string]any{"id": prop("string", "task id")}, "id"),
	},
	{
		"name": "create_task",
		"description": "Create a task in a project. Requires an idempotency_key you choose (any unique string); " +
			"retrying with the same key and content returns the task created the first time.",
		"inputSchema": objSchema(taskContentProps(map[string]any{
			"project":         prop("string", "project id (defaults to the current project)"),
			"idempotency_key": prop("string", "your unique key for this create (retry-safe)"),
		}), "title", "idempotency_key"),
	},
	{
		"name": "update_task",
		"description": "Replace a task's editable fields. Send EVERY field you want kept (omitted optional fields clear) " +
			"and the expected_revision you read; a stale revision returns the current task instead.",
		"inputSchema": objSchema(taskContentProps(map[string]any{
			"id":                prop("string", "task id"),
			"expected_revision": prop("integer", "the revision you read"),
			"idempotency_key":   prop("string", "your unique key for this update (retry-safe)"),
		}), "id", "title", "state", "expected_revision", "idempotency_key"),
	},
	{
		"name":        "get_task_history",
		"description": "Accepted revisions of a task in order, each with its full snapshot and actor.",
		"inputSchema": objSchema(map[string]any{
			"id":    prop("string", "task id"),
			"after": prop("integer", "next_cursor from the previous page"),
			"limit": prop("integer", "page size (default 20, max 100)"),
		}, "id"),
	},
	{
		"name":        "list_task_comments",
		"description": "Discussion of a task in append order (immutable Markdown comments).",
		"inputSchema": objSchema(map[string]any{
			"id":    prop("string", "task id"),
			"after": prop("integer", "next_cursor from the previous page"),
			"limit": prop("integer", "page size (default 20, max 100)"),
		}, "id"),
	},
	{
		"name":        "get_task_comment",
		"description": "One comment by task id and comment id.",
		"inputSchema": objSchema(map[string]any{
			"task_id":    prop("string", "task id"),
			"comment_id": prop("string", "comment id"),
		}, "task_id", "comment_id"),
	},
	{
		"name": "add_task_comment",
		"description": "Append a Markdown comment to a task (never edits the task). Requires an idempotency_key; " +
			"the author and time are recorded by the service.",
		"inputSchema": objSchema(map[string]any{
			"id":              prop("string", "task id"),
			"body":            prop("string", "Markdown body (at most 32 KiB)"),
			"idempotency_key": prop("string", "your unique key for this comment (retry-safe)"),
		}, "id", "body", "idempotency_key"),
	},
}

func assigneeProp(desc string) map[string]any {
	return map[string]any{"type": "object", "description": desc + ": {kind: user|group, id: <access identity uuid>}",
		"properties": map[string]any{"kind": propEnum("user or group", "user", "group"), "id": prop("string", "identity uuid")},
		"required":   []string{"kind", "id"}}
}

func taskContentProps(extra map[string]any) map[string]any {
	props := map[string]any{
		"title":               prop("string", "short title (at most 256 bytes)"),
		"objective":           prop("string", "what done looks like"),
		"acceptance_criteria": prop("string", "how it is verified"),
		"non_goals":           prop("string", "explicitly out of scope"),
		"state":               propEnum("BACKLOG (default on create), READY, IN_PROGRESS, REVIEW, BLOCKED, DONE, CANCELLED", "BACKLOG", "READY", "IN_PROGRESS", "REVIEW", "BLOCKED", "DONE", "CANCELLED"),
		"assignee":            assigneeProp("optional assignee"),
		"blocker":             prop("string", "what blocks it, if BLOCKED"),
		"dependencies":        map[string]any{"type": "array", "items": prop("string", "task id"), "description": "advisory task ids this depends on"},
		"candidate_refs":      map[string]any{"type": "array", "items": prop("string", "reference"), "description": "candidate references (PRs, docs, links)"},
		"evidence_refs":       map[string]any{"type": "array", "items": prop("string", "reference"), "description": "evidence references"},
		"next_action":         prop("string", "the next concrete step"),
		"archived":            prop("boolean", "archive (DONE/CANCELLED only)"),
	}
	for k, v := range extra {
		props[k] = v
	}
	return props
}

var taskToolNames = func() map[string]bool {
	m := map[string]bool{}
	for _, d := range taskToolDefs {
		m[d["name"].(string)] = true
	}
	return m
}()

func isTaskTool(name string) bool { return taskToolNames[name] }

// taskArgs are the task-tool arguments: the editable content is the
// storage type itself (one spelling, so a new field cannot be dropped on
// the way through), plus addressing, paging and the retry key. Decoded
// strictly, like the HTTP bodies they become: under replace-all updates a
// misspelled field must be an error, never a silent clear.
type taskArgs struct {
	store.TaskContent
	Project          string          `json:"project"`
	ID               string          `json:"id"`
	TaskID           string          `json:"task_id"`
	CommentID        string          `json:"comment_id"`
	IdempotencyKey   string          `json:"idempotency_key"`
	ExpectedRevision int64           `json:"expected_revision"`
	IncludeArchived  bool            `json:"include_archived"`
	After            json.RawMessage `json:"after"` // list: task id; history/comments: integer
	Limit            int             `json:"limit"`
	Body             string          `json:"body"`
}

func decodeTaskArgs(raw json.RawMessage) (taskArgs, error) {
	var a taskArgs
	if len(raw) == 0 || string(raw) == "null" {
		return a, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return a, fmt.Errorf("arguments: %w", err)
	}
	return a, nil
}

// content is the HTTP body for create/update: the content exactly as
// given (nil lists included; the service normalises them).
func (a *taskArgs) content() (map[string]any, error) {
	raw, err := json.Marshal(a.TaskContent)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// afterString is the list cursor (a task id); afterInt the history and
// comment cursor (a sequence or revision, accepted as a number or a
// numeric string so a model can echo next_cursor either way).
func (a *taskArgs) afterString() (string, error) {
	if len(a.After) == 0 || string(a.After) == "null" {
		return "", nil
	}
	var v string
	if err := json.Unmarshal(a.After, &v); err != nil {
		return "", errors.New("after must be the next_cursor string from the previous page")
	}
	return v, nil
}

func (a *taskArgs) afterInt() (int64, error) {
	if len(a.After) == 0 || string(a.After) == "null" {
		return 0, nil
	}
	var n int64
	if err := json.Unmarshal(a.After, &n); err == nil {
		return n, nil
	}
	var v string
	if err := json.Unmarshal(a.After, &v); err == nil {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n, nil
		}
	}
	return 0, errors.New("after must be the next_cursor number from the previous page")
}

// taskTool translates one task tool call into a task-API request made
// with the caller's authority and returns the JSON result as text.
func (s *srv) taskTool(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	// The caller is resolved per call, never cached: on the stdio facade
	// that re-reads hub.json, so a rotated task credential takes effect
	// without a restart; on the hub it is already per request.
	tasks := s.tasks
	if tasks == nil && s.taskSetup != nil {
		var err error
		if tasks, err = s.taskSetup(); err != nil {
			return "", err
		}
	}
	if tasks == nil {
		return "", errors.New("task tools are not available on this server")
	}
	a, err := decodeTaskArgs(raw)
	if err != nil {
		return "", err
	}
	call := func(method, path string, body any, key string) (string, error) {
		var payload []byte
		if body != nil {
			payload, _ = json.Marshal(body)
		}
		headers := map[string]string{}
		if key != "" {
			headers["Idempotency-Key"] = key
		}
		status, resp, err := tasks(ctx, method, path, headers, payload)
		if err != nil {
			return "", err
		}
		if status/100 != 2 {
			var e struct {
				Error   string          `json:"error"`
				Current json.RawMessage `json:"current"`
			}
			json.Unmarshal(resp, &e)
			if e.Error == "" {
				e.Error = fmt.Sprintf("HTTP %d", status)
			}
			if (status == http.StatusNotFound || status/100 == 3) && !bytes.Contains(resp, []byte(`"error"`)) {
				e.Error = "the hub does not serve task routes (upgrade the hub)"
			}
			if len(e.Current) > 0 {
				return "", fmt.Errorf("%s\ncurrent: %s", e.Error, e.Current)
			}
			return "", errors.New(e.Error)
		}
		var pretty bytes.Buffer
		if json.Indent(&pretty, resp, "", "  ") != nil {
			return "", errors.New("the hub returned a malformed task response")
		}
		return pretty.String(), nil
	}
	project := func() (string, error) {
		p := a.Project
		if p == "" {
			p = s.project
		}
		if p == "" {
			return "", errors.New("project argument is required on this server")
		}
		return url.PathEscape(p), nil
	}
	key := func() (string, error) {
		if a.IdempotencyKey == "" {
			return "", errors.New("idempotency_key is required (choose any unique string; reuse it only to retry this exact call)")
		}
		return a.IdempotencyKey, nil
	}
	taskID := func(id string) (string, error) {
		if id == "" {
			return "", errors.New("task id is required")
		}
		return url.PathEscape(id), nil
	}
	switch name {
	case "list_tasks":
		p, err := project()
		if err != nil {
			return "", err
		}
		q := url.Values{}
		if a.State != "" {
			q.Set("state", a.State)
		}
		if a.Assignee != nil {
			q.Set("assignee", a.Assignee.Kind+"/"+a.Assignee.ID)
		}
		if a.IncludeArchived {
			q.Set("include_archived", "true")
		}
		after, err := a.afterString()
		if err != nil {
			return "", err
		}
		if after != "" {
			q.Set("after", after)
		}
		if a.Limit > 0 {
			q.Set("limit", strconv.Itoa(a.Limit))
		}
		path := "/v1/projects/" + p + "/tasks"
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		return call("GET", path, nil, "")
	case "get_task":
		id, err := taskID(a.ID)
		if err != nil {
			return "", err
		}
		return call("GET", "/v1/tasks/"+id, nil, "")
	case "create_task":
		p, err := project()
		if err != nil {
			return "", err
		}
		k, err := key()
		if err != nil {
			return "", err
		}
		body, err := a.content()
		if err != nil {
			return "", err
		}
		return call("POST", "/v1/projects/"+p+"/tasks", body, k)
	case "update_task":
		id, err := taskID(a.ID)
		if err != nil {
			return "", err
		}
		k, err := key()
		if err != nil {
			return "", err
		}
		body, err := a.content()
		if err != nil {
			return "", err
		}
		body["expected_revision"] = a.ExpectedRevision
		return call("PUT", "/v1/tasks/"+id, body, k)
	case "get_task_history":
		id, err := taskID(a.ID)
		if err != nil {
			return "", err
		}
		after, err := a.afterInt()
		if err != nil {
			return "", err
		}
		return call("GET", "/v1/tasks/"+id+"/history?"+pageQuery(after, a.Limit), nil, "")
	case "list_task_comments":
		id, err := taskID(a.ID)
		if err != nil {
			return "", err
		}
		after, err := a.afterInt()
		if err != nil {
			return "", err
		}
		return call("GET", "/v1/tasks/"+id+"/comments?"+pageQuery(after, a.Limit), nil, "")
	case "get_task_comment":
		id, err := taskID(a.TaskID)
		if err != nil {
			return "", err
		}
		if a.CommentID == "" {
			return "", errors.New("comment_id is required")
		}
		return call("GET", "/v1/tasks/"+id+"/comments/"+url.PathEscape(a.CommentID), nil, "")
	case "add_task_comment":
		id, err := taskID(a.ID)
		if err != nil {
			return "", err
		}
		k, err := key()
		if err != nil {
			return "", err
		}
		return call("POST", "/v1/tasks/"+id+"/comments", map[string]string{"body": a.Body}, k)
	}
	return "", fmt.Errorf("unknown tool %q", name)
}

func pageQuery(after int64, limit int) string {
	q := url.Values{}
	if after > 0 {
		q.Set("after", strconv.FormatInt(after, 10))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return q.Encode()
}

// localTaskCaller is the stdio facade's route to task tools: the project's
// bound hub, with that hub's dedicated task credential. No credential
// configured is an actionable error, never a fallback to the local socket
// or to the hub's shared checkpoint token.
func localTaskCaller() (TaskCallFunc, error) { return taskCallerIn(".", mcpStateRoot()) }

const (
	taskStateEnabled  = "enabled"
	taskStateDisabled = "disabled"
	taskStateUnknown  = "unknown"
)

// probeTaskState asks the project's hub, with the task credential, whether
// tasks are enabled for the project — once, at session start. Anything
// short of a definite answer is unknown: no credential configured, the
// hub unreachable, a non-200, or a hub too old to say. Bounded so a slow
// hub cannot hold the session start.
func probeTaskState(dir, root, project string) string {
	if project == "" {
		return taskStateUnknown
	}
	call, err := taskCallerIn(dir, root)
	if err != nil {
		return taskStateUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	status, body, err := call(ctx, "GET", "/v1/access/identity?project="+url.QueryEscape(project), nil, nil)
	if err != nil || status != http.StatusOK {
		return taskStateUnknown
	}
	var id struct {
		TasksEnabled *bool `json:"tasks_enabled"`
	}
	if json.Unmarshal(body, &id) != nil || id.TasksEnabled == nil {
		return taskStateUnknown
	}
	if *id.TasksEnabled {
		return taskStateEnabled
	}
	return taskStateDisabled
}

// taskCallerIn resolves dir's hub binding strictly: a .aimem.json that
// exists but cannot be parsed is a refusal, never a silent trip to the
// default hub with that hub's credential (the fail-open read the capture
// paths use would answer "default hub" for a broken file).
func taskCallerIn(dir, root string) (TaskCallFunc, error) {
	hubName, err := ident.ProjectHubNameStrict(dir)
	if err != nil {
		return nil, fmt.Errorf("task tools refused: %w — fix the file; nothing was sent to any hub", err)
	}
	return taskCallerFor(root, hubName)
}

// taskCallerFor resolves the hub a project is bound to (hubName "" means
// the default hub) and requires its dedicated task credential.
func taskCallerFor(root, hubName string) (TaskCallFunc, error) {
	name, hub := adapter.ResolveHub(root, hubName)
	switch {
	case hub == nil && name != "":
		return nil, fmt.Errorf("this project is bound to hub %q, which this machine has not configured: aimem hub add %s <url> <token>, then aimem hub task-token %s <ordinary-token>", name, name, name)
	case hub == nil:
		return nil, errors.New("no hub configured for this project: tasks live on the project's hub (aimem hub add <name> <url> <token>, then aimem hub task-token <name> <ordinary-token>)")
	case hub.TaskToken == "":
		return nil, fmt.Errorf("hub %q has no task credential: ask the hub admin for an ordinary token issued to your user for this project, then run `aimem hub task-token %s <token>`", name, name)
	}
	return hubCaller(hub.URL, hub.TaskToken, hub.HTTPClient()), nil
}

// maxTaskResponseBytes bounds one hub response. The largest legal page is
// 100 comments or history snapshots of 32 KiB each; JSON escaping can
// inflate a byte to six, so 32 MiB holds any permitted page, and a longer
// body is an error rather than a silently truncated success.
const maxTaskResponseBytes = 32 << 20

// hubCaller performs task-API requests against a hub with one bearer.
func hubCaller(base, bearer string, client *http.Client) TaskCallFunc {
	return func(ctx context.Context, method, path string, headers map[string]string, body []byte) (int, []byte, error) {
		req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, fmt.Errorf("hub unreachable: %w", err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTaskResponseBytes+1))
		if err != nil {
			return 0, nil, err
		}
		if len(raw) > maxTaskResponseBytes {
			return 0, nil, fmt.Errorf("the hub's response exceeds %d bytes; ask for a smaller page", maxTaskResponseBytes)
		}
		return resp.StatusCode, raw, nil
	}
}
