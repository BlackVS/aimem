package mcp

// list_tasks: updated_since and order reach the hub's list, the default
// page is unchanged, and mine is the caller's own user identity.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"aimem/internal/store"
)

func TestListTasksSinceOrderAndMine(t *testing.T) {
	f := newHub(t)
	db, err := f.reg.Open("alpha")
	if err != nil {
		t.Fatal(err)
	}
	admin := store.TaskActor{Kind: "admin", Name: "host-admin"}
	mk := func(title string, assignee *store.TaskAssignee) store.Task {
		t.Helper()
		task, err := db.CreateTask(store.TaskContent{Title: title, State: "READY", Assignee: assignee}, admin, "k-"+title)
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	old := mk("old", nil)
	// updated_at has whole seconds: start the next ones in a later second
	// than old's, so updated_since tells them apart.
	for time.Now().UTC().Format(time.RFC3339) <= old.UpdatedAt {
		time.Sleep(20 * time.Millisecond)
	}
	alices := mk("alices", &store.TaskAssignee{Kind: "user", ID: f.aliceID})
	mk("bobs", &store.TaskAssignee{Kind: "user", ID: f.bobID})
	titles := func(token string, args map[string]any) ([]string, string) {
		t.Helper()
		args["project"] = "alpha"
		text, isErr := toolText(f.rpc(t, token, "tools/call", map[string]any{"name": "list_tasks", "arguments": args}))
		if isErr {
			t.Fatalf("list_tasks %v: %s", args, text)
		}
		var p struct {
			Tasks []struct {
				Title string `json:"title"`
			} `json:"tasks"`
			Next string `json:"next_cursor"`
		}
		json.Unmarshal([]byte(text), &p)
		var out []string
		for _, r := range p.Tasks {
			out = append(out, r.Title)
		}
		return out, p.Next
	}
	// The default page is unchanged: creation order.
	if got, _ := titles(f.reader, map[string]any{}); strings.Join(got, ",") != "old,alices,bobs" {
		t.Fatalf("default page: %v", got)
	}
	// What changed since a time, newest first, in one call.
	// alices and bobs may share a second; updated_desc breaks the tie by id, newest first.
	if got, _ := titles(f.reader, map[string]any{"updated_since": alices.UpdatedAt, "order": "updated_desc"}); strings.Join(got, ",") != "bobs,alices" {
		t.Fatalf("since + updated_desc: %v", got)
	}
	// A fractional time keeps the "at or after" meaning: half a second
	// into old's second excludes old.
	if got, _ := titles(f.reader, map[string]any{"updated_since": strings.TrimSuffix(old.UpdatedAt, "Z") + ".5Z"}); strings.Join(got, ",") != "alices,bobs" {
		t.Fatalf("fractional since: %v", got)
	}
	// Paging with the order's cursor.
	first, next := titles(f.reader, map[string]any{"order": "title", "limit": 2})
	rest, _ := titles(f.reader, map[string]any{"order": "title", "limit": 2, "after": next})
	if strings.Join(first, ",")+"|"+strings.Join(rest, ",") != "alices,bobs|old" {
		t.Fatalf("order=title paged: %v %v", first, rest)
	}
	// mine is each caller's own user identity, combined with the filters.
	if got, _ := titles(f.alice, map[string]any{"mine": true}); strings.Join(got, ",") != "alices" {
		t.Fatalf("alice's mine: %v", got)
	}
	if got, _ := titles(f.stranger, map[string]any{"mine": true}); strings.Join(got, ",") != "bobs" {
		t.Fatalf("bob's mine: %v", got)
	}
	if got, _ := titles(f.alice, map[string]any{"mine": true, "updated_since": time.Now().UTC().Add(time.Hour).Format(time.RFC3339)}); len(got) != 0 {
		t.Fatalf("mine + since: %v", got)
	}
	if got, _ := titles(f.alice, map[string]any{"mine": true, "state": "BACKLOG"}); len(got) != 0 {
		t.Fatalf("mine + state: %v", got)
	}
	for _, c := range []struct {
		token string
		args  map[string]any
		want  string
	}{
		{f.alice, map[string]any{"mine": true, "assignee": map[string]string{"kind": "user", "id": f.aliceID}}, "mine and assignee are exclusive"},
		{f.env, map[string]any{"mine": true}, "mine needs a user credential"},
		{f.reader, map[string]any{"updated_since": "yesterday"}, "RFC 3339"},
		{f.reader, map[string]any{"order": "newest"}, "invalid task order"},
	} {
		c.args["project"] = "alpha"
		if text, isErr := toolText(f.rpc(t, c.token, "tools/call", map[string]any{"name": "list_tasks", "arguments": c.args})); !isErr || !strings.Contains(text, c.want) {
			t.Fatalf("%v: %v %s", c.args, isErr, text)
		}
	}
	for _, args := range []map[string]any{{"query": "x", "order": "title"}, {"query": "x", "mine": true}, {"query": "x", "updated_since": "2026-10-21T00:00:00Z"}} {
		args["project"] = "alpha"
		if text, isErr := toolText(f.rpc(t, f.reader, "tools/call", map[string]any{"name": "search_tasks", "arguments": args})); !isErr || !strings.Contains(text, "belong to list_tasks") {
			t.Fatalf("search_tasks %v: %v %s", args, isErr, text)
		}
	}
}
