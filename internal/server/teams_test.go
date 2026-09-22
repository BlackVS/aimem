package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"aimem/internal/store"
)

func TestTeamAdminHTTP(t *testing.T) {
	f := newTaskFixture(t)
	var logs bytes.Buffer
	f.s.log = slog.New(slog.NewJSONHandler(&logs, nil))
	path := "/v1/projects/alpha/teams"
	body := fmt.Sprintf(`{"name":"Compiler team","description":"build work","enrollment":[{"user_id":%q,"coordinator":true}]}`, f.aliceUser)
	for _, token := range []string{f.alice, f.bob, f.writer, "invalid-secret", ""} {
		w := taskReq(t, f.h, "POST", path, token, "denied", body)
		if w.Code != 401 && w.Code != 403 {
			t.Fatalf("unauthorized %d", w.Code)
		}
	}
	w := taskReq(t, f.h, "POST", path, f.admin, "create", body)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var team store.Team
	if err := json.Unmarshal(w.Body.Bytes(), &team); err != nil {
		t.Fatal(err)
	}
	if team.ProjectInstance != f.alphaInstance || len(team.Enrollment) != 1 {
		t.Fatalf("team %+v", team)
	}
	w2 := taskReq(t, f.h, "POST", path, f.admin, "create", body)
	if w2.Code != 201 || w2.Body.String() != w.Body.String() {
		t.Fatal("receipt mismatch")
	}
	for _, bad := range []string{`{"name":"bad","actor":"forged"}`, `{"name":"bad"} {}`, `{"name":"bad","enrollment":[{"user_id":"missing"}]}`, `null`} {
		w := taskReq(t, f.h, "POST", path, f.admin, "bad", bad)
		if w.Code != 400 {
			t.Fatalf("invalid accepted %s: %d", bad, w.Code)
		}
	}
	update := `{"name":"Renamed","expected_revision":1}`
	w = taskReq(t, f.h, "PUT", path+"/"+team.ID, f.admin, "update", update)
	if w.Code != 200 {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	var up store.Team
	json.Unmarshal(w.Body.Bytes(), &up)
	if len(up.Enrollment) != 0 || up.Revision != 2 {
		t.Fatalf("replacement %+v", up)
	}
	w = taskReq(t, f.h, "PUT", path+"/"+team.ID, f.admin, "stale", update)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"current"`) {
		t.Fatalf("stale %d %s", w.Code, w.Body)
	}
	for _, suffix := range []string{"", "/" + team.ID, "/" + team.ID + "/events"} {
		for _, token := range []string{f.alice, f.bob, f.writer} {
			if w := taskReq(t, f.h, "GET", path+suffix, token, "", ""); w.Code != 403 {
				t.Fatalf("read exposure %s %d", suffix, w.Code)
			}
		}
	}
	w = taskReq(t, f.h, "GET", path+"/"+team.ID+"/events?limit=1", f.admin, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var page struct {
		Events []store.TeamEvent `json:"events"`
		Next   int64             `json:"next_cursor"`
	}
	json.Unmarshal(w.Body.Bytes(), &page)
	if len(page.Events) != 1 || page.Next == 0 || page.Events[0].RequestID == "" || page.Events[0].Actor.Kind != "admin" {
		t.Fatalf("audit %+v", page)
	}
	w = taskReq(t, f.h, "GET", fmt.Sprintf("%s/%s/events?after=%d", path, team.ID, page.Next), f.admin, "", "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, p := range []string{"missing", store.UserScopeProject, "group-test"} {
		w := taskReq(t, f.h, "POST", "/v1/projects/"+p+"/teams", f.admin, "p", body)
		if w.Code != 400 && w.Code != 404 {
			t.Fatalf("scope %s %d", p, w.Code)
		}
	}
	d, _ := f.reg.OpenExisting("alpha")
	d.SetMeta(store.TasksMetaKey, "off")
	if w := taskReq(t, f.h, "POST", path, f.admin, "create", body); w.Code != 403 {
		t.Fatalf("disabled replay %d", w.Code)
	}
	if strings.Contains(logs.String(), "invalid-secret") || strings.Contains(logs.String(), "forged") || strings.Contains(logs.String(), "build work") {
		t.Fatal("sensitive input logged")
	}
	if !strings.Contains(logs.String(), `"status":401`) || !strings.Contains(logs.String(), `"status":400`) {
		t.Fatalf("missing refusal logs %s", logs.String())
	}
}
