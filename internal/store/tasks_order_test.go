package store

// ListTasks orders: each is total (ties broken by id), pages resume
// exactly after the last row under its order, and a cursor serves only
// the order that issued it.

import (
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestTaskListOrdersPageExactly(t *testing.T) {
	_, db := taskDB(t)
	// Titles differ in case, states spread over the lifecycle, and update
	// times tie, so every tie-break is exercised.
	rows := []struct{ title, state, updated string }{
		{"beta", "READY", "2026-10-21T08:00:00Z"},
		{"Alpha", "BACKLOG", "2026-10-21T09:00:00Z"},
		{"alpha", "DONE", "2026-10-21T09:00:00Z"},
		{"Gamma", "READY", "2026-10-21T07:00:00Z"},
		{"delta", "IN_PROGRESS", "2026-10-21T09:00:00Z"},
		{"Beta", "BACKLOG", "2026-10-20T23:59:59Z"},
		{"epsilon", "CANCELLED", "2026-10-21T08:00:00Z"},
		{"zeta", "READY", "2026-10-21T08:00:00Z"},
		{"eta", "REVIEW", "2026-10-19T12:00:00Z"},
	}
	var all []TaskSummary
	for i, r := range rows {
		task := mustCreate(t, db, r.title, "k-order-"+string(rune('a'+i)))
		if _, err := db.sql.Exec(`UPDATE tasks SET state=?, body=json_set(body,'$.state',?,'$.updated_at',?) WHERE id=?`, r.state, r.state, r.updated, task.ID); err != nil {
			t.Fatal(err)
		}
		all = append(all, TaskSummary{ID: task.ID, Title: r.title, State: r.state, UpdatedAt: r.updated})
	}
	want := map[string]func(a, b TaskSummary) bool{
		TaskOrderID: func(a, b TaskSummary) bool { return a.ID < b.ID },
		TaskOrderUpdatedDesc: func(a, b TaskSummary) bool {
			if a.UpdatedAt != b.UpdatedAt {
				return a.UpdatedAt > b.UpdatedAt
			}
			return a.ID > b.ID
		},
		TaskOrderUpdatedAsc: func(a, b TaskSummary) bool {
			if a.UpdatedAt != b.UpdatedAt {
				return a.UpdatedAt < b.UpdatedAt
			}
			return a.ID < b.ID
		},
		TaskOrderTitle: func(a, b TaskSummary) bool {
			if x, y := strings.ToLower(a.Title), strings.ToLower(b.Title); x != y {
				return x < y
			}
			return a.ID < b.ID
		},
		TaskOrderState: func(a, b TaskSummary) bool {
			if x, y := taskStateRank(a.State), taskStateRank(b.State); x != y {
				return x < y
			}
			if a.UpdatedAt != b.UpdatedAt {
				return a.UpdatedAt > b.UpdatedAt
			}
			return a.ID > b.ID
		},
	}
	ids := func(ts []TaskSummary) []string {
		out := make([]string, len(ts))
		for i, t := range ts {
			out[i] = t.ID
		}
		return out
	}
	cursors := map[string]string{}
	for order, less := range want {
		exp := slices.Clone(all)
		sort.Slice(exp, func(i, j int) bool { return less(exp[i], exp[j]) })
		// One page holds the whole order.
		page, err := db.ListTasks(TaskFilter{Order: order, Limit: 100})
		if err != nil || !slices.Equal(ids(page.Tasks), ids(exp)) || page.Next != "" {
			t.Fatalf("%s: %v\n got %v\nwant %v", order, err, ids(page.Tasks), ids(exp))
		}
		// Pages of two resume exactly after the last row, through every tie.
		var got []TaskSummary
		after := ""
		for {
			p, err := db.ListTasks(TaskFilter{Order: order, After: after, Limit: 2})
			if err != nil {
				t.Fatalf("%s after %q: %v", order, after, err)
			}
			got = append(got, p.Tasks...)
			if p.Next == "" {
				break
			}
			if after == "" {
				cursors[order] = p.Next
			}
			after = p.Next
		}
		if !slices.Equal(ids(got), ids(exp)) {
			t.Fatalf("%s paged:\n got %v\nwant %v", order, ids(got), ids(exp))
		}
	}
	// The default order is id, and its cursor is still the bare task id.
	if p, err := db.ListTasks(TaskFilter{Limit: 2}); err != nil || p.Next != p.Tasks[1].ID || cursors[TaskOrderID] != p.Next {
		t.Fatalf("default order cursor: %q %v", p.Next, err)
	}
	// A cursor serves only the order that issued it.
	if _, err := db.ListTasks(TaskFilter{Order: TaskOrderTitle, After: cursors[TaskOrderUpdatedDesc]}); err == nil || !strings.Contains(err.Error(), `belongs to order "updated_desc"`) {
		t.Fatalf("cursor of another order: %v", err)
	}
	if _, err := db.ListTasks(TaskFilter{After: cursors[TaskOrderState]}); err == nil {
		t.Fatal("an opaque cursor accepted under the id order")
	}
	if _, err := db.ListTasks(TaskFilter{Order: TaskOrderState, After: cursors[TaskOrderID]}); err == nil {
		t.Fatal("a bare id cursor accepted under the state order")
	}
	// A cursor naming no known order is refused without echoing it.
	forged := taskCursor{Order: "-----BEGIN RSA PRIVATE KEY-----", ID: all[0].ID}.encode()
	for _, f := range []TaskFilter{{Order: "newest"}, {Order: TaskOrderTitle, After: "!!"}, {Order: TaskOrderTitle, After: "e30"}, {Order: TaskOrderTitle, After: forged}} {
		if _, err := db.ListTasks(f); err == nil || strings.Contains(err.Error(), "BEGIN") {
			t.Fatalf("filter %+v: %v", f, err)
		}
	}
}
