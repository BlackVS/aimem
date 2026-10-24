package main

import (
	"strings"
	"testing"

	"aimem/internal/store"
)

// The report lists what the filter flags in stored task text, one line
// per field or comment, and changes nothing.
func TestTasksTextCheckReportsWithoutWriting(t *testing.T) {
	reg := projectService(t)
	db, err := reg.Open("example")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta(store.TasksMetaKey, "on"); err != nil {
		t.Fatal(err)
	}
	actor := store.TaskActor{Kind: "admin", Name: "host-admin"}
	// Stored directly, as text written before the hub checked it.
	old, err := db.CreateTask(store.TaskContent{Title: "Clean up", Objective: "All11containersremoved", State: "READY"}, actor, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTaskComment(old.ID, "CI409 SUCCESS99s", actor, "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTaskComment(old.ID, "The worker pushed the fix to the branch.", actor, "c2"); err != nil {
		t.Fatal(err)
	}
	clean, err := db.CreateTask(store.TaskContent{Title: "Ship the release", Objective: "Tag v0.10.14 from master.", State: "READY"}, actor, "k2")
	if err != nil {
		t.Fatal(err)
	}

	out, err := stdoutOf(t, func() error { return tasksCmd([]string{"text-check", "-p", "example"}) })
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"task " + old.ID + ` objective: "All11containersremoved" (words and numbers joined together)`,
		"task " + old.ID + ` comment 1 by host-admin at `,
		`"CI409" (words and numbers joined together)`,
		"project example: 2 tasks, 12 fields and 2 comments checked, 2 flagged (the filter alone; nothing was changed)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, clean.ID) || strings.Contains(out, "comment 2") {
		t.Fatalf("report flagged readable text:\n%s", out)
	}
	// A later --since leaves everything out.
	out, err = stdoutOf(t, func() error { return tasksCmd([]string{"text-check", "-p", "example", "--since", "2999-01-01"}) })
	if err != nil || !strings.Contains(out, "since 2999-01-01T00:00:00Z: 2 tasks, 0 fields and 0 comments checked, 0 flagged") {
		t.Fatalf("--since in the future: %v\n%s", err, out)
	}
	if _, err := stdoutOf(t, func() error { return tasksCmd([]string{"text-check", "--since", "yesterday"}) }); err == nil || !strings.Contains(err.Error(), "RFC 3339") {
		t.Fatalf("bad --since: %v", err)
	}
	// Nothing changed.
	if got, _ := db.GetTask(old.ID); got.Revision != 1 {
		t.Fatalf("the report moved the task to revision %d", got.Revision)
	}
	if page, _ := db.TaskCommentsOrdered(old.ID, 0, 100, false); len(page.Comments) != 2 {
		t.Fatalf("%d comments after the report", len(page.Comments))
	}
}
