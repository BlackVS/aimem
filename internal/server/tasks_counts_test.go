package server

// GET /v1/projects/{p}/tasks/counts is the board's shape under the list's
// assignee and epic filters, read by any task reader, and refuses what the
// list refuses: an unknown project is a 404 for both.

import (
	"encoding/json"
	"reflect"
	"testing"

	"aimem/internal/store"
)

func TestTaskCountsRoute(t *testing.T) {
	f := newTaskFixture(t)
	db, err := f.reg.Open("alpha")
	if err != nil {
		t.Fatal(err)
	}
	admin := store.TaskActor{Kind: "admin", Name: "host-admin"}
	epic, err := db.CreateEpic(store.EpicContent{Title: "release"}, admin, "k-epic")
	if err != nil {
		t.Fatal(err)
	}
	bobs := &store.TaskAssignee{Kind: "user", ID: f.bobUser}
	for i, c := range []struct {
		state    string
		assignee *store.TaskAssignee
		epic     string
		archived bool
	}{
		{"READY", nil, "", false},
		{"READY", bobs, epic.ID, false},
		{"BLOCKED", bobs, "", false},
		{"CANCELLED", bobs, epic.ID, true},
	} {
		task, err := db.CreateTask(store.TaskContent{Title: "t", State: c.state, Assignee: c.assignee, Epic: c.epic}, admin, "k-"+string(rune('a'+i)))
		if err != nil {
			t.Fatal(err)
		}
		if c.archived {
			content := task.TaskContent
			content.Archived = true
			if _, err := db.UpdateTask(task.ID, content, task.Revision, admin, "k-archive"); err != nil {
				t.Fatal(err)
			}
		}
	}
	type reply struct {
		Project  string         `json:"project"`
		Counts   map[string]int `json:"counts"`
		Archived int            `json:"archived"`
		Total    int            `json:"total"`
	}
	shape := func(archived, total int, states map[string]int) reply {
		out := reply{Project: "alpha", Counts: map[string]int{}, Archived: archived, Total: total}
		for _, st := range store.TaskStates {
			out.Counts[st] = states[st]
		}
		return out
	}
	for _, c := range []struct {
		query string
		want  reply
	}{
		{"", shape(1, 4, map[string]int{"READY": 2, "BLOCKED": 1})},
		{"?assignee=user/" + f.bobUser, shape(1, 3, map[string]int{"READY": 1, "BLOCKED": 1})},
		{"?epic=" + epic.ID, shape(1, 2, map[string]int{"READY": 1})},
		{"?assignee=user/" + f.bobUser + "&epic=" + epic.ID, shape(1, 2, map[string]int{"READY": 1})},
		{"?assignee=user/" + f.aliceUser, shape(0, 0, nil)},
	} {
		// bob reads with a cross-project reader token: a task read.
		w := taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks/counts"+c.query, f.bob, "", "")
		var got reply
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || !reflect.DeepEqual(got, c.want) {
			t.Fatalf("counts%s: %d %s", c.query, w.Code, w.Body)
		}
	}
	for _, c := range []struct {
		path string
		code int
	}{
		{"/v1/projects/nope/tasks", 404},
		{"/v1/projects/nope/tasks/counts", 404},
		{"/v1/projects/user/tasks/counts", 400},
		{"/v1/projects/alpha/tasks/counts?assignee=bob", 400},
		{"/v1/projects/alpha/tasks/counts?assignee=team/" + f.bobUser, 400},
		{"/v1/projects/alpha/tasks/counts?epic=nope", 400},
	} {
		if w := taskReq(t, f.h, "GET", c.path, f.bob, "", ""); w.Code != c.code {
			t.Fatalf("%s: %d %s, want %d", c.path, w.Code, w.Body, c.code)
		}
	}
	if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks/counts", "", "", ""); w.Code != 401 {
		t.Fatalf("anonymous counts: %d", w.Code)
	}
}
