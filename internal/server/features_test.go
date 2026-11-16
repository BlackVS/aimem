package server

// The feature routes (docs/DESIGN-feature-record.md, section 6): set_feature,
// confirm_feature_task and list_features, and the kind and feature filters
// of the task list and counts, over the real handler with real tokens.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"aimem/internal/store"
)

// featureRig is the task fixture with an epic (the goal) in alpha.
type featureRig struct {
	*taskFixture
	goal string
	seq  int
}

func newFeatureRig(t *testing.T) *featureRig {
	t.Helper()
	g := &featureRig{taskFixture: newTaskFixture(t)}
	g.goal = g.created(t, "POST", "/v1/projects/alpha/epics", `{"title":"the goal"}`)
	return g
}

func (g *featureRig) key() string {
	g.seq++
	return fmt.Sprintf("feat-%d", g.seq)
}

// created makes a record with alice's token and returns its id.
func (g *featureRig) created(t *testing.T, method, path, body string) string {
	t.Helper()
	w := taskReq(t, g.h, method, path, g.alice, g.key(), body)
	if w.Code != 201 {
		t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body)
	}
	var out struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	return out.ID
}

func (g *featureRig) task(t *testing.T, title, state string) string {
	t.Helper()
	return g.created(t, "POST", "/v1/projects/alpha/tasks", fmt.Sprintf(`{"title":%q,"state":%q,"epic":%q}`, title, state, g.goal))
}

func (g *featureRig) feature(t *testing.T, title string, position int) string {
	t.Helper()
	return g.created(t, "POST", "/v1/projects/alpha/tasks", fmt.Sprintf(`{"title":%q,"epic":%q,"feature":{"goal":%q,"position":%d,"mark":"optional","alternative":"done by hand"}}`, title, g.goal, g.goal, position))
}

func (g *featureRig) revision(t *testing.T, id string) int64 {
	t.Helper()
	return decodeTask(t, taskReq(t, g.h, "GET", "/v1/tasks/"+id, g.alice, "", "")).Revision
}

func (g *featureRig) setFeature(t *testing.T, token, id, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return taskReq(t, g.h, "PUT", "/v1/tasks/"+id+"/feature", token, key, body)
}

func (g *featureRig) confirm(t *testing.T, token, feature, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return taskReq(t, g.h, "POST", "/v1/tasks/"+feature+"/feature/tasks", token, key, body)
}

type featureList struct {
	Goal     string       `json:"goal"`
	Features []featureRow `json:"features"`
}

func (g *featureRig) features(t *testing.T, query string) featureList {
	t.Helper()
	w := taskReq(t, g.h, "GET", "/v1/projects/alpha/epics/"+g.goal+"/features"+query, g.alice, "", "")
	var out featureList
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("list_features: %d %s", w.Code, w.Body)
	}
	return out
}

// listed returns the ids of a task list's rows, in order.
func listed(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var page struct {
		Tasks []struct {
			ID   string `json:"id"`
			Kind string `json:"kind"`
		} `json:"tasks"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	var ids []string
	for _, r := range page.Tasks {
		ids = append(ids, r.ID+"/"+r.Kind)
	}
	return ids
}

func TestFeatureRoutesSetConfirmList(t *testing.T) {
	g := newFeatureRig(t)
	work := g.task(t, "work", "READY")
	f1 := g.feature(t, "feature one", 1)

	// A feature's record carries kind and its link; a task carries neither.
	var rec map[string]any
	json.Unmarshal(taskReq(t, g.h, "GET", "/v1/tasks/"+f1, g.alice, "", "").Body.Bytes(), &rec)
	if link, _ := rec["feature"].(map[string]any); rec["kind"] != "feature" || link["goal"] != g.goal || link["position"] != 1.0 {
		t.Fatalf("feature record: %v", rec)
	}
	body := taskReq(t, g.h, "GET", "/v1/tasks/"+work, g.alice, "", "").Body.String()
	if strings.Contains(body, `"kind"`) || strings.Contains(body, `"feature"`) {
		t.Fatalf("a task reads with feature fields: %s", body)
	}

	// The list and the counts take kind; a list is of work by default.
	for q, want := range map[string]string{"": work + "/", "?kind=task": work + "/", "?kind=feature": f1 + "/feature", "?kind=any": work + "/," + f1 + "/feature"} {
		if got := strings.Join(listed(t, taskReq(t, g.h, "GET", "/v1/projects/alpha/tasks"+q, g.alice, "", "")), ","); got != want {
			t.Errorf("list%s: %s, want %s", q, got, want)
		}
	}
	if w := taskReq(t, g.h, "GET", "/v1/projects/alpha/tasks?kind=goal", g.alice, "", ""); w.Code != 400 || !strings.Contains(w.Body.String(), "kind must be task, feature or any") {
		t.Fatalf("a bad kind: %d %s", w.Code, w.Body)
	}
	for q, want := range map[string]string{"": `"total":1`, "?kind=feature": `"total":1`, "?kind=any": `"total":2`} {
		if w := taskReq(t, g.h, "GET", "/v1/projects/alpha/tasks/counts"+q, g.alice, "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Errorf("counts%s: %d %s", q, w.Code, w.Body)
		}
	}
	if w := taskReq(t, g.h, "GET", "/v1/projects/alpha/tasks/counts?kind=goal", g.alice, "", ""); w.Code != 400 || !strings.Contains(w.Body.String(), "kind must be task, feature or any") || strings.Count(w.Body.String(), "\n") != 1 {
		t.Fatalf("counts with a bad kind: %d %s", w.Code, w.Body)
	}

	// set_feature turns a READY task into a feature at position 2.
	conv := g.task(t, "becomes a feature", "READY")
	w := g.setFeature(t, g.alice, conv, g.key(), fmt.Sprintf(`{"goal":%q,"position":2,"mark":"mandatory","ground":"check","ground_note":"the goal's check needs it","expected_revision":1}`, g.goal))
	if got := decodeTask(t, w); w.Code != 200 || got.Kind != store.KindFeature || got.Feature.Position != 2 || got.State != "READY" || got.Revision != 2 {
		t.Fatalf("conversion: %d %s", w.Code, w.Body)
	}

	// confirm_feature_task by a prefix; the list by the feature's prefix.
	// The prefixes run past the millisecond into the sequence: records
	// made in one millisecond share their first 13 characters.
	w = g.confirm(t, g.alice, f1, g.key(), fmt.Sprintf(`{"task":%q,"expected_revision":1}`, work[:18]))
	if got := decodeTask(t, w); w.Code != 200 || len(got.Dependencies) != 1 || got.Dependencies[0] != work {
		t.Fatalf("confirm: %d %s", w.Code, w.Body)
	}
	if got := strings.Join(listed(t, taskReq(t, g.h, "GET", "/v1/projects/alpha/tasks?feature="+f1[:18], g.alice, "", "")), ","); got != work+"/" {
		t.Fatalf("the feature's tasks: %s", got)
	}
	if w := taskReq(t, g.h, "GET", "/v1/projects/alpha/tasks/counts?kind=any&feature="+f1, g.alice, "", ""); !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("the feature's counts: %s", w.Body)
	}
	if w := taskReq(t, g.h, "GET", "/v1/projects/alpha/tasks?feature="+work, g.alice, "", ""); w.Code != 400 || !strings.Contains(w.Body.String(), "names no feature of this project") {
		t.Fatalf("a task as the feature filter: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, g.h, "GET", "/v1/projects/alpha/tasks?feature=0190a1b2-c3d4", g.alice, "", ""); w.Code != 400 || !strings.Contains(w.Body.String(), "names no feature of this project") {
		t.Fatalf("an unknown feature prefix: %d %s", w.Code, w.Body)
	}

	// list_features: position order, links and confirmed counts.
	got := g.features(t, "")
	if got.Goal != g.goal || len(got.Features) != 2 || got.Features[0].ID != f1 || got.Features[1].ID != conv ||
		got.Features[0].Confirmed != 1 || got.Features[1].Feature.Mark != "mandatory" || got.Features[0].Tie || got.Features[1].Tie ||
		got.Features[0].Links.Self != "/v1/tasks/"+f1 {
		t.Fatalf("list_features: %+v", got)
	}
	// A shared position is marked on both rows, in id order.
	f3 := g.feature(t, "feature three", 1)
	got = g.features(t, "")
	if len(got.Features) != 3 || got.Features[0].ID != f1 || got.Features[1].ID != f3 || !got.Features[0].Tie || !got.Features[1].Tie || got.Features[2].Tie {
		t.Fatalf("a tie: %+v", got)
	}
	if w := taskReq(t, g.h, "GET", "/v1/projects/alpha/epics/"+g.goal+"/features?include_archived=maybe", g.alice, "", ""); w.Code != 400 {
		t.Fatalf("a bad include_archived: %d %s", w.Code, w.Body)
	}
	if w := taskReq(t, g.h, "GET", "/v1/projects/alpha/epics/0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a60/features", g.alice, "", ""); w.Code != 404 {
		t.Fatalf("an unknown goal: %d %s", w.Code, w.Body)
	}

	// The way back: refused with confirmed tasks; remove takes no link.
	if w := g.setFeature(t, g.alice, f1, g.key(), `{"remove":true,"expected_revision":2}`); w.Code != 400 || !strings.Contains(w.Body.String(), "confirmed tasks") {
		t.Fatalf("remove with confirmed tasks: %d %s", w.Code, w.Body)
	}
	if w := g.setFeature(t, g.alice, f1, g.key(), `{"remove":true,"position":1,"expected_revision":2}`); w.Code != 400 || !strings.Contains(w.Body.String(), "remove takes no link fields") {
		t.Fatalf("remove with a link field: %d %s", w.Code, w.Body)
	}
	w = g.confirm(t, g.alice, f1, g.key(), fmt.Sprintf(`{"task":%q,"remove":true,"expected_revision":2}`, work))
	if got := decodeTask(t, w); w.Code != 200 || len(got.Dependencies) != 0 {
		t.Fatalf("take out: %d %s", w.Code, w.Body)
	}
	w = g.setFeature(t, g.alice, f1, g.key(), `{"remove":true,"expected_revision":3}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), `"kind"`) || strings.Contains(w.Body.String(), `"feature"`) {
		t.Fatalf("the way back: %d %s", w.Code, w.Body)
	}
}

func TestFeatureRouteRefusals(t *testing.T) {
	g := newFeatureRig(t)
	f1 := g.feature(t, "feature one", 1)
	work := g.task(t, "work", "READY")
	link := fmt.Sprintf(`{"goal":%q,"position":3,"mark":"optional","expected_revision":1}`, g.goal)
	confirm := fmt.Sprintf(`{"task":%q,"expected_revision":1}`, work)

	// No grant answers as not found, on every route.
	for _, w := range []*httptest.ResponseRecorder{
		g.setFeature(t, g.stranger, f1, "s1", link),
		g.confirm(t, g.stranger, f1, "s2", confirm),
		taskReq(t, g.h, "GET", "/v1/projects/alpha/epics/"+g.goal+"/features", g.stranger, "", ""),
	} {
		if w.Code != 404 {
			t.Errorf("no grant: %d %s", w.Code, w.Body)
		}
	}
	// A read-only grant may not write.
	for _, w := range []*httptest.ResponseRecorder{g.setFeature(t, g.bob, f1, "b1", link), g.confirm(t, g.bob, f1, "b2", confirm)} {
		if w.Code != 403 {
			t.Errorf("read-only: %d %s", w.Code, w.Body)
		}
	}
	// The idempotency key is required.
	for _, w := range []*httptest.ResponseRecorder{g.setFeature(t, g.alice, f1, "", link), g.confirm(t, g.alice, f1, "", confirm)} {
		if w.Code != 400 || !strings.Contains(w.Body.String(), "Idempotency-Key") {
			t.Errorf("no key: %d %s", w.Code, w.Body)
		}
	}
	// A stale revision is a conflict carrying the current record.
	for _, w := range []*httptest.ResponseRecorder{
		g.setFeature(t, g.alice, f1, g.key(), strings.Replace(link, `"expected_revision":1`, `"expected_revision":9`, 1)),
		g.confirm(t, g.alice, f1, g.key(), strings.Replace(confirm, `"expected_revision":1`, `"expected_revision":9`, 1)),
	} {
		var c struct {
			Current taskResponse `json:"current"`
		}
		if w.Code != 409 || json.Unmarshal(w.Body.Bytes(), &c) != nil || c.Current.ID != f1 || c.Current.Revision != 1 || c.Current.Kind != store.KindFeature {
			t.Errorf("stale revision: %d %s", w.Code, w.Body)
		}
	}
	// A repeated key returns the first answer.
	first := g.setFeature(t, g.alice, f1, "same-set", link)
	again := g.setFeature(t, g.alice, f1, "same-set", link)
	if first.Code != 200 || again.Code != 200 || first.Body.String() != again.Body.String() || g.revision(t, f1) != 2 {
		t.Fatalf("set retry: %d %s / %d %s", first.Code, first.Body, again.Code, again.Body)
	}
	confirm = fmt.Sprintf(`{"task":%q,"expected_revision":2}`, work)
	first = g.confirm(t, g.alice, f1, "same-confirm", confirm)
	again = g.confirm(t, g.alice, f1, "same-confirm", confirm)
	if first.Code != 200 || again.Code != 200 || first.Body.String() != again.Body.String() || g.revision(t, f1) != 3 {
		t.Fatalf("confirm retry: %d %s / %d %s", first.Code, first.Body, again.Code, again.Body)
	}

	// The task to confirm: required, known and readable (one answer), and
	// not a feature, whatever its project.
	if w := g.confirm(t, g.alice, f1, g.key(), `{"expected_revision":3}`); w.Code != 400 || !strings.Contains(w.Body.String(), "task is required") {
		t.Fatalf("no task: %d %s", w.Code, w.Body)
	}
	unknown := g.confirm(t, g.alice, f1, g.key(), `{"task":"0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a60","expected_revision":3}`)
	if unknown.Code != 400 || !strings.Contains(unknown.Body.String(), errConfirmTaskUnknown.Error()) {
		t.Fatalf("an unknown task: %d %s", unknown.Code, unknown.Body)
	}
	betaDB, err := g.reg.OpenExisting("beta")
	if err != nil {
		t.Fatal(err)
	}
	admin := store.TaskActor{Kind: "admin", Name: "host-admin"}
	betaTask, err := betaDB.CreateTask(store.TaskContent{Title: "beta work"}, admin, "beta-1")
	if err != nil {
		t.Fatal(err)
	}
	if w := g.confirm(t, g.alice, f1, g.key(), fmt.Sprintf(`{"task":%q,"expected_revision":3}`, betaTask.ID)); w.Code != 400 || w.Body.String() != unknown.Body.String() {
		t.Fatalf("an unreadable task answers as an unknown one: %d %s, unknown %s", w.Code, w.Body, unknown.Body)
	}
	betaGoal, err := betaDB.CreateEpic(store.EpicContent{Title: "beta goal"}, admin, "beta-goal")
	if err != nil {
		t.Fatal(err)
	}
	betaFeature, err := betaDB.CreateFeature(store.TaskContent{Title: "beta feature", Epic: betaGoal.ID}, store.FeatureLink{Goal: betaGoal.ID, Position: 1, Mark: "optional"}, admin, "beta-feature")
	if err != nil {
		t.Fatal(err)
	}
	// The admin reads both projects: another project's feature is refused
	// by the route, which the store cannot do.
	if w := g.confirm(t, g.admin, f1, g.key(), fmt.Sprintf(`{"task":%q,"expected_revision":3}`, betaFeature.ID)); w.Code != 400 || !strings.Contains(w.Body.String(), "is a feature, and a feature confirms tasks, not features") {
		t.Fatalf("another project's feature: %d %s", w.Code, w.Body)
	}
	// A task of another project the caller reads is confirmed.
	if w := g.confirm(t, g.admin, f1, g.key(), fmt.Sprintf(`{"task":%q,"expected_revision":3}`, betaTask.ID)); w.Code != 200 {
		t.Fatalf("another project's task: %d %s", w.Code, w.Body)
	}
	// Taking out a full id needs only the feature's list: the beta task
	// is not readable by alice, and still comes out.
	if w := g.confirm(t, g.alice, f1, g.key(), fmt.Sprintf(`{"task":%q,"remove":true,"expected_revision":4}`, betaTask.ID)); w.Code != 200 {
		t.Fatalf("take out an unreadable task: %d %s", w.Code, w.Body)
	}

	// The prose fields are checked as a task's are, on a create and a set.
	noted := g.task(t, "noted", "BACKLOG")
	w := taskReq(t, g.h, "POST", "/v1/projects/alpha/tasks", g.alice, g.key(), fmt.Sprintf(`{"title":"f","epic":%q,"feature":{"goal":%q,"position":1,"mark":"optional","alternative":"Checkpoint74files written"}}`, g.goal, g.goal))
	refusedText(t, w, "alternative", "Checkpoint74files", stageFilter)
	w = g.setFeature(t, g.alice, noted, g.key(), fmt.Sprintf(`{"goal":%q,"position":1,"mark":"mandatory","ground":"check","ground_note":"Checkpoint74files written","expected_revision":1}`, g.goal))
	refusedText(t, w, "ground_note", "Checkpoint74files", stageFilter)
	if g.revision(t, noted) != 1 {
		t.Fatal("a refused set wrote")
	}
	// An unchanged note is not checked again, as a task's unchanged field.
	db, err := g.reg.OpenExisting("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetFeature(noted, &store.FeatureLink{Goal: g.goal, Position: 4, Mark: "optional", Alternative: "Checkpoint74files written"}, false, 1, admin, "stored-before"); err != nil {
		t.Fatal(err)
	}
	if w := g.setFeature(t, g.alice, noted, g.key(), fmt.Sprintf(`{"goal":%q,"position":5,"mark":"optional","alternative":"Checkpoint74files written","expected_revision":2}`, g.goal)); w.Code != 200 {
		t.Fatalf("an unchanged note: %d %s", w.Code, w.Body)
	}
	// A key reused from a task create does not let a feature's text pass
	// unchecked: the task create's receipt is not the feature's.
	plain := fmt.Sprintf(`{"title":"reused","epic":%q}`, g.goal)
	if w := taskReq(t, g.h, "POST", "/v1/projects/alpha/tasks", g.alice, "reused-key", plain); w.Code != 201 {
		t.Fatalf("plain create: %d %s", w.Code, w.Body)
	}
	w = taskReq(t, g.h, "POST", "/v1/projects/alpha/tasks", g.alice, "reused-key", fmt.Sprintf(`{"title":"reused","epic":%q,"feature":{"goal":%q,"position":1,"mark":"optional","alternative":"Checkpoint74files written"}}`, g.goal, g.goal))
	refusedText(t, w, "alternative", "Checkpoint74files", stageFilter)
	// The link inside a create is decoded strictly.
	if w := taskReq(t, g.h, "POST", "/v1/projects/alpha/tasks", g.alice, g.key(), fmt.Sprintf(`{"title":"f","epic":%q,"feature":{"goal":%q,"position":1,"mark":"optional","weight":3}}`, g.goal, g.goal)); w.Code != 400 || !strings.Contains(w.Body.String(), "weight") {
		t.Fatalf("an unknown link field: %d %s", w.Code, w.Body)
	}
}

// A client one release older reads a feature by its id and its full update
// keeps the kind and the link; its list (no kind) shows work only.
func TestFeatureOlderClientOnThisHub(t *testing.T) {
	g := newFeatureRig(t)
	f1 := g.feature(t, "feature one", 1)
	w := taskReq(t, g.h, "PUT", "/v1/tasks/"+f1, g.alice, g.key(), fmt.Sprintf(`{"title":"feature one, renamed","state":"READY","epic":%q,"expected_revision":1}`, g.goal))
	if got := decodeTask(t, w); w.Code != 200 || got.Kind != store.KindFeature || got.Feature == nil || got.Feature.Position != 1 || got.Title != "feature one, renamed" {
		t.Fatalf("an older client's full update: %d %s", w.Code, w.Body)
	}
	if ids := listed(t, taskReq(t, g.h, "GET", "/v1/projects/alpha/tasks", g.alice, "", "")); len(ids) != 0 {
		t.Fatalf("an older client's list shows %v", ids)
	}
	// A feature moves to DONE only with evidence.
	if w := taskReq(t, g.h, "PUT", "/v1/tasks/"+f1, g.alice, g.key(), fmt.Sprintf(`{"title":"feature one","state":"DONE","epic":%q,"expected_revision":2}`, g.goal)); w.Code != 400 || !strings.Contains(w.Body.String(), "evidence") {
		t.Fatalf("DONE without evidence: %d %s", w.Code, w.Body)
	}
}

// A member's read of a feature's text for an offer answers as not found.
func TestTaskReadAnswersAFeatureAsNotFound(t *testing.T) {
	g := newTaskReadRig(t)
	db, err := g.s.reg.OpenExisting("beta")
	if err != nil {
		t.Fatal(err)
	}
	goal, err := db.CreateEpic(store.EpicContent{Title: "goal"}, boardActor, "tf-goal")
	if err != nil {
		t.Fatal(err)
	}
	f, err := db.CreateFeature(store.TaskContent{Title: "a feature", State: "READY", Epic: goal.ID}, store.FeatureLink{Goal: goal.ID, Position: 1, Mark: "optional"}, boardActor, "tf-1")
	if err != nil {
		t.Fatal(err)
	}
	if r := g.readTask(t, g.taskRead, "beta", f.ID); r.status != 404 || r.code() != "task_not_found" || strings.Contains(string(r.body), "a feature") {
		t.Fatalf("a feature's text: %d %s", r.status, r.body)
	}
	g.auditHas(t, "task.read.refused.task_not_found", "peer:aicrew-example", "credential="+g.taskReadID, "reason=feature")
	// Turned back into a task, it reads as work again.
	if _, err := db.SetFeature(f.ID, nil, true, f.Revision, boardActor, "tf-2"); err != nil {
		t.Fatal(err)
	}
	if v := g.text(t, "beta", f.ID); v.Title != "a feature" {
		t.Fatalf("after the way back: %+v", v)
	}
}

// An ambiguous prefix, of the task to confirm or of the feature filter, is
// a conflict naming the matches, as on every task route.
func TestFeatureRoutesRefuseAnAmbiguousPrefix(t *testing.T) {
	g := newFeatureRig(t)
	f1 := g.feature(t, "feature one", 1)
	work := g.task(t, "work", "READY")
	cloneTask(t, g.reg, "alpha", "alpha", work, sibling(work))
	cloneTask(t, g.reg, "alpha", "alpha", f1, sibling(f1))
	w := g.confirm(t, g.alice, f1, g.key(), fmt.Sprintf(`{"task":%q,"expected_revision":1}`, work[:35]))
	if w.Code != 409 || !strings.Contains(w.Body.String(), "ambiguous id prefix") || !strings.Contains(w.Body.String(), work) {
		t.Fatalf("an ambiguous task: %d %s", w.Code, w.Body)
	}
	w = taskReq(t, g.h, "GET", "/v1/projects/alpha/tasks?feature="+f1[:35], g.alice, "", "")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "ambiguous id prefix") {
		t.Fatalf("an ambiguous feature filter: %d %s", w.Code, w.Body)
	}
}

// list_features leaves archived features out unless asked, and caps the
// list at 100 with truncated set.
func TestListFeaturesArchivedAndCap(t *testing.T) {
	g := newFeatureRig(t)
	db, err := g.reg.OpenExisting("alpha")
	if err != nil {
		t.Fatal(err)
	}
	admin := store.TaskActor{Kind: "admin", Name: "host-admin"}
	link := store.FeatureLink{Goal: g.goal, Position: 1, Mark: "optional"}
	old, err := db.CreateFeature(store.TaskContent{Title: "done long ago", State: "DONE", Epic: g.goal, Archived: true,
		EvidenceRefs: []store.TaskRef{{Kind: "text", Ref: "it shipped"}}}, link, admin, "archived")
	if err != nil {
		t.Fatal(err)
	}
	// A task of the goal and a feature of another goal are not its features.
	if _, err := db.CreateTask(store.TaskContent{Title: "work of the goal", Epic: g.goal}, admin, "goal-task"); err != nil {
		t.Fatal(err)
	}
	other, err := db.CreateEpic(store.EpicContent{Title: "another goal"}, admin, "other-goal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateFeature(store.TaskContent{Title: "elsewhere", Epic: other.ID}, store.FeatureLink{Goal: other.ID, Position: 1, Mark: "optional"}, admin, "other-feature"); err != nil {
		t.Fatal(err)
	}
	if got := g.features(t, ""); len(got.Features) != 0 {
		t.Fatalf("an archived feature, a task or another goal's feature is listed: %+v", got)
	}
	if got := g.features(t, "?include_archived=true"); len(got.Features) != 1 || got.Features[0].ID != old.ID || !got.Features[0].Archived {
		t.Fatalf("include_archived: %+v", got)
	}
	var page struct {
		Features  []featureRow `json:"features"`
		Truncated bool         `json:"truncated"`
	}
	read := func() {
		t.Helper()
		w := taskReq(t, g.h, "GET", "/v1/projects/alpha/epics/"+g.goal+"/features", g.alice, "", "")
		page.Truncated = false
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatalf("list_features: %d %s", w.Code, w.Body)
		}
	}
	for i := 0; i < maxGoalFeatures; i++ {
		link.Position = i + 1
		if _, err := db.CreateFeature(store.TaskContent{Title: "f", Epic: g.goal}, link, admin, fmt.Sprintf("cap-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if read(); len(page.Features) != maxGoalFeatures || page.Truncated {
		t.Fatalf("exactly the cap: %d, truncated %v", len(page.Features), page.Truncated)
	}
	if _, err := db.CreateFeature(store.TaskContent{Title: "f", Epic: g.goal}, link, admin, "cap-over"); err != nil {
		t.Fatal(err)
	}
	if read(); len(page.Features) != maxGoalFeatures || !page.Truncated {
		t.Fatalf("over the cap: %d, truncated %v", len(page.Features), page.Truncated)
	}
}
