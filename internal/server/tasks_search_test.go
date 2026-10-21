package server

// GET /v1/projects/{p}/tasks?q= ranks the matches and says where each hit
// was; bad terms and q with order are 400s.

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestTaskListRouteSearch(t *testing.T) {
	f := newTaskFixture(t)
	for i, title := range []string{"rotate the gecko keys", "unrelated", "gecko gecko audit"} {
		if w := taskReq(t, f.h, "POST", "/v1/projects/alpha/tasks", f.alice, "s"+string(rune('1'+i)), `{"title":"`+title+`"}`); w.Code != 201 {
			t.Fatalf("create %s: %d %s", title, w.Code, w.Body)
		}
	}
	var p struct {
		Tasks []struct {
			Title   string `json:"title"`
			Matched string `json:"matched"`
			Snippet string `json:"snippet"`
		} `json:"tasks"`
	}
	w := taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks?q=GECKO", f.bob, "", "")
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &p) != nil || len(p.Tasks) != 2 {
		t.Fatalf("search: %d %s", w.Code, w.Body)
	}
	for _, row := range p.Tasks {
		if row.Matched != "task" || !strings.Contains(row.Snippet, "[gecko]") {
			t.Fatalf("row: %+v", row)
		}
	}
	var ids []string
	w = taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks?q=gecko&limit=1", f.bob, "", "")
	var first struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	if json.Unmarshal(w.Body.Bytes(), &first) != nil || len(first.Tasks) != 1 {
		t.Fatalf("first hit: %d %s", w.Code, w.Body)
	}
	ids = append(ids, first.Tasks[0].ID)
	if w := taskReq(t, f.h, "POST", "/v1/tasks/"+ids[0]+"/comments", f.alice, "sc1", `{"body":"the iguana was fed"}`); w.Code != 201 {
		t.Fatalf("comment: %d %s", w.Code, w.Body)
	}
	for q, want := range map[string]string{
		"q=iguana":                `"matched":"comment"`, // a comment hit
		"q=iguana&state=BACKLOG":  `"matched":"comment"`, // with a filter that keeps it
		"q=iguana&state=DONE":     `"tasks":[]`,          // with a filter that drops it
		"q=no-such-word-anywhere": `"tasks":[]`,          // nothing matches
	} {
		w := taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks?"+q, f.bob, "", "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), want) {
			t.Fatalf("%s: %d %s", q, w.Code, w.Body)
		}
	}
	for _, q := range []string{"q=gecko&order=title", "q=%20", "q=", "q=&order=title", "state=READY&q", "q=" + url.QueryEscape(strings.Repeat("x", 257))} {
		if w := taskReq(t, f.h, "GET", "/v1/projects/alpha/tasks?"+q, f.bob, "", ""); w.Code != 400 {
			t.Fatalf("%.40s: %d %s", q, w.Code, w.Body)
		}
	}
}
