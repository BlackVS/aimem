package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"aimem/internal/store"
)

// An ordinary member lists the teams enrolling it before any session
// exists; the listing names no other member and creates nothing.
func TestMyTeamsListing(t *testing.T) {
	f := newTaskFixture(t)
	mine := "/v1/projects/alpha/teams/mine"
	// Nothing enrolled yet: an empty listing, not a refusal.
	w := taskReq(t, f.h, "GET", mine, f.alice, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"teams":[]`) {
		t.Fatalf("empty listing %d %s", w.Code, w.Body)
	}
	setup := fmt.Sprintf(`{"name":"Build team","description":"builds","enrollment":[{"user_id":%q,"coordinator":true}]}`, f.aliceUser)
	if w = taskReq(t, f.h, "POST", "/v1/projects/alpha/teams", f.admin, "setup", setup); w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var team store.Team
	json.Unmarshal(w.Body.Bytes(), &team)
	other := fmt.Sprintf(`{"name":"Other team","enrollment":[{"user_id":%q}]}`, f.bobUser)
	if w = taskReq(t, f.h, "POST", "/v1/projects/alpha/teams", f.admin, "setup-other", other); w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	var out struct {
		Protocol int                  `json:"protocol_version"`
		Teams    []store.EnrolledTeam `json:"teams"`
	}
	w = taskReq(t, f.h, "GET", mine, f.alice, "", "")
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out.Protocol != 1 || len(out.Teams) != 1 || out.Teams[0].ID != team.ID || out.Teams[0].Name != "Build team" || !out.Teams[0].Coordinator || out.Teams[0].CoordinatorActive || out.Teams[0].Revision != team.Revision {
		t.Fatalf("listing %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "user_id") || strings.Contains(w.Body.String(), f.bobUser) || strings.Contains(w.Body.String(), "Other team") {
		t.Fatalf("listing discloses others: %s", w.Body)
	}
	// An active coordinator session shows as such.
	join := `{"role":"coordinator","profile":{"label":"agent","platform":"fixture","platform_version":"unknown"}}`
	if w = taskReq(t, f.h, "POST", "/v1/projects/alpha/teams/"+team.ID+"/join", f.alice, "join", join); w.Code != 201 {
		t.Fatalf("join %d %s", w.Code, w.Body)
	}
	w = taskReq(t, f.h, "GET", mine, f.alice, "", "")
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || len(out.Teams) != 1 || !out.Teams[0].CoordinatorActive {
		t.Fatalf("active coordinator not shown: %s", w.Body)
	}
	// Credentials that cannot hold a session cannot list either; a query
	// string is refused; the admin route still reaches a team by ID.
	for _, token := range []string{f.admin, f.env, f.writer} {
		if w := taskReq(t, f.h, "GET", mine, token, "", ""); w.Code != 403 {
			t.Fatalf("credential accepted: %d %s", w.Code, w.Body)
		}
	}
	if w := taskReq(t, f.h, "GET", mine+"?limit=5", f.alice, "", ""); w.Code != 400 {
		t.Fatalf("query accepted: %d", w.Code)
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/teams/"+team.ID, f.admin, "", ""); w.Code != 200 {
		t.Fatalf("admin team read by ID: %d", w.Code)
	}
}
