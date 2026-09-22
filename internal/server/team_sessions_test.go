package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"aimem/internal/store"
)

func TestSessionHTTPAuthority(t *testing.T) {
	t.Setenv("AIMEM_TEAM_MAX_SESSIONS", "1")
	f := newTaskFixture(t)
	setup := fmt.Sprintf(`{"name":"Build team","enrollment":[{"user_id":%q,"coordinator":true}]}`, f.aliceUser)
	w := taskReq(t, f.h, "POST", "/v1/projects/alpha/teams", f.admin, "setup", setup)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var team store.Team
	json.Unmarshal(w.Body.Bytes(), &team)
	join := `{"role":"coordinator","profile":{"label":"agent","platform":"fixture","platform_version":"unknown"}}`
	base := "/v1/projects/alpha/teams/" + team.ID
	for _, token := range []string{f.admin, f.env, f.writer, f.bob} {
		if w := taskReq(t, f.h, "POST", base+"/join", token, "bad", join); w.Code != 403 {
			t.Fatalf("credential accepted: %d %s", w.Code, w.Body)
		}
	}
	// A socket caller is an operator, never an agent session.
	if w := taskReq(t, f.s.Handler(), "POST", base+"/join", "", "socket", join); w.Code != 403 {
		t.Fatal(w.Code)
	}
	w = taskReq(t, f.h, "POST", "/v1/projects/alpha/teams/Build%20team/join", f.alice, "join", join)
	if w.Code != 201 {
		t.Fatalf("join %d %s", w.Code, w.Body)
	}
	var out struct {
		Session      teamMemberView `json:"session"`
		Protocol     int            `json:"protocol_version"`
		Instructions string         `json:"instructions"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if out.Protocol != 1 || out.Session.TeamID != team.ID || out.Session.Generation != 1 || !strings.Contains(out.Instructions, "wait") {
		t.Fatalf("join contract %+v", out)
	}
	if strings.Contains(w.Body.String(), "token_id") || strings.Contains(w.Body.String(), "user_id") {
		t.Fatal("internal bindings leaked")
	}
	if full := taskReq(t, f.h, "POST", base+"/join", f.alice, "over-quota", strings.Replace(join, "coordinator", "worker", 1)); full.Code != 409 {
		t.Fatal("operator quota ignored", full.Code)
	}
	if replay := taskReq(t, f.h, "POST", base+"/join", f.alice, "join", join); replay.Code != 201 {
		t.Fatal("quota blocked accepted retry", replay.Code)
	}
	handle := fmt.Sprintf(`{"session_id":%q,"generation":1}`, out.Session.ID)
	accessDB, err := f.s.openAccess(false)
	if err != nil {
		t.Fatal(err)
	}
	_, otherToken, err := accessDB.Issue("admin", f.aliceUser, "other-agent", f.alphaInstance, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if denied := taskReq(t, f.h, "POST", base+"/resume", otherToken, "wrong-token", handle); denied.Code != 403 {
		t.Fatal("session token binding", denied.Code)
	}
	roster := fmt.Sprintf("%s/members?session_id=%s&generation=1", base, out.Session.ID)
	if w = taskReq(t, f.h, "GET", roster, f.alice, "", ""); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = taskReq(t, f.h, "GET", roster+"&unexpected=1", f.alice, "", ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w = taskReq(t, f.h, "POST", base+"/resume", f.alice, "resume", handle); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = taskReq(t, f.h, "POST", base+"/resume", f.alice, "resume", handle); w.Code != 200 {
		t.Fatal("resume receipt", w.Body.String())
	}
	if w = taskReq(t, f.h, "POST", base+"/resume", f.alice, "stale", handle); w.Code != 409 {
		t.Fatal(w.Code)
	}
	if w = taskReq(t, f.h, "GET", roster, f.alice, "", ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
	for _, bad := range []string{`null`, join + ` {}`, `{"role":"worker","actor":"forged"}`, `{"role":"worker","profile":{"label":"x","platform":"x","platform_version":"x","token_id":"forged"}}`} {
		if w = taskReq(t, f.h, "POST", base+"/join", f.alice, "bad", bad); w.Code != 400 {
			t.Fatalf("bad accepted: %d %s", w.Code, w.Body)
		}
	}
	if w = taskReq(t, f.h, "POST", strings.Replace(base, "alpha", "beta", 1)+"/join", f.alice, "cross", join); w.Code != 403 {
		t.Fatal(w.Code)
	}
	acc, err := f.s.openAccess(false)
	if err != nil {
		t.Fatal(err)
	}
	if err = acc.SetGrant("admin", f.alphaInstance, "user", f.aliceUser, false); err != nil {
		t.Fatal(err)
	}
	for _, req := range []struct{ method, path, key, body string }{{"POST", base + "/join", "join", join}, {"POST", base + "/resume", "resume", handle}, {"GET", roster, "", ""}} {
		if w = taskReq(t, f.h, req.method, req.path, f.alice, req.key, req.body); w.Code != 403 {
			t.Fatalf("revoked grant replay/read %d", w.Code)
		}
	}
	if err = acc.SetGrant("admin", f.alphaInstance, "user", f.aliceUser, true); err != nil {
		t.Fatal(err)
	}
	// Enrollment revocation also rejects an accepted join's receipt.
	w = taskReq(t, f.h, "PUT", base, f.admin, "revoke", `{"name":"Build team","expected_revision":1,"enrollment":[]}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = taskReq(t, f.h, "POST", base+"/join", f.alice, "join", join); w.Code != http.StatusForbidden {
		t.Fatal(w.Code)
	}
	if err = acc.Revoke("admin", f.aliceTokenID); err != nil {
		t.Fatal(err)
	}
	if w = taskReq(t, f.h, "POST", base+"/join", f.alice, "join", join); w.Code != 401 {
		t.Fatal("revoked token", w.Code)
	}
}

func TestSessionOperatorPolicy(t *testing.T) {
	t.Setenv("AIMEM_TEAM_MAX_SESSIONS", "2")
	t.Setenv("AIMEM_TEAM_SUSPECT_SECONDS", "300")
	max, timeout, err := sessionPolicy()
	if err != nil || max != 2 || int(timeout.Seconds()) != 300 {
		t.Fatal(max, timeout, err)
	}
	t.Setenv("AIMEM_TEAM_MAX_SESSIONS", "not-a-number")
	if _, _, err = sessionPolicy(); err == nil {
		t.Fatal("invalid policy accepted")
	}
}
