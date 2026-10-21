package server

// GET /v1/projects/{p}/tasks?updated_since= filters by update time, with
// and without order=updated_desc, pages, and refuses a malformed time.

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"
)

func TestTaskListRouteUpdatedSince(t *testing.T) {
	f := newTaskFixture(t)
	create := func(title, key string) string {
		t.Helper()
		w := taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, key, `{"title":"`+title+`"}`)
		var task struct {
			UpdatedAt string `json:"updated_at"`
		}
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &task) != nil {
			t.Fatalf("create %s: %d %s", title, w.Code, w.Body)
		}
		return task.UpdatedAt
	}
	first := create("first", "u1")
	// updated_at has whole seconds: the next tasks start in a later second.
	for time.Now().UTC().Format(time.RFC3339) <= first {
		time.Sleep(20 * time.Millisecond)
	}
	since := create("second", "u2")
	create("third", "u3")
	list := func(query string) (titles []string, next string) {
		t.Helper()
		w := taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks?"+query, f.bob, "", "")
		var p struct {
			Tasks []struct {
				Title string `json:"title"`
			} `json:"tasks"`
			Next string `json:"next_cursor"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil {
			t.Fatalf("%s: %d %s", query, w.Code, w.Body)
		}
		for _, r := range p.Tasks {
			titles = append(titles, r.Title)
		}
		return titles, p.Next
	}
	s := "updated_since=" + url.QueryEscape(since)
	if got, _ := list(s); len(got) != 2 || got[0] != "second" || got[1] != "third" {
		t.Fatalf("since: %v", got)
	}
	// Newest first, one per page: third, then second, then the end.
	got, next := list(s + "&order=updated_desc&limit=1")
	if len(got) != 1 || got[0] != "third" || next == "" {
		t.Fatalf("since + updated_desc page 1: %v %q", got, next)
	}
	got, next = list(s + "&order=updated_desc&limit=1&after=" + url.QueryEscape(next))
	if len(got) != 1 || got[0] != "second" {
		t.Fatalf("since + updated_desc page 2: %v %q", got, next)
	}
	if next != "" {
		if got, _ := list(s + "&order=updated_desc&limit=1&after=" + url.QueryEscape(next)); len(got) != 0 {
			t.Fatalf("since + updated_desc page 3: %v", got)
		}
	}
	for _, q := range []string{"updated_since=yesterday", "updated_since=2026-10-21"} {
		if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks?"+q, f.bob, "", ""); w.Code != 400 {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body)
		}
	}
}
