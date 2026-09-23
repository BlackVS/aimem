package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"aimem/internal/mcp"
	"aimem/internal/uuidv7"
)

const teamsUsage = `usage: aimem teams list <project>
       aimem teams show <project> <team-id>
       aimem teams events <project> <team-id> [key=value ...]
       aimem teams export <project> <team-id> [key=value ...]
       aimem teams create <project> <config.json> [idempotency-key]
       aimem teams configure <project> <team-id> <config.json> [idempotency-key]
       aimem teams recover <project> <team-id> <attempt-id> <reconciliation.json> [idempotency-key]
       aimem teams unmanage <project> <team-id> <task-id> <request.json> [idempotency-key]
       aimem teams rebind-token <project> <team-id> <session-id> <request.json> [idempotency-key]

Run on the hub host as its local operator. recover closes an abandoned attempt
after recorded reconciliation; unmanage releases a task this team manages;
rebind-token moves a session to a replacement token of the same user after
recorded reconciliation. Configuration JSON contains name,
description and enrollment [{user_id,coordinator}]. configure also requires
expected_revision. Configuration replaces all fields; omitted enrollment clears
it. Save/reuse an explicit idempotency key to retry an uncertain write. list/events
print one page with next_cursor; use the HTTP API to page further. export writes
the whole JSONL audit export (header, events, message metadata, end) to stdout,
following pages at one snapshot. Filters for events/export: session_id, task_id,
attempt_id, operation (exact or prefix ending in '.'), since, until (RFC3339);
export also takes limit (page size) and include_bodies=true.

Agent commands (from a configured checkout, using its task credential):
  aimem teams setup TEAM <worker|coordinator> [flags]   verified onboarding: checks, join or
                                                        reconcile, role entry (setup --help)
  aimem teams commands [DIR] [--check]                  write or refresh the /join_team entry
                                                        points for Claude Code, OpenCode and Codex
  aimem teams <join|members|heartbeat|resume|leave|profile|send|messages|inbox|ack> PROJECT TEAM request.json [KEY]
  aimem teams <offer|assignment|reserved|accept|decline|withdraw|block|resume-work|cancel|stopped|close-stop|submit|review|handoff|edit|finalize> PROJECT TEAM request.json [KEY]
KEY is required for writes. join accepts a team name or ID; other commands use
the returned ID. request.json carries the operation fields, including attempt or
task where the operation needs one. See docs/TEAM-AGENT-QUICKSTART.md. The hub
reports workflow_ready:false until lifecycle events reach the inbox.`

func teamsCmd(args []string) error {
	if len(args) > 0 && args[0] == "setup" {
		return teamSetupCmd(args[1:])
	}
	if len(args) > 0 && args[0] == "commands" {
		return teamCommandsCmd(args[1:])
	}
	if len(args) > 0 && teamAgentOps[args[0]] {
		return teamSessionCmd(args)
	}
	method, path, raw, key, err := teamsRequest(args)
	if err != nil {
		return err
	}
	if args[0] == "export" {
		return exportPages(func(path string) ([]byte, error) { return operatorGet(client(), path) }, path, os.Stdout)
	}
	req, err := http.NewRequest(method, "http://aimem"+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		return err
	}
	fmt.Println(pretty.String())
	return nil
}

var auditQueryKeys = map[string]map[string]bool{
	"events": {"session_id": true, "task_id": true, "attempt_id": true, "operation": true, "since": true, "until": true, "after": true, "limit": true},
	"export": {"session_id": true, "task_id": true, "attempt_id": true, "operation": true, "since": true, "until": true, "limit": true, "include_bodies": true},
}

// auditQuery turns key=value arguments into the query string of an audit
// read; the hub validates values, the CLI only refuses keys it cannot pass.
func auditQuery(op string, args []string) (string, error) {
	q := url.Values{}
	for _, arg := range args {
		k, v, ok := strings.Cut(arg, "=")
		if !ok || !auditQueryKeys[op][k] || v == "" || q.Has(k) {
			return "", fmt.Errorf("%s takes key=value filters with distinct keys from: session_id task_id attempt_id operation since until %s", op, map[string]string{"events": "after limit", "export": "limit include_bodies"}[op])
		}
		q.Set(k, v)
	}
	if len(q) == 0 {
		return "", nil
	}
	return "?" + q.Encode(), nil
}

func operatorGet(c *http.Client, path string) ([]byte, error) {
	resp, err := c.Get("http://aimem" + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, body)
	}
	return body, nil
}

// exportPages follows an audit export from its first page to the end record
// marked complete, at the snapshot the first page took, and writes one header,
// every event and message record, and the final end record.
func exportPages(get func(path string) ([]byte, error), path string, out io.Writer) error {
	base, err := url.Parse(path)
	if err != nil {
		return err
	}
	for page := 0; ; page++ {
		if page > 100000 {
			return errors.New("export did not complete")
		}
		body, err := get(base.String())
		if err != nil {
			return err
		}
		var end struct {
			Complete bool             `json:"complete"`
			Next     map[string]int64 `json:"next"`
		}
		sawEnd := false
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			var record struct {
				Record string `json:"record"`
			}
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				return fmt.Errorf("export page %d: %w", page, err)
			}
			switch record.Record {
			case "header":
				if page > 0 {
					continue
				}
			case "end":
				if err := json.Unmarshal([]byte(line), &end); err != nil {
					return err
				}
				sawEnd = true
				if !end.Complete {
					continue
				}
			case "event", "message":
			default:
				return fmt.Errorf("export page %d: unknown record %q", page, record.Record)
			}
			if _, err := fmt.Fprintln(out, line); err != nil {
				return err
			}
		}
		if !sawEnd {
			return fmt.Errorf("export page %d: no end record", page)
		}
		if end.Complete {
			return nil
		}
		q := base.Query()
		moved := false
		for _, k := range []string{"after_events", "after_messages", "snapshot_events", "snapshot_messages"} {
			v, ok := end.Next[k]
			if !ok {
				return fmt.Errorf("export page %d: end record lacks %s", page, k)
			}
			moved = moved || q.Get(k) != fmt.Sprint(v)
			q.Set(k, fmt.Sprint(v))
		}
		if !moved {
			return fmt.Errorf("export page %d: cursor did not advance", page)
		}
		base.RawQuery = q.Encode()
	}
}

// teamAgentOps are the checkout-credential commands, each bridged to the
// MCP tool of the same name (hyphens become underscores).
var teamAgentOps = map[string]bool{"join": true, "members": true, "heartbeat": true, "resume": true, "leave": true, "profile": true, "send": true, "messages": true, "inbox": true, "ack": true,
	"offer": true, "assignment": true, "reserved": true, "accept": true, "decline": true, "withdraw": true, "block": true, "resume-work": true, "cancel": true, "stopped": true, "close-stop": true, "submit": true, "review": true, "handoff": true, "edit": true, "finalize": true}

var teamReadOps = map[string]bool{"members": true, "messages": true, "inbox": true, "assignment": true, "reserved": true}

func teamSessionCmd(args []string) error {
	writes := len(args) > 0 && !teamReadOps[args[0]]
	if len(args) != 4 && !writes || writes && len(args) != 5 {
		return errors.New("usage: aimem teams <operation> PROJECT TEAM request.json [idempotency-key (required for writes)]")
	}
	f, err := os.Open(args[3])
	if err != nil {
		return err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil {
		return err
	}
	if len(raw) > 64<<10 {
		return errors.New("session request exceeds 64 KiB")
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return errors.New("session request must be a JSON object")
	}
	for _, k := range []string{"project", "team", "idempotency_key"} {
		if _, ok := body[k]; ok {
			return fmt.Errorf("%s comes from CLI arguments, not request file", k)
		}
	}
	body["project"], _ = json.Marshal(args[1])
	body["team"], _ = json.Marshal(args[2])
	if writes {
		body["idempotency_key"], _ = json.Marshal(args[4])
	}
	raw, _ = json.Marshal(body)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	out, err := mcp.RunTeamTool(ctx, "team_"+strings.ReplaceAll(args[0], "-", "_"), raw)
	if err != nil {
		return err
	}
	fmt.Println(out)
	if args[0] == "leave" {
		var handle struct {
			SessionID string `json:"session_id"`
		}
		if json.Unmarshal(body["session_id"], &handle.SessionID) == nil {
			noteTeamLeave(".", stateRoot(), handle.SessionID)
		}
	}
	return nil
}

func teamsRequest(args []string) (method, path string, raw []byte, key string, err error) {
	bad := func() (string, string, []byte, string, error) { return "", "", nil, "", errors.New(teamsUsage) }
	if len(args) < 2 {
		return bad()
	}
	path = "/v1/projects/" + url.PathEscape(args[1]) + "/teams"
	switch args[0] {
	case "list":
		if len(args) != 2 {
			return bad()
		}
		return "GET", path, nil, "", nil
	case "show":
		if len(args) != 3 {
			return bad()
		}
		return "GET", path + "/" + url.PathEscape(args[2]), nil, "", nil
	case "events", "export":
		if len(args) < 3 {
			return bad()
		}
		query, err := auditQuery(args[0], args[3:])
		if err != nil {
			return "", "", nil, "", err
		}
		return "GET", path + "/" + url.PathEscape(args[2]) + "/" + args[0] + query, nil, "", nil
	case "create", "configure", "recover", "unmanage", "rebind-token":
		fileIndex := 2
		method = "POST"
		switch args[0] {
		case "configure":
			fileIndex = 3
			method = "PUT"
			if len(args) < 4 {
				return bad()
			}
			path += "/" + url.PathEscape(args[2])
		case "recover", "unmanage", "rebind-token":
			// Operator routes: the admin authority is the local socket itself.
			fileIndex = 4
			if len(args) < 5 {
				return bad()
			}
			switch args[0] {
			case "recover":
				path += "/" + url.PathEscape(args[2]) + "/assignments/" + url.PathEscape(args[3]) + "/recover"
			case "unmanage":
				path += "/" + url.PathEscape(args[2]) + "/tasks/" + url.PathEscape(args[3]) + "/unmanage"
			default:
				path += "/" + url.PathEscape(args[2]) + "/sessions/" + url.PathEscape(args[3]) + "/rebind-token"
			}
		}
		if len(args) < fileIndex+1 || len(args) > fileIndex+2 {
			return bad()
		}
		f, e := os.Open(args[fileIndex])
		if e != nil {
			return "", "", nil, "", e
		}
		defer f.Close()
		raw, e = io.ReadAll(io.LimitReader(f, (64<<10)+1))
		if e != nil {
			return "", "", nil, "", e
		}
		if len(raw) > 64<<10 || !json.Valid(raw) {
			return "", "", nil, "", errors.New("configuration must be valid JSON at most 64 KiB")
		}
		key = uuidv7.New()
		if len(args) == fileIndex+2 {
			key = args[fileIndex+1]
		}
		return method, path, raw, key, nil
	}
	return bad()
}
