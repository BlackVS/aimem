package server

// A reached goal, an epic's check, and the features listing's ready-to-close
// mark and count of the goal's tasks on no feature, over the real handler
// with real tokens.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"aimem/internal/store"
)

// putEpic updates the rig's epic id with alice's token at its current
// revision; fields is the body's JSON fields without the revision.
func (g *featureRig) putEpic(t *testing.T, id, fields string) (int, string) {
	t.Helper()
	w := taskReq(t, g.h, "GET", "/v1/projects/alpha/epics/"+id, g.alice, "", "")
	var e store.Epic
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &e) != nil {
		t.Fatalf("get epic: %d %s", w.Code, w.Body)
	}
	w = taskReq(t, g.h, "PUT", "/v1/projects/alpha/epics/"+id, g.alice, g.key(), fmt.Sprintf(`{%s,"expected_revision":%d}`, fields, e.Revision))
	return w.Code, w.Body.String()
}

func (g *featureRig) epic(t *testing.T, id string) store.Epic {
	t.Helper()
	w := taskReq(t, g.h, "GET", "/v1/projects/alpha/epics/"+id, g.alice, "", "")
	var e store.Epic
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &e) != nil {
		t.Fatalf("get epic: %d %s", w.Code, w.Body)
	}
	return e
}

// setState moves a record to state, resending its whole content (an update
// replaces it); DONE carries evidence.
func (g *featureRig) setState(t *testing.T, id, state string) {
	t.Helper()
	cur := decodeTask(t, taskReq(t, g.h, "GET", "/v1/tasks/"+id, g.alice, "", ""))
	c := cur.TaskContent
	c.State = state
	if state == "DONE" {
		c.EvidenceRefs = []store.TaskRef{{Kind: "url", Ref: "https://example.com/ci/1"}}
	}
	body, _ := json.Marshal(struct {
		store.TaskContent
		ExpectedRevision int64 `json:"expected_revision"`
	}{c, cur.Revision})
	if w := taskReq(t, g.h, "PUT", "/v1/tasks/"+id, g.alice, g.key(), string(body)); w.Code != 200 {
		t.Fatalf("set %s: %d %s", state, w.Code, w.Body)
	}
}

func (g *featureRig) confirmOn(t *testing.T, token, feature, task string) {
	t.Helper()
	if w := g.confirm(t, token, feature, g.key(), fmt.Sprintf(`{"task":%q,"expected_revision":%d}`, task, g.revision(t, feature))); w.Code != 200 {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
}

// The check is written, read back and kept; REACHED is refused with an
// unfinished feature and accepted without one; a reached goal is listed
// only on request and refuses a new task and a move into it.
func TestGoalReachedAndCheckOverHTTP(t *testing.T) {
	g := newFeatureRig(t)
	if code, body := g.putEpic(t, g.goal, `"title":"the goal","check":"a fresh session delivers one task"`); code != 200 || !strings.Contains(body, `"check":"a fresh session delivers one task"`) {
		t.Fatalf("write the check: %d %s", code, body)
	}
	// An update that sends neither check nor state keeps both.
	if code, body := g.putEpic(t, g.goal, `"title":"the goal, renamed"`); code != 200 {
		t.Fatalf("rename: %d %s", code, body)
	}
	if e := g.epic(t, g.goal); e.Check != "a fresh session delivers one task" || e.State != "OPEN" || e.Title != "the goal, renamed" {
		t.Fatalf("kept: %+v", e)
	}
	if code, body := g.putEpic(t, g.goal, `"title":"the goal","check":"a\u0000b"`); code != 400 || !strings.Contains(body, "check") {
		t.Fatalf("the check is prose-checked: %d %s", code, body)
	}

	f := g.feature(t, "the feature", 1)
	if code, body := g.putEpic(t, g.goal, `"title":"the goal","state":"REACHED"`); code != 400 || !strings.Contains(body, f) || !strings.Contains(body, "set_feature") {
		t.Fatalf("REACHED with an open feature: %d %s", code, body)
	}
	g.setState(t, f, "CANCELLED")
	if code, body := g.putEpic(t, g.goal, `"title":"the goal","state":"REACHED"`); code != 200 || !strings.Contains(body, `"state":"REACHED"`) {
		t.Fatalf("REACHED: %d %s", code, body)
	}
	// A client that sends no state keeps REACHED.
	if code, _ := g.putEpic(t, g.goal, `"title":"the goal, again"`); code != 200 || g.epic(t, g.goal).State != "REACHED" {
		t.Fatalf("an update without a state reopened the goal: %d %+v", code, g.epic(t, g.goal))
	}
	if w := taskReq(t, g.h, "GET", "/v1/projects/alpha/epics", g.alice, "", ""); w.Code != 200 || strings.Contains(w.Body.String(), g.goal) {
		t.Fatalf("a reached goal is left out by default: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, g.h, "GET", "/v1/projects/alpha/epics?include_retired=true", g.alice, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), g.goal) {
		t.Fatalf("a reached goal on request: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, g.h, "POST", "/v1/projects/alpha/tasks", g.alice, g.key(), fmt.Sprintf(`{"title":"late","epic":%q}`, g.goal)); w.Code != 400 || !strings.Contains(w.Body.String(), "reached goal") {
		t.Fatalf("a new task in a reached goal: %d %s", w.Code, w.Body)
	}
	other := g.created(t, "POST", "/v1/projects/alpha/epics", `{"title":"another goal"}`)
	mover := g.created(t, "POST", "/v1/projects/alpha/tasks", fmt.Sprintf(`{"title":"mover","epic":%q,"feature":{"goal":%q,"position":1,"mark":"optional"}}`, other, other))
	if w := g.setFeature(t, g.alice, mover, g.key(), fmt.Sprintf(`{"goal":%q,"position":1,"mark":"optional","expected_revision":1}`, g.goal)); w.Code != 400 || !strings.Contains(w.Body.String(), "the goal is reached") {
		t.Fatalf("a move into a reached goal: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, g.h, "POST", "/v1/projects/alpha/epics", g.alice, g.key(), `{"title":"born reached","state":"REACHED"}`); w.Code != 400 {
		t.Fatalf("a new epic is not REACHED: %d %s", w.Code, w.Body)
	}
}

type goalListing struct {
	Features []featureRow `json:"features"`
	OnNone   *int         `json:"tasks_on_no_feature"`
}

func (g *featureRig) goalListing(t *testing.T, token string) (goalListing, string) {
	t.Helper()
	w := taskReq(t, g.h, "GET", "/v1/projects/alpha/epics/"+g.goal+"/features", token, "", "")
	var out goalListing
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || out.OnNone == nil {
		t.Fatalf("list_features: %d %s", w.Code, w.Body)
	}
	return out, w.Body.String()
}

func readyByID(l goalListing) map[string]bool {
	m := map[string]bool{}
	for _, r := range l.Features {
		m[r.ID] = r.Ready
	}
	return m
}

// The mark: every confirmed task done marks an open feature; an open task,
// no confirmed task or a finished feature does not. The count: the goal's
// tasks on no feature, the log among them, without the ones another goal's
// feature confirms or that are archived.
func TestListFeaturesMarkAndCount(t *testing.T) {
	g := newFeatureRig(t)
	ready := g.feature(t, "all done", 1)
	half := g.feature(t, "one open", 2)
	empty := g.feature(t, "nothing confirmed", 3)
	finished := g.feature(t, "already done", 4)
	for _, f := range []string{ready, finished} {
		g.confirmOn(t, g.alice, f, g.task(t, "done work", "DONE"))
		g.confirmOn(t, g.alice, f, g.task(t, "cancelled work", "CANCELLED"))
	}
	g.confirmOn(t, g.alice, half, g.task(t, "done work", "DONE"))
	g.confirmOn(t, g.alice, half, g.task(t, "open work", "IN_PROGRESS"))
	g.setState(t, finished, "DONE")

	g.task(t, "the goal's log", "IN_PROGRESS")
	other := g.created(t, "POST", "/v1/projects/alpha/epics", `{"title":"another goal"}`)
	elsewhere := g.task(t, "confirmed by another goal's feature", "READY")
	of := g.created(t, "POST", "/v1/projects/alpha/tasks", fmt.Sprintf(`{"title":"other feature","epic":%q,"feature":{"goal":%q,"position":1,"mark":"optional"}}`, other, other))
	g.confirmOn(t, g.alice, of, elsewhere)
	g.created(t, "POST", "/v1/projects/alpha/tasks", fmt.Sprintf(`{"title":"archived","state":"DONE","archived":true,"epic":%q}`, g.goal))

	l, _ := g.goalListing(t, g.alice)
	got := readyByID(l)
	want := map[string]bool{ready: true, half: false, empty: false, finished: false}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("ready_to_close of %s: got %v, want %v (%+v)", id, got[id], w, l.Features)
		}
	}
	if *l.OnNone != 1 {
		t.Fatalf("tasks_on_no_feature: the log alone, got %d", *l.OnNone)
	}
}

// A confirmed task of a project the caller cannot read gives no mark and no
// trace: the row of a caller without the grant reads as for a task not done,
// and names neither the task nor its title; a caller with the grant gets the
// mark.
func TestListFeaturesUnreadableConfirmedTask(t *testing.T) {
	g := newFeatureRig(t)
	f := g.feature(t, "spans two projects", 1)
	g.confirmOn(t, g.alice, f, g.task(t, "done here", "DONE"))
	betaDB, err := g.reg.OpenExisting("beta")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := betaDB.CreateTask(store.TaskContent{Title: "secret beta work", State: "DONE"}, store.TaskActor{Kind: "admin", Name: "host-admin"}, "beta-done")
	if err != nil {
		t.Fatal(err)
	}
	g.confirmOn(t, g.admin, f, beta.ID)

	l, body := g.goalListing(t, g.alice)
	if readyByID(l)[f] {
		t.Fatalf("a task alice cannot read must withhold the mark: %s", body)
	}
	if strings.Contains(body, beta.ID) || strings.Contains(body, "secret beta work") {
		t.Fatalf("the listing leaks the other project's task: %s", body)
	}
	if l.Features[0].Confirmed != 2 {
		t.Fatalf("confirmed_tasks counts the feature's ids: %+v", l.Features[0])
	}
	if l, body := g.goalListing(t, g.admin); !readyByID(l)[f] {
		t.Fatalf("a caller who reads both projects gets the mark: %s", body)
	}
}
