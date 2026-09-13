package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aimem/internal/access"
)

func TestAccessManagementAndOrdinaryTokenBoundary(t *testing.T) {
	s, reg := testServer(t)
	if _, err := reg.Open("alpha"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Open("beta"); err != nil {
		t.Fatal(err)
	}
	writer, wd, err := NewTokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	admin, ad, err := NewTokenSecret()
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveTokens(reg.Root(), []TokenEntry{{Name: "old-writer", Role: "writer", SHA256: wd}, {Name: "host-admin", Role: "admin", SHA256: ad}}); err != nil {
		t.Fatal(err)
	}
	h := s.authWrapper("host-env-admin", s.Handler())
	for _, token := range []string{writer, ""} {
		w := authedReq(t, h, "POST", "/v1/access/users", token, `{"name":"blocked"}`)
		if w.Code != 401 && w.Code != 403 {
			t.Fatalf("non-admin managed users: %d", w.Code)
		}
	}
	request := func(method, path string, body any) string {
		t.Helper()
		raw, _ := json.Marshal(body)
		w := authedReq(t, h, method, path, admin, string(raw))
		if w.Code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
		}
		return w.Body.String()
	}
	var u access.User
	if err := json.Unmarshal([]byte(request("POST", "/v1/access/users", map[string]any{"name": "Alice"})), &u); err != nil {
		t.Fatal(err)
	}
	var g access.Group
	if err := json.Unmarshal([]byte(request("POST", "/v1/access/groups", map[string]any{"name": "Developers"})), &g); err != nil {
		t.Fatal(err)
	}
	request("PUT", "/v1/access/groups/"+g.ID+"/members/"+u.ID, nil)
	request("PUT", "/v1/projects/alpha/access/group/"+g.ID, nil)
	var issued struct {
		Token  access.Token `json:"token"`
		Secret string       `json:"secret"`
	}
	issue := map[string]any{"user_id": u.ID, "label": "agent", "project": "alpha", "expires_at": time.Now().Add(time.Hour)}
	if err := json.Unmarshal([]byte(request("POST", "/v1/access/tokens", issue)), &issued); err != nil {
		t.Fatal(err)
	}
	if issued.Secret == "" {
		t.Fatal("missing secret")
	}
	for _, p := range []string{"alpha", "beta"} {
		w := authedReq(t, h, "GET", "/v1/access/identity?project="+p, issued.Secret, "")
		if w.Code != 200 {
			t.Fatalf("identity: %d %s", w.Code, w.Body)
		}
		var v struct {
			Write bool `json:"task_write"`
		}
		json.Unmarshal(w.Body.Bytes(), &v)
		if v.Write != (p == "alpha") {
			t.Fatalf("write permission for %s: %s", p, w.Body)
		}
	}
	// New credentials cannot use legacy APIs or /mcp's trusted transport as
	// an alternate write path. The task surface will be added separately.
	for _, path := range []string{"/v1/projects", "/v1/access", "/v1/logs", "/mcp", "/v1/projects/alpha/memories"} {
		w := authedReq(t, h, "POST", path, issued.Secret, `{}`)
		if w.Code != 403 {
			t.Fatalf("ordinary escaped via %s: %d", path, w.Code)
		}
	}
	for _, role := range []string{"admin", "writer"} {
		issue["role"] = role
		raw, _ := json.Marshal(issue)
		w := authedReq(t, h, "POST", "/v1/access/tokens", admin, string(raw))
		if w.Code != 400 {
			t.Fatalf("remote role accepted: %d", w.Code)
		}
	}
	delete(issue, "role")
	w := authedReq(t, h, "POST", "/v1/access/tokens", admin, `{"user_id":"x"} {"role":"admin"}`)
	if w.Code != 400 {
		t.Fatal("multiple objects accepted")
	}
	snapshot := request("GET", "/v1/access", nil)
	if strings.Contains(snapshot, issued.Secret) || strings.Contains(snapshot, "digest") {
		t.Fatal("snapshot leaked secret")
	}
	request("DELETE", "/v1/access/groups/"+g.ID+"/members/"+u.ID, nil)
	w = authedReq(t, h, "GET", "/v1/access/identity?project=alpha", issued.Secret, "")
	if !strings.Contains(w.Body.String(), `"task_write":false`) {
		t.Fatal("removed membership still writes", w.Body)
	}
	request("DELETE", "/v1/access/tokens/"+issued.Token.ID, nil)
	if w = authedReq(t, h, "GET", "/v1/access/identity", issued.Secret, ""); w.Code != 401 {
		t.Fatalf("revoked: %d", w.Code)
	}
	for _, token := range []string{admin, "host-env-admin"} {
		if w = authedReq(t, h, "GET", "/v1/access", token, ""); w.Code != 200 {
			t.Fatal("host admin lost access")
		}
	}
	if w = authedReq(t, h, "GET", "/v1/projects", writer, ""); w.Code != 200 {
		t.Fatal("legacy writer regression")
	}
}

func TestAccessRejectsMissingProjectAndInvalidCredential(t *testing.T) {
	s, reg := testServer(t)
	h := s.authWrapper("admin", s.Handler())
	w := authedReq(t, h, "PUT", "/v1/projects/missing/access/user/x", "admin", "")
	if w.Code == 200 {
		t.Fatal("missing project accepted")
	}
	if strings.Contains(w.Body.String(), "lstat") || strings.Contains(w.Body.String(), "projects") {
		t.Fatal("grant response exposes filesystem details", w.Body)
	}
	w = authedReq(t, h, "POST", "/v1/access/tokens", "admin", `{"project":"missing"}`)
	if w.Code != 400 || strings.Contains(w.Body.String(), "lstat") || strings.Contains(w.Body.String(), "projects") {
		t.Fatal("token issue response exposes filesystem details", w.Body)
	}
	if _, err := reg.OpenExisting("missing"); err == nil {
		t.Fatal("access grant created project")
	}
	// Failed ordinary authentication does not fall back to configured admin.
	if w := authedReq(t, h, "GET", "/v1/access", "aimem_user_"+strings.Repeat("0", 64), ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("invalid ordinary token: %d", w.Code)
	}
}

func TestAccessListingSurvivesUninitializedAndDamagedProjects(t *testing.T) {
	s, reg := testServer(t)
	for _, project := range []string{"good", "unused", "damaged"} {
		if _, err := reg.Open(project); err != nil {
			t.Fatal(err)
		}
	}
	id, err := reg.ProjectAccessID("good")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(reg.Root(), "projects", "husk"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(reg.Root(), "projects", "damaged", "access-id"), []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	h := s.authWrapper("admin", s.Handler())
	if w := authedReq(t, h, "POST", "/v1/access/users", "admin", `{"name":"Alice"}`); w.Code != 200 {
		t.Fatal(w.Body)
	}
	w := authedReq(t, h, "GET", "/v1/access", "admin", "")
	var result struct {
		Users    []access.User     `json:"users"`
		Projects map[string]string `json:"projects"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Users) != 1 || result.Users[0].Name != "Alice" || len(result.Projects) != 1 || result.Projects[id] != "good" {
		t.Fatalf("listing unavailable or incomplete: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(reg.Root(), "projects", "unused", "access-id")); !os.IsNotExist(err) {
		t.Fatalf("listing created project identity: %v", err)
	}
}
