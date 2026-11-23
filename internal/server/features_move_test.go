package server

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"aimem/internal/store"
)

// PUT /v1/tasks/{id}/feature with another goal moves the feature: the
// record answers with the new epic and link, list_features follows it, the
// confirmed tasks stay, and the history entry names the goal it left. The
// route's authorization and refusals are those of any link write.
func TestFeatureMoveOverHTTP(t *testing.T) {
	g := newFeatureRig(t)
	next := g.created(t, "POST", "/v1/projects/alpha/epics", `{"title":"the next goal"}`)
	f := g.feature(t, "feature one", 1)
	work := g.task(t, "work", "READY")
	if w := g.confirm(t, g.alice, f, g.key(), fmt.Sprintf(`{"task":%q,"expected_revision":1}`, work)); w.Code != 200 {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	move := fmt.Sprintf(`{"goal":%q,"position":2,"mark":"mandatory","ground":"owner_condition","ground_note":"the owner's word of 2026-11-23","expected_revision":2}`, next)

	if w := g.setFeature(t, g.bob, f, g.key(), move); w.Code != 403 {
		t.Fatalf("a read-only grant moved a feature: %d %s", w.Code, w.Body)
	}
	if w := g.setFeature(t, g.alice, f, g.key(), strings.Replace(move, `"mark":"mandatory",`, ``, 1)); w.Code != 400 || !strings.Contains(w.Body.String(), "mark must be mandatory or optional") {
		t.Fatalf("a move without a mark: %d %s", w.Code, w.Body)
	}
	if w := g.setFeature(t, g.alice, f, g.key(), strings.Replace(move, `"position":2,`, ``, 1)); w.Code != 400 || !strings.Contains(w.Body.String(), "position must be between 1 and") {
		t.Fatalf("a move without a position: %d %s", w.Code, w.Body)
	}

	w := g.setFeature(t, g.alice, f, g.key(), move)
	rec := decodeTask(t, w)
	if w.Code != 200 || rec.ID != f || rec.Epic != next || rec.Feature == nil || rec.Feature.Goal != next || rec.Feature.Mark != "mandatory" ||
		rec.Feature.Ground != "owner_condition" || rec.Feature.Position != 2 || !slices.Equal(rec.Dependencies, []string{work}) {
		t.Fatalf("the move: %d %s", w.Code, w.Body)
	}
	if rows := g.features(t, "").Features; len(rows) != 0 {
		t.Fatalf("the goal left still lists the feature: %+v", rows)
	}
	w = taskReq(t, g.h, "GET", "/v1/projects/alpha/epics/"+next+"/features", g.alice, "", "")
	var joined featureList
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &joined) != nil || len(joined.Features) != 1 || joined.Features[0].ID != f || joined.Features[0].Confirmed != 1 {
		t.Fatalf("the goal joined: %d %s", w.Code, w.Body)
	}
	if ids := listed(t, taskReq(t, g.h, "GET", "/v1/projects/alpha/tasks?feature="+f, g.alice, "", "")); !slices.Equal(ids, []string{work + "/"}) {
		t.Fatalf("list_tasks with feature after the move: %v", ids)
	}

	w = taskReq(t, g.h, "GET", "/v1/tasks/"+f+"/history", g.alice, "", "")
	var h struct {
		Changes []struct {
			Task struct {
				Revision int64  `json:"revision"`
				Epic     string `json:"epic"`
			} `json:"task"`
			MovedFrom string `json:"moved_from"`
		} `json:"changes"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &h) != nil || len(h.Changes) != 3 {
		t.Fatalf("history: %d %s", w.Code, w.Body)
	}
	if last := h.Changes[2]; last.Task.Revision != 3 || last.Task.Epic != next || last.MovedFrom != g.goal {
		t.Fatalf("the move's history entry: %+v", last)
	}
	if strings.Count(w.Body.String(), `"moved_from"`) != 1 {
		t.Fatalf("moved_from on an entry that is not a move: %s", w.Body)
	}

	// Once the goal left is retired, a move back into it is a 400 that says
	// what to do.
	if w := taskReq(t, g.h, "PUT", "/v1/projects/alpha/epics/"+g.goal, g.alice, g.key(), `{"title":"the goal","state":"RETIRED","expected_revision":1}`); w.Code != 200 {
		t.Fatalf("retire: %d %s", w.Code, w.Body)
	}
	back := fmt.Sprintf(`{"goal":%q,"position":1,"mark":"optional","expected_revision":3}`, g.goal)
	if w := g.setFeature(t, g.alice, f, g.key(), back); w.Code != 400 || !strings.Contains(w.Body.String(), "move the feature to an OPEN goal") {
		t.Fatalf("a move into a retired goal: %d %s", w.Code, w.Body)
	}
}

// The rows keep only the goal's features: a record that names another goal,
// or is no feature, is left out, as a guard against data no write produces.
func TestFeatureRowsKeepOnlyTheGoalsFeatures(t *testing.T) {
	const goal, next = "goal-a", "goal-b"
	link := func(g string) *store.FeatureLink { return &store.FeatureLink{Goal: g, Position: 1, Mark: "optional"} }
	records := []store.Task{
		{ID: "stays", Kind: store.KindFeature, Feature: link(goal), TaskContent: store.TaskContent{Epic: goal}},
		{ID: "moved", Kind: store.KindFeature, Feature: link(next), TaskContent: store.TaskContent{Epic: next}},
		{ID: "removed", TaskContent: store.TaskContent{Epic: goal}},
	}
	rows := featureRows(goal, records, func(string) bool { return true })
	if len(rows) != 1 || rows[0].ID != "stays" || rows[0].Feature.Goal != goal {
		t.Fatalf("rows: %+v", rows)
	}
}
