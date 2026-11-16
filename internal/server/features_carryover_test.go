package server

// The carry-over procedure of docs/FEATURES.md (docs/DESIGN-feature-record.md,
// section 9), tried on the aicrew goal's records as section 10 of the design
// shows them: the eleven trial records keep their ids, history, comments,
// dependencies and evidence, and list_features equals the goal's list.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"aimem/internal/store"
)

// placeAs gives a record that was just created the id a fixture names,
// through a second connection to the project's store: the API mints ids.
// It rewrites the row, its history and its feed entries in one transaction,
// before anything else refers to the record.
func placeAs(t *testing.T, reg *store.Registry, project, table, from, to string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(reg.Root(), "projects", project, "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	var qs []string
	switch table {
	case "tasks":
		qs = []string{
			`UPDATE task_history SET task_id = ?2, body = json_set(body, '$.task.id', ?2) WHERE task_id = ?1`,
			`UPDATE task_state_changes SET task_id = ?2 WHERE task_id = ?1`,
			`UPDATE tasks SET id = ?2, body = json_set(body, '$.id', ?2) WHERE id = ?1`,
		}
	case "epics":
		qs = []string{
			`UPDATE epic_history SET epic_id = ?2, body = json_set(body, '$.epic.id', ?2) WHERE epic_id = ?1`,
			`UPDATE epics SET id = ?2, body = json_set(body, '$.id', ?2) WHERE id = ?1`,
		}
	}
	for _, q := range qs {
		if _, err := tx.Exec(q, from, to); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// trialFeature is one row of the design's section 10.
type trialFeature struct {
	id, title, mark, ground, note, alt, state string
}

const aicrewGoal = "01a186a3-12c3-7000-b23d-83d1cad3dad1"

var aicrewFeatures = []trialFeature{
	{"01a1e56e-4bc8-7000-a000-000000000001", "The cycle is proven in a simulation", "mandatory", "owner_condition", "owner, 2026-10-29 (increments I2, I3 and I5)", "", "DONE"},
	{"01a1e56e-4bdb-7000-a000-000000000002", "Turns start fresh from a brief", "mandatory", "owner_condition", "owner, 2026-10-15", "", "DONE"},
	{"01a1e56e-4bed-7000-ab2f-47753e8623bf", "A member can hold its own forge credential", "mandatory", "check", "", "", "DONE"},
	{"01a1e56e-4c01-7000-a000-000000000004", "The members' hosts exist", "mandatory", "check", "", "", "READY"},
	{"01a1e56e-4c10-7000-a000-000000000005", "The team is brought up on those hosts", "mandatory", "check", "", "", "BACKLOG"},
	{"01a1e56e-4c1f-7000-a000-000000000006", "The cycle is run", "mandatory", "check", "it is the goal's check", "", "BACKLOG"},
	{"01a1e56f-d5ff-7000-a000-000000000007", "The architect is a role in the product", "optional", "", "", "an architect session does it by hand", "BACKLOG"},
	{"01a1e56f-d60f-7000-a000-000000000008", "The operator has a seat in the product", "optional", "", "", "decisions are recorded on the task by hand", "BACKLOG"},
	{"01a1e56f-d647-7000-a000-000000000009", "Ordinary pull requests are merged by the product's rule", "optional", "", "", "done by hand", "BACKLOG"},
	{"01a1e56f-d65b-7000-a000-00000000000a", "Credentials are deposited through the product", "optional", "", "", "feature 3, placed by hand", "BACKLOG"},
	{"01a1e56f-d665-7000-a000-00000000000b", "An upgrade between releases is proven in the simulation", "optional", "", "", "none; a first cycle installs fresh", "IN_PROGRESS"},
}

// feature 3's confirmed tasks, in another epic of the same project.
var aicrewConfirmed = []string{"01a105fa-336e-7000-a000-000000000001", "01a105fa-337e-7000-a000-000000000002", "01a105fa-338e-7000-a000-000000000003"}

func TestFeatureCarryOverOfTheAicrewGoal(t *testing.T) {
	g := &featureRig{taskFixture: newTaskFixture(t)}
	db, err := g.reg.OpenExisting("alpha")
	if err != nil {
		t.Fatal(err)
	}
	admin := store.TaskActor{Kind: "admin", Name: "host-admin"}
	seq := 0
	key := func() string { seq++; return "fixture-" + strconv.Itoa(seq) }
	place := func(c store.TaskContent, id string) {
		t.Helper()
		rec, err := db.CreateTask(c, admin, key())
		if err != nil {
			t.Fatal(err)
		}
		placeAs(t, g.reg, "alpha", "tasks", rec.ID, id)
	}

	// The fixture: the goal, an earlier epic with feature 3's three tasks,
	// the eleven records as tasks titled [feature] N, and the goal's log.
	goal, err := db.CreateEpic(store.EpicContent{Title: "a first dogfood cycle"}, admin, key())
	if err != nil {
		t.Fatal(err)
	}
	placeAs(t, g.reg, "alpha", "epics", goal.ID, aicrewGoal)
	earlier, err := db.CreateEpic(store.EpicContent{Title: "credentials"}, admin, key())
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range aicrewConfirmed {
		place(store.TaskContent{Title: fmt.Sprintf("354c-%d", i), State: "DONE", Epic: earlier.ID}, id)
	}
	owner := &store.TaskAssignee{Kind: "user", ID: g.aliceUser}
	for i, f := range aicrewFeatures {
		c := store.TaskContent{Title: fmt.Sprintf("[feature] %d: %s", i+1, f.title), State: f.state, Epic: aicrewGoal,
			Objective: "the trial's record of feature " + strconv.Itoa(i+1)}
		if f.state == "DONE" {
			c.EvidenceRefs = []store.TaskRef{{Kind: "text", Ref: "the trial's evidence for feature " + strconv.Itoa(i+1)}}
		}
		if i == 2 {
			c.AcceptanceCriteria = "the product's command (`join --cred`) writes a forge credential into a member's home, and the member's clone uses it"
			c.Dependencies = slices.Clone(aicrewConfirmed)
			c.EvidenceRefs = []store.TaskRef{{Kind: "task", Ref: aicrewConfirmed[2], Note: "354c-2 DONE"}}
		}
		if i == 3 {
			c.Assignee = owner
		}
		place(c, f.id)
	}
	logID := "01a186a3-12d0-7000-a000-00000000000c"
	place(store.TaskContent{Title: "the goal's log", State: "IN_PROGRESS", Epic: aicrewGoal}, logID)
	// A task that depends on feature 5, as a trial record might.
	dependent := "01a1e570-0001-7000-a000-00000000000d"
	place(store.TaskContent{Title: "bring-up script", State: "BACKLOG", Epic: aicrewGoal, Dependencies: []string{aicrewFeatures[4].id}}, dependent)

	// The records' life before the carry-over, made through the routes: a
	// second revision and a comment on each.
	for _, f := range aicrewFeatures {
		cur := decodeTask(t, taskReq(t, g.h, "GET", "/v1/tasks/"+f.id, g.alice, "", ""))
		body, _ := json.Marshal(struct {
			store.TaskContent
			ExpectedRevision int64 `json:"expected_revision"`
		}{func() store.TaskContent { c := cur.TaskContent; c.NextAction = "the trial goes on"; return c }(), 1})
		if w := taskReq(t, g.h, "PUT", "/v1/tasks/"+f.id, g.alice, g.key(), string(body)); w.Code != 200 {
			t.Fatalf("history for %s: %d %s", f.id, w.Code, w.Body)
		}
		if w := taskReq(t, g.h, "POST", "/v1/tasks/"+f.id+"/comments", g.alice, g.key(), `{"body":"The trial discussed this record."}`); w.Code != 201 {
			t.Fatalf("comment on %s: %d %s", f.id, w.Code, w.Body)
		}
	}

	// Step 1: the goal's records whose title begins [feature].
	w := taskReq(t, g.h, "GET", "/v1/projects/alpha/tasks?epic="+aicrewGoal+"&limit=100", g.alice, "", "")
	var page struct {
		Tasks []struct{ ID, Title string } `json:"tasks"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatalf("step 1: %d %s", w.Code, w.Body)
	}
	position := regexp.MustCompile(`^\[feature\] (\d+):`)
	records := map[string]int{}
	for _, r := range page.Tasks {
		if m := position.FindStringSubmatch(r.Title); m != nil {
			records[r.ID], _ = strconv.Atoi(m[1])
		}
	}
	if len(records) != len(aicrewFeatures) {
		t.Fatalf("step 1 found %d records: %v", len(records), records)
	}

	// Steps 2 to 4: set_feature on each record; one a task depends on is
	// refused with that task named, the dependency is moved onto the
	// feature the other way round, and the record is retried.
	set := func(i int) *featureRigResponse {
		f := aicrewFeatures[i]
		cur := decodeTask(t, taskReq(t, g.h, "GET", "/v1/tasks/"+f.id, g.alice, "", ""))
		body, _ := json.Marshal(map[string]any{"goal": aicrewGoal, "position": records[f.id], "mark": f.mark, "ground": f.ground,
			"ground_note": f.note, "alternative": f.alt, "expected_revision": cur.Revision})
		w := g.setFeature(t, g.alice, f.id, g.key(), string(body))
		return &featureRigResponse{w.Code, w.Body.String()}
	}
	for i := range aicrewFeatures {
		r := set(i)
		if i == 4 {
			if r.code != 400 || !strings.Contains(r.body, dependent) {
				t.Fatalf("step 2, a record a task depends on: %d %s", r.code, r.body)
			}
			cur := decodeTask(t, taskReq(t, g.h, "GET", "/v1/tasks/"+dependent, g.alice, "", ""))
			c := cur.TaskContent
			c.Dependencies = nil
			body, _ := json.Marshal(struct {
				store.TaskContent
				ExpectedRevision int64 `json:"expected_revision"`
			}{c, cur.Revision})
			if w := taskReq(t, g.h, "PUT", "/v1/tasks/"+dependent, g.alice, g.key(), string(body)); w.Code != 200 {
				t.Fatalf("step 2, the dependency taken off: %d %s", w.Code, w.Body)
			}
			if r = set(i); r.code == 200 {
				cur := decodeTask(t, taskReq(t, g.h, "GET", "/v1/tasks/"+aicrewFeatures[i].id, g.alice, "", ""))
				w := g.confirm(t, g.alice, aicrewFeatures[i].id, g.key(), fmt.Sprintf(`{"task":%q,"expected_revision":%d}`, dependent, cur.Revision))
				if w.Code != 200 {
					t.Fatalf("step 2, the dependency moved onto the feature: %d %s", w.Code, w.Body)
				}
			}
		}
		if r.code != 200 {
			t.Fatalf("step 4, set_feature on %s: %d %s", aicrewFeatures[i].id, r.code, r.body)
		}
	}

	// Step 5: list_features equals the goal's list.
	w = taskReq(t, g.h, "GET", "/v1/projects/alpha/epics/"+aicrewGoal+"/features", g.alice, "", "")
	var got featureList
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Features) != len(aicrewFeatures) {
		t.Fatalf("step 5: %d %s", w.Code, w.Body)
	}
	for i, f := range aicrewFeatures {
		row := got.Features[i]
		l := row.Feature
		if row.ID != f.id || l.Position != i+1 || l.Mark != f.mark || l.Ground != f.ground || l.GroundNote != f.note || l.Alternative != f.alt || row.State != f.state || row.Tie {
			t.Errorf("row %d: %+v %+v, want %+v", i+1, row, *l, f)
		}
	}
	if got.Features[2].Confirmed != 3 || got.Features[4].Confirmed != 1 || got.Features[3].Assignee == nil || got.Features[3].Assignee.ID != g.aliceUser {
		t.Fatalf("confirmed tasks and owners: %+v", got.Features)
	}
	// Feature 3's tasks read back through the list.
	if ids := listed(t, taskReq(t, g.h, "GET", "/v1/projects/alpha/tasks?feature="+aicrewFeatures[2].id[:13], g.alice, "", "")); strings.Join(ids, ",") != strings.Join(func() []string {
		var out []string
		for _, id := range aicrewConfirmed {
			out = append(out, id+"/")
		}
		return out
	}(), ",") {
		t.Fatalf("feature 3's tasks: %v", ids)
	}

	// Step 6: the result is recorded on the goal's log.
	if w := taskReq(t, g.h, "POST", "/v1/tasks/"+logID+"/comments", g.alice, g.key(), `{"body":"The carry-over converted eleven records, and list_features agrees with the goal's list."}`); w.Code != 201 {
		t.Fatalf("step 6: %d %s", w.Code, w.Body)
	}

	// Nothing is lost: each record keeps its id, its history (the earlier
	// revisions and the conversion after them), its comment, its
	// dependencies and its evidence.
	for i, f := range aicrewFeatures {
		rec := decodeTask(t, taskReq(t, g.h, "GET", "/v1/tasks/"+f.id, g.alice, "", ""))
		if rec.Kind != store.KindFeature || rec.State != f.state || rec.NextAction != "the trial goes on" {
			t.Errorf("%s after the carry-over: %+v", f.id, rec)
		}
		var hist struct {
			Changes []store.TaskChange `json:"changes"`
		}
		json.Unmarshal(taskReq(t, g.h, "GET", "/v1/tasks/"+f.id+"/history", g.alice, "", "").Body.Bytes(), &hist)
		if n := len(hist.Changes); n < 3 || hist.Changes[0].Task.ID != f.id || hist.Changes[0].Task.Kind != "" || hist.Changes[n-1].Task.Revision != rec.Revision {
			t.Errorf("%s history: %d changes", f.id, n)
		}
		if w := taskReq(t, g.h, "GET", "/v1/tasks/"+f.id+"/comments", g.alice, "", ""); !strings.Contains(w.Body.String(), "The trial discussed this record.") {
			t.Errorf("%s comments: %s", f.id, w.Body)
		}
		if f.state == "DONE" && len(rec.EvidenceRefs) != 1 {
			t.Errorf("%s evidence: %+v", f.id, rec.EvidenceRefs)
		}
		if i == 2 && (!slices.Equal(rec.Dependencies, aicrewConfirmed) || rec.EvidenceRefs[0].Note != "354c-2 DONE") {
			t.Errorf("feature 3: %+v", rec)
		}
	}
	// The goal's log stays a task, with its comment.
	if rec := decodeTask(t, taskReq(t, g.h, "GET", "/v1/tasks/"+logID, g.alice, "", "")); rec.Kind != "" {
		t.Fatalf("the goal's log: %+v", rec)
	}
	// The feed closes each record that was work, whatever its state.
	feed, _, err := db.TaskStateChanges(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	last := map[string]string{}
	for _, c := range feed {
		last[c.TaskID] = c.To
	}
	for _, f := range aicrewFeatures {
		if last[f.id] != store.FeatureFeedState {
			t.Errorf("%s ends the feed at %q", f.id, last[f.id])
		}
	}
}

type featureRigResponse struct {
	code int
	body string
}
