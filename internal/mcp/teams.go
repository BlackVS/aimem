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
	for _, op := range []string{"join", "members", "heartbeat", "resume", "leave", "profile", "send", "messages", "inbox", "ack"} {
		props := map[string]any{"project": prop("string", "project, default current checkout"), "team": prop("string", "team ID; join also accepts exact readable name")}
		required := []string{"team"}
		if !teamReadOperation(op) {
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
		if op == "messages" || op == "inbox" {
			props["after"] = prop("integer", "nonnegative next_cursor; use 0 on inbox reconnect to redeliver all unacknowledged messages")
			props["limit"] = prop("integer", "page size 1-100, default 50")
		}
		if op == "inbox" {
			props["wait_seconds"] = prop("integer", "bounded wait 0-25 seconds, default 0; an empty inbox does not complete work")
		}
		if op == "send" {
			props["recipient"] = objSchema(map[string]any{"kind": propEnum("inbox routing, not privacy", "member", "team"), "id": prop("string", "recipient session ID for member; absent for team")}, "kind")
			props["kind"] = propEnum("typed message, never an execution assignment", "question", "answer", "note", "progress", "blocker", "review_feedback")
			props["payload"] = objSchema(map[string]any{"text": prop("string", "message text; total content at most 32 KiB, do not send secrets"), "refs": map[string]any{"type": "array", "items": taskRefProp()}, "deadline": prop("string", "optional question-only RFC3339 deadline")}, "text")
			props["task_id"] = prop("string", "optional task in this project")
			props["reply_to"] = prop("string", "message ID in this team; answers require a question")
			required = append(required, "recipient", "kind", "payload")
		}
		if op == "ack" {
			props["message_ids"] = map[string]any{"type": "array", "items": prop("string", "delivered message ID"), "minItems": 1, "maxItems": 100}
			required = append(required, "message_ids")
		}
		defs = append(defs, map[string]any{"name": "team_" + op, "description": "Team " + op + " using your project task credential. All messages are team-visible; recipient means inbox routing. Reading is a delivery attempt; explicit ack records receipt, not answer or completion. Use inbox reads/bounded waits; notifications do not prove model receipt. Workers wait for coordinator assignments; never pick backlog tasks independently while joined. Assignment, execution and management tools exist (team_offer through team_finalize), but the hub reports workflow_ready:false until lifecycle events reach the inbox; messages cannot assign work. Resume fences old handles but cannot stop local commands; reconcile old execution first. A retry returns its original result, which may have an old generation.", "inputSchema": objSchema(props, required...)})
	}
	return defs
}()

func teamReadOperation(op string) bool { return op == "members" || op == "messages" || op == "inbox" }

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
	if tool, ok := teamWorkToolByName(name); ok {
		return callTeamWorkTool(ctx, call, defaultProject, tool, raw)
	}
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
		After                   json.RawMessage    `json:"after"`
		Limit                   int                `json:"limit"`
		WaitSeconds             int                `json:"wait_seconds"`
		MessageIDs              []string           `json:"message_ids"`
		store.TeamMessageContent
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
	if op == "send" {
		path = "/v1/projects/" + url.PathEscape(a.Project) + "/teams/" + url.PathEscape(a.Team) + "/messages"
	}
	headers := map[string]string{}
	var body []byte
	if teamReadOperation(op) {
		method = "GET"
		q := url.Values{"session_id": {a.SessionID}, "generation": {strconv.FormatInt(a.Generation, 10)}}
		if len(a.After) != 0 {
			if op == "members" {
				var after string
				if err := json.Unmarshal(a.After, &after); err != nil {
					return "", err
				}
				q.Set("after", after)
			} else {
				var after int64
				if err := json.Unmarshal(a.After, &after); err != nil {
					return "", err
				}
				q.Set("after", strconv.FormatInt(after, 10))
			}
		}
		if _, present := fields["limit"]; present {
			q.Set("limit", strconv.Itoa(a.Limit))
		}
		if op == "inbox" {
			q.Set("wait_seconds", strconv.Itoa(a.WaitSeconds))
		}
		path += "?" + q.Encode()
	} else {
		if a.Key == "" {
			return "", errors.New("idempotency_key required")
		}
		headers["Idempotency-Key"] = a.Key
		if op == "send" {
			body, _ = json.Marshal(struct {
				store.TeamSessionHandle
				store.TeamMessageContent
			}{store.TeamSessionHandle{SessionID: a.SessionID, Generation: a.Generation}, a.TeamMessageContent})
		} else if op == "ack" {
			body, _ = json.Marshal(struct {
				store.TeamSessionHandle
				MessageIDs []string `json:"message_ids"`
			}{store.TeamSessionHandle{SessionID: a.SessionID, Generation: a.Generation}, a.MessageIDs})
		} else if op == "join" {
			body, _ = json.Marshal(map[string]any{"role": a.Role, "profile": a.Profile})
		} else {
			body, _ = json.Marshal(store.TeamSessionCommand{SessionID: a.SessionID, Generation: a.Generation, Profile: a.Profile, ExpectedProfileRevision: a.ExpectedProfileRevision, Availability: a.Availability})
		}
	}
	return forwardTeamRequest(ctx, call, method, path, headers, body)
}
