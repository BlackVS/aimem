package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aimem/internal/teamguide"
	"aimem/internal/teamsetup"
)

// team_context on the hub facade, behind the real bearer gate: every
// ordinary token reads the same complete guidance, including a token whose
// user holds no project grant and no team session; the gate still refuses
// a request without a valid token.
func TestTeamContextOnTheHubUnderOrdinaryAuthentication(t *testing.T) {
	f := newHub(t)
	u, err := teamguide.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	want, err := u.Role("coordinator", "")
	if err != nil {
		t.Fatal(err)
	}
	for name, token := range map[string]string{"project token": f.alice, "user token": f.reader, "user without grants": f.stranger, "admin": f.env} {
		listed := false
		for _, n := range toolNames(f.rpc(t, token, "tools/list", nil)) {
			listed = listed || n == "team_context"
		}
		if !listed {
			t.Errorf("%s: team_context not listed", name)
		}
		text, isErr := toolText(f.rpc(t, token, "tools/call", map[string]any{"name": "team_context", "arguments": map[string]any{"role": "coordinator"}}))
		if isErr || text != want {
			t.Errorf("%s: role read differs from the embedded unit (error %v)", name, isErr)
		}
	}
	// The index, like every read, ends with its terminator.
	text, isErr := toolText(f.rpc(t, f.stranger, "tools/call", map[string]any{"name": "team_context", "arguments": map[string]any{}}))
	if isErr || !strings.HasSuffix(text, u.Terminator("index", "")+"\n") {
		t.Errorf("index read: %v %.200s", isErr, text[max(0, len(text)-200):])
	}
	// Unknown and forbidden arguments are refused with a bounded, useful error.
	for args, wantErr := range map[string]string{
		`{"section":"example/nope"}`:                      "example/offer",
		`{"role":"admin"}`:                                "coordinator or worker",
		`{"url":"https://example.com"}`:                   `unknown field "url"`,
		`{"section":"../../etc/passwd"}`:                  "section must be a section id",
		`{"role":"worker","command":"ls"}`:                `unknown field "command"`,
		`{"section":"` + strings.Repeat("a", 5000) + `"}`: "at most 4 KiB",
	} {
		text, isErr := toolText(f.rpc(t, f.stranger, "tools/call", map[string]any{"name": "team_context", "arguments": json.RawMessage(args)}))
		if !isErr || !strings.Contains(text, wantErr) || len(text) > 1024 {
			t.Errorf("%.40s: want a bounded error with %q, got %v %q", args, wantErr, isErr, text)
		}
	}
	// The gate is unchanged: no token, or a wrong one, never reaches the tool.
	for _, token := range []string{"", "not-a-token"} {
		raw := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"team_context","arguments":{"role":"worker"}}}`
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(raw))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		f.h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "aimem team guidance") {
			t.Errorf("token %q: HTTP %d %.80s", token, w.Code, w.Body)
		}
	}
	// Tasks-only filtering is unchanged for everything else.
	text, isErr = toolText(f.rpc(t, f.stranger, "tools/call", map[string]any{"name": "recall_memory", "arguments": map[string]any{"query": "x", "project": "alpha"}}))
	if !isErr || !strings.Contains(text, "task tools only") {
		t.Errorf("legacy tool by name: %v %q", isErr, text)
	}
}

// The local facade lists and serves team_context whatever the project's
// task state, and answers without its hub, credential or checkout: the
// callers below would fail the test if anything reached them.
func TestTeamContextOnTheLocalFacadeNeedsNothingElse(t *testing.T) {
	for _, state := range []string{taskStateEnabled, taskStateDisabled, taskStateUnknown} {
		s := &srv{
			api:       &http.Client{Transport: failTransport{t}},
			project:   "some-other-project",
			taskState: state,
			local:     &localCheckout{dir: t.TempDir(), root: t.TempDir()},
			taskSetup: func() (TaskCallFunc, error) { t.Fatal("task credential resolved"); return nil, nil },
		}
		var list struct {
			Result map[string]any `json:"result"`
		}
		json.Unmarshal(s.handle(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)), &list)
		if !strings.Contains(fmt.Sprint(toolNames(list.Result)), "team_context") {
			t.Errorf("%s: not listed", state)
		}
		var call struct {
			Result map[string]any `json:"result"`
		}
		json.Unmarshal(s.handle(t.Context(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"team_context","arguments":{"role":"worker"}}}`)), &call)
		text, isErr := toolText(call.Result)
		u, _ := teamguide.Embedded()
		if isErr || !strings.HasSuffix(text, u.Terminator("role worker", "")+"\n") {
			t.Errorf("%s: %v %.200s", state, isErr, text)
		}
	}
}

type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("team_context reached the network: %s", r.URL)
	return nil, fmt.Errorf("no network")
}

// The guidance names the hub release the setup core requires.
func TestTeamGuidanceMinimumHubMatchesTheSetupProtocol(t *testing.T) {
	p := teamsetup.Protocol
	if want := fmt.Sprintf("%d.%d.%d", p[0], p[1], p[2]); teamguide.MinHub != want {
		t.Fatalf("teamguide.MinHub %s, teamsetup.Protocol %s", teamguide.MinHub, want)
	}
}
