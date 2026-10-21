package server

// GET /v1/projects/{p}/tasks?order= pages in the asked order with the
// cursor it issues; the default order keeps the bare task id as cursor.

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestTaskListRouteOrder(t *testing.T) {
	f := newTaskFixture(t)
	for i, title := range []string{"bravo", "Alpha", "charlie"} {
		if w := taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, "o"+string(rune('1'+i)), `{"title":"`+title+`"}`); w.Code != 201 {
			t.Fatalf("create %s: %d %s", title, w.Code, w.Body)
		}
	}
	type page struct {
		Tasks []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"tasks"`
		Next string `json:"next_cursor"`
	}
	list := func(query string) (page, int, string) {
		t.Helper()
		w := taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks?"+query, f.bob, "", "")
		var p page
		json.Unmarshal(w.Body.Bytes(), &p)
		return p, w.Code, w.Body.String()
	}
	var titles []string
	after := ""
	for {
		p, code, body := list("order=title&limit=2&after=" + url.QueryEscape(after))
		if code != 200 {
			t.Fatalf("order=title: %d %s", code, body)
		}
		for _, row := range p.Tasks {
			titles = append(titles, row.Title)
		}
		if p.Next == "" {
			break
		}
		after = p.Next
	}
	if strings.Join(titles, ",") != "Alpha,bravo,charlie" {
		t.Fatalf("order=title paged: %v", titles)
	}
	// The default order is creation order, and its cursor the bare id.
	p, code, body := list("limit=2")
	if code != 200 || len(p.Tasks) != 2 || p.Tasks[0].Title != "bravo" || p.Next != p.Tasks[1].ID {
		t.Fatalf("default order: %d %s", code, body)
	}
	for _, q := range []string{"order=updated_desc", "order=updated_asc", "order=state", "order=id"} {
		if p, code, body := list(q); code != 200 || len(p.Tasks) != 3 {
			t.Fatalf("%s: %d %s", q, code, body)
		}
	}
	updated, _, _ := list("order=updated_desc&limit=1")
	for _, q := range []string{"order=newest", "order=title&after=" + url.QueryEscape(updated.Next), "order=state&after=" + p.Next} {
		if _, code, body := list(q); code != 400 {
			t.Fatalf("%s: %d %s", q, code, body)
		}
	}
}
