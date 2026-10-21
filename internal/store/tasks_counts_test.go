package store

// CountTasks is the board's shape: the active tasks per state with every
// state present, the archived apart, under the assignee and epic filters.

import (
	"errors"
	"reflect"
	"testing"

	"aimem/internal/uuidv7"
)

func TestCountTasks(t *testing.T) {
	_, db := taskDB(t)
	epic, err := db.CreateEpic(EpicContent{Title: "release"}, aliceActor, "k-count-epic")
	if err != nil {
		t.Fatal(err)
	}
	bob := &TaskAssignee{Kind: "user", ID: uuidv7.New()}
	n := 0
	mk := func(state string, assignee *TaskAssignee, epicID string) Task {
		t.Helper()
		n++
		c := content("count task")
		c.State, c.Assignee, c.Epic = state, assignee, epicID
		task, err := db.CreateTask(c, aliceActor, "k-count-"+string(rune('a'+n)))
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	mk("BACKLOG", nil, "")
	mk("BACKLOG", bob, epic.ID)
	mk("READY", bob, "")
	mk("IN_PROGRESS", nil, epic.ID)
	done := mk("DONE", bob, epic.ID)
	c := done.TaskContent
	c.Archived = true
	if _, err := db.UpdateTask(done.ID, c, done.Revision, aliceActor, "k-count-archive"); err != nil {
		t.Fatal(err)
	}
	want := func(archived, total int, states map[string]int) TaskCounts {
		out := TaskCounts{Counts: map[string]int{}, Archived: archived, Total: total}
		for _, st := range TaskStates {
			out.Counts[st] = states[st]
		}
		return out
	}
	for _, c := range []struct {
		name string
		f    TaskFilter
		want TaskCounts
	}{
		{"all", TaskFilter{}, want(1, 5, map[string]int{"BACKLOG": 2, "READY": 1, "IN_PROGRESS": 1})},
		{"assignee", TaskFilter{Assignee: bob}, want(1, 3, map[string]int{"BACKLOG": 1, "READY": 1})},
		{"epic", TaskFilter{Epic: epic.ID}, want(1, 3, map[string]int{"BACKLOG": 1, "IN_PROGRESS": 1})},
		{"assignee and epic", TaskFilter{Assignee: bob, Epic: epic.ID}, want(1, 2, map[string]int{"BACKLOG": 1})},
		{"nobody's", TaskFilter{Assignee: &TaskAssignee{Kind: "group", ID: uuidv7.New()}}, want(0, 0, nil)},
		// The list's other filters are not read: the counts are per state.
		{"state ignored", TaskFilter{State: "READY", UpdatedSince: "2999-01-01T00:00:00Z"}, want(1, 5, map[string]int{"BACKLOG": 2, "READY": 1, "IN_PROGRESS": 1})},
	} {
		got, err := db.CountTasks(c.f)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
	for _, f := range []TaskFilter{
		{Assignee: &TaskAssignee{Kind: "team", ID: uuidv7.New()}},
		{Assignee: &TaskAssignee{Kind: "user", ID: "nope"}},
		{Epic: "nope"},
	} {
		if _, err := db.CountTasks(f); !errors.Is(err, ErrTaskInvalid) {
			t.Fatalf("%+v: want an invalid-input error, got %v", f, err)
		}
	}
}
