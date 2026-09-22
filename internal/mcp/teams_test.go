package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTeamMCPThroughRealHub(t *testing.T) {
	f := newHub(t)
	setup := fmt.Sprintf(`{"name":"Builders","enrollment":[{"user_id":%q,"coordinator":true}]}`, f.aliceID)
	req := httptest.NewRequest("POST", "/v1/projects/alpha/teams", strings.NewReader(setup))
	req.Header.Set("Authorization", "Bearer "+f.env)
	req.Header.Set("Idempotency-Key", "setup")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	args := map[string]any{"project": "alpha", "team": "Builders", "role": "coordinator", "profile": map[string]any{"label": "agent", "platform": "fixture", "platform_version": "1"}, "idempotency_key": "join"}
	call := func(token, name string, a any) (string, bool) {
		return toolText(f.rpc(t, token, "tools/call", map[string]any{"name": name, "arguments": a}))
	}
	for _, token := range []string{f.env, f.reader} {
		if raw, bad := call(token, "team_join", args); !bad {
			t.Fatalf("privilege bypass %s", raw)
		}
	}
	raw, bad := call(f.alice, "team_join", args)
	if bad {
		t.Fatal(raw)
	}
	var out struct {
		Session struct {
			ID         string `json:"id"`
			Team       string `json:"team_id"`
			Generation int    `json:"generation"`
		} `json:"session"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	if out.Session.ID == "" || out.Session.Generation != 1 || strings.Contains(raw, "token_id") {
		t.Fatal(raw)
	}
	handle := map[string]any{"project": "alpha", "team": out.Session.Team, "session_id": out.Session.ID, "generation": 1}
	if raw, bad = call(f.alice, "team_members", handle); bad || !strings.Contains(raw, out.Session.ID) {
		t.Fatal(raw)
	}
	// Exercise message schemas and routing through MCP's authenticated dispatcher.
	send := map[string]any{"project": "alpha", "team": out.Session.Team, "session_id": out.Session.ID, "generation": 1,
		"recipient": map[string]any{"kind": "team"}, "kind": "note", "payload": map[string]any{"text": "fixture message"}, "idempotency_key": "send"}
	raw, bad = call(f.alice, "team_send", send)
	if bad {
		t.Fatal(raw)
	}
	var sent struct {
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(raw), &sent); err != nil || sent.Message.ID == "" {
		t.Fatal(err, raw)
	}
	for _, name := range []string{"team_messages", "team_inbox"} {
		if raw, bad = call(f.alice, name, handle); bad || !strings.Contains(raw, sent.Message.ID) {
			t.Fatal(name, raw)
		}
	}
	ack := map[string]any{"project": "alpha", "team": out.Session.Team, "session_id": out.Session.ID, "generation": 1, "message_ids": []string{sent.Message.ID}, "idempotency_key": "ack"}
	if raw, bad = call(f.alice, "team_ack", ack); bad {
		t.Fatal(raw)
	}
	if raw, bad = call(f.alice, "team_inbox", handle); bad || strings.Contains(raw, sent.Message.ID) {
		t.Fatal(raw)
	}
	send["sender_id"] = "forged"
	if raw, bad = call(f.alice, "team_send", send); !bad {
		t.Fatal("forged sender accepted", raw)
	}
	handle["idempotency_key"] = "resume"
	if raw, bad = call(f.alice, "team_resume", handle); bad || !strings.Contains(raw, `"generation": 2`) {
		t.Fatal(raw)
	}
	handle["idempotency_key"] = "stale"
	if raw, bad = call(f.alice, "team_resume", handle); !bad {
		t.Fatal("stale accepted", raw)
	}
	handle["generation"] = 2
	handle["idempotency_key"] = "leave"
	if raw, bad = call(f.alice, "team_leave", handle); bad || !strings.Contains(raw, `"state": "left"`) {
		t.Fatal(raw)
	}
}

func TestTeamMessageWireContract(t *testing.T) {
	for _, tc := range []struct{ name, args, method, suffix string }{
		{"team_inbox", `"after":42,"limit":10,"wait_seconds":25`, "GET", "/inbox?after=42&generation=1&limit=10&session_id=s&wait_seconds=25"},
		{"team_messages", `"after":0`, "GET", "/messages?after=0&generation=1&session_id=s"},
		{"team_send", `"recipient":{"kind":"team"},"kind":"note","payload":{"text":"hi"},"idempotency_key":"k"`, "POST", "/messages"},
		{"team_ack", `"message_ids":["m"],"idempotency_key":"k"`, "POST", "/ack"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			call := func(_ context.Context, method, path string, headers map[string]string, body []byte) (int, []byte, error) {
				called = true
				if method != tc.method || path != "/v1/projects/alpha/teams/t"+tc.suffix {
					t.Fatal(method, path)
				}
				if method == "POST" && (headers["Idempotency-Key"] != "k" || strings.Contains(string(body), "idempotency_key") || strings.Contains(string(body), `"project"`)) {
					t.Fatal(headers, string(body))
				}
				return 200, []byte(`{"protocol_version":1}`), nil
			}
			raw := `{"project":"alpha","team":"t","session_id":"s","generation":1,` + tc.args + `}`
			if _, err := callTeamTool(context.Background(), call, "", tc.name, json.RawMessage(raw)); err != nil || !called {
				t.Fatal(err)
			}
		})
	}
}

func TestTeamToolProtocolAndStrictFields(t *testing.T) {
	base := `{"project":"alpha","team":"Builders","role":"worker","profile":{"label":"a","platform":"b","platform_version":"1"},"idempotency_key":"join"}`
	sent := 0
	for _, version := range []string{`{}`, `{"protocol_version":2}`, `not-json`} {
		call := func(context.Context, string, string, map[string]string, []byte) (int, []byte, error) {
			sent++
			return 200, []byte(version), nil
		}
		if _, err := callTeamTool(context.Background(), call, "", "team_join", json.RawMessage(base)); err == nil {
			t.Fatal("unsupported accepted")
		}
	}
	before := sent
	call := func(context.Context, string, string, map[string]string, []byte) (int, []byte, error) {
		sent++
		return 200, []byte(`{"protocol_version":1}`), nil
	}
	for _, raw := range []string{`null`, strings.Replace(base, `"role":"worker"`, `"role":"worker","actor":"forged"`, 1), strings.Replace(base, `"label":"a"`, `"label":"a","token_id":"forged"`, 1)} {
		if _, err := callTeamTool(context.Background(), call, "", "team_join", json.RawMessage(raw)); err == nil {
			t.Fatal("forged accepted")
		}
	}
	if sent != before {
		t.Fatal("invalid request sent")
	}
}
