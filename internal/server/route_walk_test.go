package server

// Task 01a1dd8c-4919: every ordinary route answers a project the caller
// cannot read exactly as it answers a project that does not exist, and a
// task in such a project as a task that does not exist. The walk compares
// the two answers on every route of the table; a difference fails unless
// the pattern is in walkExceptions, and every exception is named in
// docs/DESIGN-access-control.md.

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// walkExceptions are the ordinary routes allowed to tell an existing
// unreadable project from an unknown one outside team mode, each with its
// reason; the design doc lists the same patterns.
var walkExceptions = map[string]string{}

// walkUnknown is a well-formed task, epic and comment id no store holds.
const walkUnknown = "01a1ffff-ffff-7fff-8fff-ffffffffffff"

// walkVolatile are the fields that differ between any two requests.
var walkVolatile = regexp.MustCompile(`"(correlation_id|request_id)":"[^"]*"`)

// walkFill puts the project and the id into a route pattern.
func walkFill(pattern, project, id string) string {
	return strings.NewReplacer("{p}", project, "{id}", id, "{task_id}", id, "{task}", id, "{e}", walkUnknown,
		"{c}", walkUnknown, "{team}", "t1", "{attempt}", walkUnknown, "{operation}", "claim", "{request_key}", "k1", "{name}", "RUNBOOK").Replace(pattern)
}

// walkCall sends one request as token and returns the status and the body
// with the volatile fields and the given name removed.
func walkCall(f *taskFixture, method, path, token, key, body, name string) (int, string) {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Idempotency-Key", key)
	r.Header.Set(reservationVersionHeader, "1")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	out := walkVolatile.ReplaceAllString(w.Body.String(), "")
	return w.Code, strings.TrimSpace(strings.ReplaceAll(out, name, "X"))
}

func TestUngrantedAnswersAsUnknownOnEveryRoute(t *testing.T) {
	f := newTaskFixture(t)
	w := taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.admin, "walk-1", taskBody)
	var task struct{ ID string }
	if json.Unmarshal(w.Body.Bytes(), &task); task.ID == "" {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var patterns []string
	for p := range ordinaryRoutes {
		patterns = append(patterns, p)
	}
	sort.Strings(patterns)
	walked := 0
	for i, pattern := range patterns {
		method, path, _ := strings.Cut(pattern, " ")
		if !strings.Contains(path, "{") {
			continue // no project or task in the path; the identity route is below
		}
		body := "{}"
		if method == "GET" {
			body = ""
		}
		// A project in the path: alpha (exists, not granted) against a
		// project that does not exist. A task id alone: alpha's task
		// against an id no project holds.
		existing, unknown, name, other := walkFill(path, "alpha", task.ID), walkFill(path, "nope", task.ID), "alpha", "nope"
		if !strings.Contains(path, "{p}") {
			unknown, name, other = walkFill(path, "alpha", walkUnknown), task.ID, walkUnknown
		}
		ce, be := walkCall(f, method, existing, f.stranger, fmt.Sprintf("we%d", i), body, name)
		cu, bu := walkCall(f, method, unknown, f.stranger, fmt.Sprintf("wu%d", i), body, other)
		walked++
		if (ce != cu || be != bu) && walkExceptions[pattern] == "" {
			t.Errorf("%s tells an unreadable project from an unknown one:\n  existing %d %.300s\n  unknown  %d %.300s", pattern, ce, be, cu, bu)
		}
	}
	if walked < 40 {
		t.Fatalf("walked %d routes; the table has %d", walked, len(ordinaryRoutes))
	}
	// The identity route's ?project= is the one project-naming read
	// without the project in the path.
	ce, be := walkCall(f, "GET", "/v1/access/identity?project=alpha", f.stranger, "wi1", "", "alpha")
	cu, bu := walkCall(f, "GET", "/v1/access/identity?project=nope", f.stranger, "wi2", "", "nope")
	if ce != 404 || ce != cu || be != bu {
		t.Errorf("identity ?project=: existing %d %s, unknown %d %s", ce, be, cu, bu)
	}
}

// Every exception the walk allows is in the design doc, under the pattern.
func TestWalkExceptionsAreInTheDesign(t *testing.T) {
	doc, err := os.ReadFile("../../docs/DESIGN-access-control.md")
	if err != nil {
		t.Fatal(err)
	}
	for pattern := range walkExceptions {
		if !strings.Contains(string(doc), "`"+pattern+"`") {
			t.Errorf("%s is a walk exception the design doc does not list", pattern)
		}
	}
}

// Team mode keeps its answers (the architect's decision on task
// 01a1dd8c-4919): a team conversation is refused an existing project its
// profile is not granted with grant_denied on every project read it
// serves, as before the task; the reservation routes' team answers are
// pinned by the reservation tests.
func TestTeamModeUngrantedAnswersUnchanged(t *testing.T) {
	g := newTeamRig(t)
	walked := 0
	for _, pattern := range teamRoutes {
		method, path, _ := strings.Cut(pattern, " ")
		if method != "GET" || !strings.Contains(path, "{p}") || strings.Contains(path, "{task_id}") {
			continue
		}
		walked++
		p := strings.NewReplacer("{p}", "beta", "{e}", walkUnknown, "{name}", "RUNBOOK").Replace(path)
		if r := g.team(t, method, p); r.status != 403 || r.code() != "grant_denied" {
			t.Errorf("team %s in beta: %d %s", pattern, r.status, r.body)
		}
	}
	if walked < 10 {
		t.Fatalf("walked %d team routes", walked)
	}
}
