package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"aimem/internal/store"
	"aimem/internal/textcheck"
	"aimem/internal/writingrule"
)

// fakeJudge answers the second stage's question without a model.
type fakeJudge struct {
	readable bool
	err      error
	calls    int
	asked    string
}

func (j *fakeJudge) Readable(_ context.Context, span, around string) (bool, error) {
	j.calls++
	j.asked = span + "\n" + around
	return j.readable, j.err
}

type textRefusalBody struct {
	Error, Code, Message, Field, Span, Reason, Section, Stage string
	Fix                                                       string `json:"next_action"`
}

func refusedText(t *testing.T, w *httptest.ResponseRecorder, field, span, stage string) {
	t.Helper()
	var b textRefusalBody
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil || w.Code != 422 {
		t.Fatalf("want a 422 refusal, got %d %s", w.Code, w.Body)
	}
	if b.Code != "unreadable_text" || b.Field != field || b.Span != span || b.Stage != stage ||
		b.Section != writingrule.RuleSection || b.Fix == "" || b.Reason == "" {
		t.Fatalf("refusal %+v; want field %q, span %q, stage %q", b, field, span, stage)
	}
	// The error string stands on its own: it is all an MCP client sees.
	for _, part := range []string{"unreadable_text: " + field + ": ", span, b.Fix, b.Section} {
		if !strings.Contains(b.Error, part) {
			t.Fatalf("error %q lacks %q", b.Error, part)
		}
	}
	decided := map[string]string{stageFilter: "Decided by the filter alone.", stageFilterModel: "Decided by the filter, confirmed by the model."}[stage]
	if !strings.HasSuffix(b.Message, decided) || b.Error != "unreadable_text: "+b.Message+" Next: "+b.Fix {
		t.Fatalf("message %q does not say %q, or error %q is not code, message and fix", b.Message, decided, b.Error)
	}
}

func comments(t *testing.T, f *taskFixture, id string) int {
	t.Helper()
	db, err := f.reg.Open("alpha")
	if err != nil {
		t.Fatal(err)
	}
	page, err := db.TaskCommentsOrdered(id, 0, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	return len(page.Comments)
}

func TestTaskWritesRefuseUnreadableText(t *testing.T) {
	f := newTaskFixture(t)
	h := f.h

	// A field on create; nothing is stored.
	w := taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "c1", `{"title":"Ship the fix","objective":"Checkpoint74files written","state":"READY"}`)
	refusedText(t, w, "objective", "Checkpoint74files", stageFilter)
	db, _ := f.reg.Open("alpha")
	if page, _ := db.ListTasks(store.TaskFilter{}); len(page.Tasks) != 0 {
		t.Fatalf("a refused create stored %d tasks", len(page.Tasks))
	}

	task := decodeTask(t, taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "c2", taskBody))

	// A comment; nothing is stored.
	w = taskReq(t, h, "POST", "/v1/tasks/"+task.ID+"/comments", f.alice, "m1", `{"body":"CI409 SUCCESS99s, merged."}`)
	refusedText(t, w, "body", "CI409", stageFilter)
	if n := comments(t, f, task.ID); n != 0 {
		t.Fatalf("a refused comment stored %d comments", n)
	}
	// A technical comment passes.
	tech := "Merged as d2baf13286; CI run 40089890103 passed on macos-14. The fix is in `internal/wiring/project.go`, released in v0.10.13, see https://github.com/BlackVS/aimem/pull/282."
	raw, _ := json.Marshal(map[string]string{"body": tech})
	if w := taskReq(t, h, "POST", "/v1/tasks/"+task.ID+"/comments", f.alice, "m2", string(raw)); w.Code != 201 {
		t.Fatalf("technical comment: %d %s", w.Code, w.Body)
	}

	// A field on update.
	w = taskReq(t, h, "PUT", "/v1/tasks/"+task.ID, f.alice, "u1", `{"title":"ship it","state":"BLOCKED","blocker":"BLOCKEDr4 on the hub","expected_revision":1}`)
	refusedText(t, w, "blocker", "BLOCKEDr4", stageFilter)

	// Triage writes next_action under the same check.
	w = taskReq(t, h, "POST", "/v1/tasks/"+task.ID+"/triage", f.alice, "t1", `{"expected_revision":1,"next_action":"Waiting on approval573."}`)
	refusedText(t, w, "next_action", "approval573", stageFilter)
	if got, _ := db.GetTask(task.ID); got.Revision != 1 {
		t.Fatalf("refused writes moved the task to revision %d", got.Revision)
	}

	// Text stored before the check existed blocks no unrelated update: only
	// the fields an update changes are checked.
	old := store.TaskContent{Title: "Legacy", Objective: "All11containersremoved", State: "READY"}
	legacy, err := db.CreateTask(old, store.TaskActor{Kind: "admin", Name: "host-admin"}, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if w := taskReq(t, h, "PUT", "/v1/tasks/"+legacy.ID, f.alice, "u2", `{"title":"Legacy","objective":"All11containersremoved","state":"IN_PROGRESS","expected_revision":1}`); w.Code != 200 {
		t.Fatalf("an update that keeps stored text: %d %s", w.Code, w.Body)
	}
}

func TestSecondStageCanOnlyRelaxTheFilter(t *testing.T) {
	f := newTaskFixture(t)
	h := f.h
	task := decodeTask(t, taskReq(t, h, "POST", "/v1/projects/alpha/tasks", f.alice, "c1", taskBody))
	post := func(key, body string) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]string{"body": body})
		return taskReq(t, h, "POST", "/v1/tasks/"+task.ID+"/comments", f.alice, key, string(raw))
	}

	// Text the filter passes never reaches the model.
	j := &fakeJudge{readable: false}
	f.s.judge = j
	if w := post("p1", "The worker pushed the fix to the branch."); w.Code != 201 {
		t.Fatalf("readable comment: %d %s", w.Code, w.Body)
	}
	if j.calls != 0 {
		t.Fatalf("the model was asked %d times about text the filter passed", j.calls)
	}

	// The model calls flagged text readable: stored.
	j.readable = true
	if w := post("p2", "Comment seq9999 records it; the run CI409 is linked."); w.Code != 201 {
		t.Fatalf("relaxed comment: %d %s", w.Code, w.Body)
	}
	if j.calls != 1 || !strings.HasPrefix(j.asked, "CI409\n") || !strings.Contains(j.asked, "the run CI409 is linked") {
		t.Fatalf("the model was asked %d times with %q", j.calls, j.asked)
	}

	// The model calls it shorthand: refused, naming both stages.
	j.readable = false
	refusedText(t, post("p3", "CI409 SUCCESS99s"), "body", "CI409", stageFilterModel)

	// No answer (a timeout, an error, the cap): the filter's verdict stands.
	j.err = errors.New("context deadline exceeded")
	refusedText(t, post("p4", "CI409 SUCCESS99s"), "body", "CI409", stageFilter)

	// No second stage at all: the filter alone.
	f.s.judge = nil
	refusedText(t, post("p5", "CI409 SUCCESS99s"), "body", "CI409", stageFilter)
	if n := comments(t, f, task.ID); n != 2 {
		t.Fatalf("%d comments stored; want the readable and the relaxed one", n)
	}
}

type healthTextCheck struct {
	TextCheck struct {
		Filter, State, Why string
	} `json:"text_check"`
}

func TestHealthNamesTheTextCheck(t *testing.T) {
	s, _ := testServer(t)
	w := req(t, s.Handler(), "GET", "/v1/health", "")
	var body healthTextCheck
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.TextCheck.Filter != "on" || body.TextCheck.State != "off" || body.TextCheck.Why == "" {
		t.Fatalf("health text_check: %s", w.Body)
	}
}

// AIMEM_TEXT_CHECK=off is the operator's way out of a false positive the
// tests did not foresee: task text is stored unchecked, and health says so.
func TestTextCheckSwitchedOffStoresUncheckedText(t *testing.T) {
	t.Setenv(textcheck.FilterKey, "off")
	f := newTaskFixture(t)
	w := taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, "c1", `{"title":"Ship the fix","objective":"Checkpoint74files written","state":"READY"}`)
	if w.Code != 201 {
		t.Fatalf("create with the check off: %d %s", w.Code, w.Body)
	}
	var task store.Task
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil {
		t.Fatal(err)
	}
	if w := taskReq(t, f.h, "POST", "/v1/tasks/"+task.ID+"/comments", f.alice, "m1", `{"body":"CI409 SUCCESS99s, merged."}`); w.Code != 201 {
		t.Fatalf("comment with the check off: %d %s", w.Code, w.Body)
	}
	var body healthTextCheck
	w = req(t, f.s.Handler(), "GET", "/v1/health", "")
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.TextCheck.Filter != "off" || !strings.Contains(body.TextCheck.Why, textcheck.FilterKey) {
		t.Fatalf("health with the check off: %s", w.Body)
	}
}
