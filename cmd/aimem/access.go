package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const accessUsage = `usage: aimem access list
       aimem access user-add <name>
       aimem access user-set <id> <name> <enabled|disabled>
       aimem access group-add <name>
       aimem access member <add|rm> <group-id> <user-id>
       aimem access grant <add|rm> <project> <user|group> <subject-id>
       aimem access grant-rm-instance <project-instance> <user|group> <subject-id>
       aimem access token-issue <user-id> <label> <project|-> <expiry-RFC3339>
       aimem access token-issue-user <user-id> <label> <expiry-RFC3339>
       aimem access token-revoke <token-id>

Run on the aimem host with its local service running. '-' issues a read-only
ordinary token. token-issue-user follows the user's current project grants.
The issued secret is printed once. Admin tokens are still
managed only by the host-console 'aimem token' command.`

func accessCmd(args []string) error {
	method, path, body, err := accessRequest(args)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(method, "http://aimem"+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client().Do(req)
	if err != nil {
		return fmt.Errorf("%w (is the local aimem service running?)", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, data)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return err
	}
	fmt.Println(pretty.String())
	return nil
}

func accessRequest(args []string) (method, path string, body any, err error) {
	fail := func() (string, string, any, error) { return "", "", nil, fmt.Errorf("%s", accessUsage) }
	if len(args) == 0 {
		return fail()
	}
	q := url.PathEscape
	switch args[0] {
	case "list":
		if len(args) == 1 {
			return "GET", "/v1/access", nil, nil
		}
	case "user-add":
		if len(args) == 2 {
			return "POST", "/v1/access/users", map[string]any{"name": args[1]}, nil
		}
	case "user-set":
		if len(args) == 4 && (args[3] == "enabled" || args[3] == "disabled") {
			return "PUT", "/v1/access/users/" + q(args[1]), map[string]any{"name": args[2], "disabled": args[3] == "disabled"}, nil
		}
	case "group-add":
		if len(args) == 2 {
			return "POST", "/v1/access/groups", map[string]any{"name": args[1]}, nil
		}
	case "member":
		if len(args) == 4 && (args[1] == "add" || args[1] == "rm") {
			method = "PUT"
			if args[1] == "rm" {
				method = "DELETE"
			}
			return method, "/v1/access/groups/" + q(args[2]) + "/members/" + q(args[3]), nil, nil
		}
	case "grant":
		if len(args) == 5 && (args[1] == "add" || args[1] == "rm") && (args[3] == "user" || args[3] == "group") {
			method = "PUT"
			if args[1] == "rm" {
				method = "DELETE"
			}
			return method, "/v1/projects/" + q(args[2]) + "/access/" + args[3] + "/" + q(args[4]), nil, nil
		}
	case "token-issue":
		if len(args) == 5 {
			expires, err := time.Parse(time.RFC3339, args[4])
			if err != nil {
				return "", "", nil, fmt.Errorf("expiry must be RFC3339: %w", err)
			}
			project := args[3]
			if project == "-" {
				project = ""
			}
			return "POST", "/v1/access/tokens", map[string]any{"user_id": args[1], "label": args[2], "project": project, "expires_at": expires}, nil
		}
	case "token-revoke":
		if len(args) == 2 {
			return "DELETE", "/v1/access/tokens/" + q(args[1]), nil, nil
		}
	case "token-issue-user":
		if len(args) == 4 {
			expires, err := time.Parse(time.RFC3339, args[3])
			if err != nil {
				return "", "", nil, fmt.Errorf("expiry must be RFC3339: %w", err)
			}
			return "POST", "/v1/access/tokens", map[string]any{"user_id": args[1], "label": args[2], "scope": "user", "expires_at": expires}, nil
		}
	case "grant-rm-instance":
		if len(args) == 4 && (args[2] == "user" || args[2] == "group") {
			return "DELETE", "/v1/access/grants/" + q(args[1]) + "/" + args[2] + "/" + q(args[3]), nil, nil
		}
	}
	return fail()
}
