package mcp

// list_tasks carries the board's shape on its first page: counts under the
// page's assignee, epic and mine filters (not its state), absent on a later
// page and on a search; a hub without the counts route leaves the page as
// it was.

import (
	"encoding/json"
	"errors"
	"net/url"
	"reflect"
	"testing"

	"aimem/internal/store"
)

func TestListTasksCounts(t *testing.T) {
	f := newHub(t)
	db, err := f.reg.Open("alpha")
	if err != nil {
		t.Fatal(err)
	}
	admin := store.TaskActor{Kind: "admin", Name: "host-admin"}
	epic, err := db.CreateEpic(store.EpicContent{Title: "release"}, admin, "k-epic")
	if err != nil {
		t.Fatal(err)
	}
	alices := &store.TaskAssignee{Kind: "user", ID: f.aliceID}
	for i, c := range []struct {
		state    string
		assignee *store.TaskAssignee
		epic     string
	}{
		{"READY", nil, ""},
		{"READY", alices, epic.ID},
		{"BLOCKED", alices, ""},
		{"DONE", &store.TaskAssignee{Kind: "user", ID: f.bobID}, epic.ID},
	} {
		if _, err := db.CreateTask(store.TaskContent{Title: "t", State: c.state, Assignee: c.assignee, Epic: c.epic}, admin, "k-"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	type counts struct {
		Counts   map[string]int `json:"counts"`
		Archived int            `json:"archived"`
		Total    int            `json:"total"`
	}
	shape := func(total int, states map[string]int) *counts {
		out := &counts{Counts: map[string]int{}, Total: total}
		for _, st := range store.TaskStates {
			out.Counts[st] = states[st]
		}
		return out
	}
	page := func(token, tool string, args map[string]any) (*counts, string, int) {
		t.Helper()
		args["project"] = "alpha"
		text, isErr := toolText(f.rpc(t, token, "tools/call", map[string]any{"name": tool, "arguments": args}))
		if isErr {
			t.Fatalf("%s %v: %s", tool, args, text)
		}
		var p struct {
			Counts *counts           `json:"counts"`
			Next   string            `json:"next_cursor"`
			Tasks  []json.RawMessage `json:"tasks"`
		}
		if err := json.Unmarshal([]byte(text), &p); err != nil {
			t.Fatalf("%s %v: %v: %s", tool, args, err, text)
		}
		return p.Counts, p.Next, len(p.Tasks)
	}
	// The first page carries the whole board's shape; a state filter
	// narrows the page, not the counts.
	all := shape(4, map[string]int{"READY": 2, "BLOCKED": 1, "DONE": 1})
	for _, args := range []map[string]any{{}, {"state": "BLOCKED"}, {"updated_since": "2999-01-01T00:00:00Z"}} {
		if got, _, _ := page(f.reader, "list_tasks", args); !reflect.DeepEqual(got, all) {
			t.Fatalf("first page %v: %+v", args, got)
		}
	}
	// The assignee, epic and mine filters narrow the counts.
	if got, _, _ := page(f.reader, "list_tasks", map[string]any{"epic": epic.ID}); !reflect.DeepEqual(got, shape(2, map[string]int{"READY": 1, "DONE": 1})) {
		t.Fatalf("epic: %+v", got)
	}
	if got, _, _ := page(f.alice, "list_tasks", map[string]any{"mine": true}); !reflect.DeepEqual(got, shape(2, map[string]int{"READY": 1, "BLOCKED": 1})) {
		t.Fatalf("mine: %+v", got)
	}
	if got, _, _ := page(f.reader, "list_tasks", map[string]any{"assignee": map[string]string{"kind": "user", "id": f.aliceID}, "epic": epic.ID}); !reflect.DeepEqual(got, shape(1, map[string]int{"READY": 1})) {
		t.Fatalf("assignee + epic: %+v", got)
	}
	// A later page has none; the first page's rows are all still there.
	got, next, n := page(f.reader, "list_tasks", map[string]any{"limit": 3})
	if got == nil || next == "" || n != 3 {
		t.Fatalf("first of two pages: %+v %q %d", got, next, n)
	}
	if got, _, n := page(f.reader, "list_tasks", map[string]any{"limit": 3, "after": next}); got != nil || n != 1 {
		t.Fatalf("later page: %+v %d", got, n)
	}
	// A search is ranked by relevance and is not the board: no counts.
	if got, _, _ := page(f.reader, "search_tasks", map[string]any{"query": "t"}); got != nil {
		t.Fatalf("search: %+v", got)
	}
}

func TestWithCountsKeepsThePageWhenTheHubCannotCount(t *testing.T) {
	const page = "{\n  \"project\": \"alpha\",\n  \"tasks\": []\n}"
	for name, call := range map[string]func(method, path string, body any, key string) (string, error){
		"an older hub":       func(string, string, any, string) (string, error) { return "", errors.New("HTTP 404") },
		"a malformed answer": func(string, string, any, string) (string, error) { return `{"total":1}`, nil },
	} {
		if got := withCounts(call, "alpha", url.Values{}, page); got != page {
			t.Fatalf("%s: %s", name, got)
		}
	}
}
