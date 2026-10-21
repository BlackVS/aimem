package server

// GET /v1/tasks/{id}/comments?order=desc reads the newest first and pages
// toward older comments; the default order and its cursor are unchanged.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestTaskCommentsRouteOrder(t *testing.T) {
	f := newTaskFixture(t)
	w := taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, "co1", `{"title":"discussed"}`)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	var task struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &task)
	for i := 1; i <= 5; i++ {
		if w := taskReq(t, f.h, "POST", "/v1/tasks/"+task.ID+"/comments", f.alice, fmt.Sprintf("cc%d", i), fmt.Sprintf(`{"body":"note %d"}`, i)); w.Code != 201 {
			t.Fatalf("comment %d: %d %s", i, w.Code, w.Body)
		}
	}
	read := func(query string) (bodies []string, next int64) {
		t.Helper()
		w := taskReq(t, f.h, "GET", "/v1/tasks/"+task.ID+"/comments?"+query, f.bob, "", "")
		var p struct {
			Comments []struct {
				Body string `json:"body"`
			} `json:"comments"`
			Next int64 `json:"next_cursor"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body)
		}
		for _, c := range p.Comments {
			bodies = append(bodies, c.Body)
		}
		return bodies, p.Next
	}
	pageAll := func(order string) string {
		var all []string
		after := int64(0)
		for n := 0; n < 5; n++ {
			got, next := read(fmt.Sprintf("%slimit=2&after=%d", order, after))
			all = append(all, got...)
			if next == 0 {
				return strings.Join(all, ",")
			}
			after = next
		}
		t.Fatalf("%q did not end", order)
		return ""
	}
	if got := pageAll(""); got != "note 1,note 2,note 3,note 4,note 5" {
		t.Fatalf("default order: %s", got)
	}
	if got := pageAll("order=asc&"); got != "note 1,note 2,note 3,note 4,note 5" {
		t.Fatalf("asc: %s", got)
	}
	if got := pageAll("order=desc&"); got != "note 5,note 4,note 3,note 2,note 1" {
		t.Fatalf("desc: %s", got)
	}
	// The newest one alone, and its cursor leads to the one before it.
	got, next := read("order=desc&limit=1")
	if strings.Join(got, ",") != "note 5" || next == 0 {
		t.Fatalf("newest: %v %d", got, next)
	}
	if got, _ := read(fmt.Sprintf("order=desc&limit=1&after=%d", next)); strings.Join(got, ",") != "note 4" {
		t.Fatalf("one older: %v", got)
	}
	if w := taskReq(t, f.h, "GET", "/v1/tasks/"+task.ID+"/comments?order=newest", f.bob, "", ""); w.Code != 400 {
		t.Fatalf("unknown order: %d %s", w.Code, w.Body)
	}
}
