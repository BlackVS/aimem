package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"

	"aimem/internal/store"
)

func teamProfileSchema() map[string]any {
	model := objSchema(map[string]any{"provider": prop("string", "reported provider or unknown"), "id": prop("string", "reported model identifier or unknown"), "version": prop("string", "reported version or unknown"), "source": propEnum("declaration source, never infer from client", "runtime_reported", "operator_configured", "agent_reported", "unknown"), "observed_at": prop("string", "optional RFC3339 observation time")})
	return objSchema(map[string]any{"label": prop("string", "readable agent label"), "platform": prop("string", "agent client name"), "platform_version": prop("string", "client version or unknown"), "model": model, "capabilities": map[string]any{"type": "array", "items": prop("string", "declared capability")}}, "label", "platform", "platform_version")
}

var teamToolDefs = func() []map[string]any {
	defs := []map[string]any{}
	for _, op := range []string{"join", "members", "heartbeat", "resume", "leave", "profile"} {
		props := map[string]any{"project": prop("string", "project, default current checkout"), "team": prop("string", "team ID; join also accepts exact readable name")}
		required := []string{"team"}
		if op != "members" {
			props["idempotency_key"] = prop("string", "unique retry key; reuse only for identical request")
			required = append(required, "idempotency_key")
		}
		if op == "join" {
			props["role"] = propEnum("requested role; coordinator needs prior enrollment eligibility", "worker", "reviewer", "coordinator")
			props["profile"] = teamProfileSchema()
			required = append(required, "role", "profile")
		} else {
			props["session_id"] = prop("string", "returned session ID")
			props["generation"] = prop("integer", "current session generation")
			required = append(required, "session_id", "generation")
		}
		if op == "profile" {
			props["profile"] = teamProfileSchema()
			props["expected_profile_revision"] = prop("integer", "profile revision being replaced")
			required = append(required, "profile", "expected_profile_revision")
		}
		if op == "heartbeat" {
			props["availability"] = propEnum("availability is not model progress", "available", "unavailable")
			required = append(required, "availability")
		}
		if op == "members" {
			props["after"] = prop("string", "next_cursor from previous page")
			props["limit"] = prop("integer", "page size 1-100")
		}
		defs = append(defs, map[string]any{"name": "team_" + op, "description": "Team " + op + " using your project task credential. Workers wait for coordinator assignments; never pick backlog tasks independently while joined. Messaging and assignments are not available yet. Resume fences old handles but cannot stop local commands; reconcile old execution first. A retry returns its original result, which may have an old generation.", "inputSchema": objSchema(props, required...)})
	}
	return defs
}()

// RunTeamTool is the CLI entry point; it uses the same checkout credential
// resolution as stdio MCP and never the trusted operator socket.
func RunTeamTool(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	call, err := localTaskCaller()
	if err != nil {
		return "", err
	}
	return callTeamTool(ctx, call, "", name, raw)
}

func callTeamTool(ctx context.Context, call TaskCallFunc, defaultProject, name string, raw json.RawMessage) (string, error) {
	var schema map[string]any
	for _, d := range teamToolDefs {
		if d["name"] == name {
			schema = d["inputSchema"].(map[string]any)
			break
		}
	}
	if schema == nil {
		return "", errors.New("unknown team tool")
	}
	var fields map[string]json.RawMessage
	if len(raw) > 64<<10 || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return "", errors.New("team arguments must be a JSON object at most 64 KiB")
	}
	props := schema["properties"].(map[string]any)
	for k := range fields {
		if _, ok := props[k]; !ok {
			return "", fmt.Errorf("unknown team argument %q", k)
		}
	}
	for _, k := range schema["required"].([]string) {
		if len(fields[k]) == 0 || string(fields[k]) == "null" {
			return "", fmt.Errorf("%s required", k)
		}
	}
	var a struct {
		Project                 string             `json:"project"`
		Team                    string             `json:"team"`
		Key                     string             `json:"idempotency_key"`
		Role                    string             `json:"role"`
		Profile                 *store.TeamProfile `json:"profile"`
		SessionID               string             `json:"session_id"`
		Generation              int64              `json:"generation"`
		ExpectedProfileRevision int64              `json:"expected_profile_revision"`
		Availability            string             `json:"availability"`
		After                   string             `json:"after"`
		Limit                   int                `json:"limit"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return "", err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return "", errors.New("expected one JSON object")
	}
	if a.Project == "" {
		a.Project = defaultProject
	}
	if a.Project == "" || a.Team == "" {
		return "", errors.New("project and team are required")
	}
	op := name[len("team_"):]
	method := "POST"
	path := "/v1/projects/" + url.PathEscape(a.Project) + "/teams/" + url.PathEscape(a.Team) + "/" + op
	headers := map[string]string{}
	var body []byte
	if op == "members" {
		method = "GET"
		q := url.Values{"session_id": {a.SessionID}, "generation": {strconv.FormatInt(a.Generation, 10)}}
		if a.After != "" {
			q.Set("after", a.After)
		}
		if a.Limit != 0 {
			q.Set("limit", strconv.Itoa(a.Limit))
		}
		path += "?" + q.Encode()
	} else {
		if a.Key == "" {
			return "", errors.New("idempotency_key required")
		}
		headers["Idempotency-Key"] = a.Key
		if op == "join" {
			body, _ = json.Marshal(map[string]any{"role": a.Role, "profile": a.Profile})
		} else {
			body, _ = json.Marshal(store.TeamSessionCommand{SessionID: a.SessionID, Generation: a.Generation, Profile: a.Profile, ExpectedProfileRevision: a.ExpectedProfileRevision, Availability: a.Availability})
		}
	}
	status, resp, err := call(ctx, method, path, headers, body)
	if err != nil {
		return "", err
	}
	var result struct {
		Version int    `json:"protocol_version"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(resp, &result) != nil {
		return "", errors.New("hub team protocol unavailable or malformed; upgrade hub, do not emulate with task writes")
	}
	if status/100 != 2 {
		return "", fmt.Errorf("team request HTTP %d: %s", status, result.Error)
	}
	if result.Version != 1 {
		return "", errors.New("unsupported team protocol; upgrade compatible client/hub, do not emulate with task writes")
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, resp, "", "  "); err != nil {
		return "", err
	}
	return pretty.String(), nil
}
