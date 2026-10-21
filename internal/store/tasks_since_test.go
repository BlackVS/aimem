package store

// TaskFilter.UpdatedSince keeps the tasks updated at or after a time, in
// any order and with a search, and refuses a malformed time.

import (
	"errors"
	"strings"
	"testing"
)

func TestTaskListUpdatedSince(t *testing.T) {
	_, db := taskDB(t)
	for i, r := range []struct{ title, updated string }{
		{"kiwi a", "2026-10-21T07:59:59Z"},
		{"kiwi b", "2026-10-21T08:00:00Z"},
		{"kiwi c", "2026-10-21T08:00:01Z"},
		{"kiwi d", "2026-10-21T09:30:00Z"},
	} {
		task := mustCreate(t, db, r.title, "k-since-"+string(rune('a'+i)))
		if _, err := db.sql.Exec(`UPDATE tasks SET body=json_set(body,'$.updated_at',?) WHERE id=?`, r.updated, task.ID); err != nil {
			t.Fatal(err)
		}
	}
	titles := func(f TaskFilter) string {
		t.Helper()
		var out []string
		for {
			page, err := db.ListTasks(f)
			if err != nil {
				t.Fatalf("%+v: %v", f, err)
			}
			for _, s := range page.Tasks {
				out = append(out, s.Title)
			}
			if page.Next == "" {
				return strings.Join(out, ",")
			}
			f.After = page.Next
		}
	}
	for since, want := range map[string]string{
		"2026-10-21T08:00:00Z":      "kiwi b,kiwi c,kiwi d", // at or after: the boundary is in
		"2026-10-21T10:00:00+02:00": "kiwi b,kiwi c,kiwi d", // an offset is the same instant
		"2026-10-21T08:00:00.1Z":    "kiwi c,kiwi d",        // a fraction rounds up: 08:00:00 is before it
		"2026-10-21T09:30:00Z":      "kiwi d",
		"2026-10-22T00:00:00Z":      "",
	} {
		if got := titles(TaskFilter{UpdatedSince: since, Limit: 1}); got != want {
			t.Fatalf("since %s: %q, want %q", since, got, want)
		}
	}
	if got := titles(TaskFilter{UpdatedSince: "2026-10-21T08:00:00Z", Order: TaskOrderUpdatedDesc, Limit: 2}); got != "kiwi d,kiwi c,kiwi b" {
		t.Fatalf("since + updated_desc paged: %q", got)
	}
	if got := titles(TaskFilter{UpdatedSince: "2026-10-21T08:00:01Z", Query: "kiwi", Limit: 1}); got != "kiwi c,kiwi d" && got != "kiwi d,kiwi c" {
		t.Fatalf("since + search: %q", got)
	}
	for _, bad := range []string{"yesterday", "2026-10-21", "2026-10-21 08:00:00"} {
		if _, err := db.ListTasks(TaskFilter{UpdatedSince: bad}); !errors.Is(err, ErrTaskInvalid) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}
