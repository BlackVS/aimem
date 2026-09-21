package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/store"
)

func TestUserScopedTokenHTTPAndMCP(t *testing.T) {
	f := newTaskFixture(t)
	raw, _ := json.Marshal(map[string]any{"user_id": f.aliceUser, "label": "token-laptop", "scope": "user", "expires_at": time.Now().Add(time.Hour)})
	w := authedReq(t, f.h, "POST", "/v1/access/tokens", f.admin, string(raw))
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("issue %d: %s", w.Code, w.Body)
	}
	var issued struct {
		Token  access.Token `json:"token"`
		Secret string       `json:"secret"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.Token.Scope != access.ScopeUser || issued.Token.Project != "" || issued.Secret == "" {
		t.Fatalf("wrong token metadata: %+v", issued.Token)
	}
	id, ok := f.s.authenticate(f.env, issued.Secret)
	if !ok {
		t.Fatal("authentication")
	}
	r := httptest.NewRequest("POST", "/mcp", nil)
	r = r.WithContext(withIdentity(r.Context(), id))
	call, only := f.s.MCPPrincipal(r)
	if !only {
		t.Fatal("user token must remain task-tools-only")
	}
	check := func(project, key string, want bool) {
		t.Helper()
		w := authedReq(t, f.h, "GET", "/v1/access/identity?project="+project, issued.Secret, "")
		var identity struct {
			TaskWrite bool              `json:"task_write"`
			Scope     access.TokenScope `json:"scope"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &identity); err != nil || w.Code != 200 || identity.TaskWrite != want || identity.Scope != access.ScopeUser {
			t.Fatalf("identity %d %s", w.Code, w.Body)
		}
		wantCode := 403
		if want {
			wantCode = 201
		}
		w = taskReq(t, f.h, "POST", "/v1/projects/"+project+"/tasks", issued.Secret, key, taskBody)
		if w.Code != wantCode {
			t.Fatalf("http %d %s", w.Code, w.Body)
		}
		code, body, err := call(r.Context(), "POST", "/v1/projects/"+project+"/tasks", map[string]string{"Idempotency-Key": key + "-mcp"}, []byte(taskBody))
		if err != nil || code != wantCode {
			t.Fatalf("mcp %d %s: %v", code, body, err)
		}
	}
	check("alpha", "a", true)
	check("beta", "b-denied", false)
	grantPath := "/v1/projects/beta/access/user/" + f.aliceUser
	if w := authedReq(t, f.h, "PUT", grantPath, f.admin, ""); w.Code != 200 {
		t.Fatalf("grant %d %s", w.Code, w.Body)
	}
	check("beta", "b-granted", true) // same secret, no reissue or MCP refresh
	if w := taskReq(t, f.h, "POST", "/v1/projects/beta/tasks", f.alice, "restricted", taskBody); w.Code != 403 {
		t.Fatal("project token broadened", w.Code)
	}
	if w := taskReq(t, f.h, "POST", "/v1/projects/beta/epics", issued.Secret, "epic", `{"title":"milestone"}`); w.Code != 201 {
		t.Fatalf("epic %d %s", w.Code, w.Body)
	}
	w = taskReq(t, f.h, "POST", "/v1/projects/beta/tasks", issued.Secret, "comment-task", taskBody)
	task := decodeTask(t, w)
	commentPath := "/v1/tasks/" + task.ID + "/comments"
	if w := taskReq(t, f.h, "POST", commentPath, issued.Secret, "comment", `{"body":"evidence"}`); w.Code != 201 {
		t.Fatalf("comment %d %s", w.Code, w.Body)
	}
	beta, err := f.reg.OpenExisting("beta")
	if err != nil {
		t.Fatal(err)
	}
	if err := beta.SetMeta(store.TasksMetaKey, "off"); err != nil {
		t.Fatal(err)
	}
	if w := taskReq(t, f.h, "POST", commentPath, issued.Secret, "off", `{"body":"denied"}`); w.Code != 403 {
		t.Fatal("disabled tasks accepted write")
	}
	if err := beta.SetMeta(store.TasksMetaKey, "on"); err != nil {
		t.Fatal(err)
	}
	if w := authedReq(t, f.h, "DELETE", grantPath, f.admin, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	check("beta", "b-removed", false)
	for _, path := range []string{"/v1/access", "/v1/projects/alpha/docs"} {
		if w := authedReq(t, f.h, "GET", path, issued.Secret, ""); w.Code != 403 {
			t.Fatalf("broadened legacy access %s: %d", path, w.Code)
		}
	}
	if w := authedReq(t, f.h, "DELETE", "/v1/access/tokens/"+issued.Token.ID, f.admin, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	code, body, err := call(r.Context(), "POST", "/v1/projects/alpha/tasks", map[string]string{"Idempotency-Key": "revoked"}, []byte(taskBody))
	if err != nil || code != 403 {
		t.Fatalf("stale MCP identity wrote after revocation: %d %s %v", code, body, err)
	}
	if w := authedReq(t, f.h, "GET", "/v1/access/identity", issued.Secret, ""); w.Code != 401 {
		t.Fatal("revoked authenticated")
	}
}

func TestIssueScopeValidationAndLegacyHTTP(t *testing.T) {
	f := newTaskFixture(t)
	for _, tc := range []struct {
		scope, project string
		want           int
	}{
		{"user", "", 200}, {"project", "alpha", 200}, {"read-only", "", 200},
		{"", "alpha", 200}, {"", "", 200}, {"user", "alpha", 400}, {"read-only", "alpha", 400}, {"project", "", 400}, {"admin", "", 400},
	} {
		body := map[string]any{"user_id": f.aliceUser, "label": "token-test", "project": tc.project, "expires_at": time.Now().Add(time.Hour)}
		if tc.scope != "" {
			body["scope"] = tc.scope
		}
		raw, _ := json.Marshal(body)
		w := authedReq(t, f.h, "POST", "/v1/access/tokens", f.admin, string(raw))
		if w.Code != tc.want {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body)
		}
		if tc.want == 200 {
			var result struct {
				Token access.Token `json:"token"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			want := tc.scope
			if want == "" {
				want = "read-only"
				if tc.project != "" {
					want = "project"
				}
			}
			if string(result.Token.Scope) != want {
				t.Fatalf("scope %q, want %q", result.Token.Scope, want)
			}
		}
		if w := authedReq(t, f.h, "POST", "/v1/access/tokens", f.alice, string(raw)); w.Code != 403 {
			t.Fatal("ordinary self-issuance", w.Code)
		}
	}
	// An explicit unknown field must not accidentally select legacy semantics.
	w := authedReq(t, f.h, "POST", "/v1/access/tokens", f.admin, `{"scpoe":"user"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "unknown field") {
		t.Fatalf("strict decode %d %s", w.Code, w.Body)
	}
}
