package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

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
print one page with next_cursor; use the HTTP API to page further. Agent sessions
and task assignment are not available in this increment.`

func teamsCmd(args []string) error {
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
