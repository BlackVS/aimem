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
	"time"

	"aimem/internal/mcp"
	"aimem/internal/uuidv7"
)

const teamsUsage = `usage: aimem teams list <project>
       aimem teams show <project> <team-id>
       aimem teams events <project> <team-id>
       aimem teams create <project> <config.json> [idempotency-key]
       aimem teams configure <project> <team-id> <config.json> [idempotency-key]

Run on the hub host as its local operator. Configuration JSON contains name,
description and enrollment [{user_id,coordinator}]. configure also requires
expected_revision. Configuration replaces all fields; omitted enrollment clears
it. Save/reuse an explicit idempotency key to retry an uncertain write. list/events
print one page with next_cursor; use the HTTP API to page further.

Agent commands (from a configured checkout, using its task credential):
  aimem teams <join|members|heartbeat|resume|leave|profile> PROJECT TEAM request.json [KEY]
KEY is required for writes. join accepts a team name or ID; other commands use
the returned ID. See docs/TEAM-SETUP.md. Task assignment is not available yet.`

func teamsCmd(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "join", "members", "heartbeat", "resume", "leave", "profile":
			return teamSessionCmd(args)
		}
	}
	method, path, raw, key, err := teamsRequest(args)
	if err != nil {
		return err
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

func teamSessionCmd(args []string) error {
	writes := len(args) > 0 && args[0] != "members"
	if len(args) != 4 && !writes || writes && len(args) != 5 {
		return errors.New("usage: aimem teams <join|members|heartbeat|resume|leave|profile> PROJECT TEAM request.json [idempotency-key (required for writes)]")
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := mcp.RunTeamTool(ctx, "team_"+args[0], raw)
	if err != nil {
		return err
	}
	fmt.Println(out)
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
	case "show", "events":
		if len(args) != 3 {
			return bad()
		}
		path += "/" + url.PathEscape(args[2])
		if args[0] == "events" {
			path += "/events"
		}
		return "GET", path, nil, "", nil
	case "create", "configure":
		fileIndex := 2
		method = "POST"
		if args[0] == "configure" {
			fileIndex = 3
			method = "PUT"
			if len(args) < 4 {
				return bad()
			}
			path += "/" + url.PathEscape(args[2])
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
