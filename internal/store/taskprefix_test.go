package store

import (
	"errors"
	"strings"
	"testing"
)

func TestIDPrefixShape(t *testing.T) {
	full := "01a17718-4ca9-7000-b07d-4c212c4de792"
	for _, c := range []struct {
		s    string
		want bool
	}{
		{"0", true},
		{"01a17718-4ca9", true},
		{"01a17718-4ca9-7", true},
		{"01a17718-4ca9-7000-b", true},
		{full[:35], true},
		{full, false},                   // a full id is not a prefix
		{"01A17718-4CA9", false},        // ids are written lowercase
		{"01a17718x4ca9", false},        // a dash where the layout has one
		{"01a17718-4ca9-6", false},      // the version is 7
		{"01a17718-4ca9-7000-c", false}, // the variant is 8, 9, a or b
		{"", false},
		{"nope", false},
	} {
		if got := IDPrefixShape(c.s); got != c.want {
			t.Errorf("IDPrefixShape(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

// cloneTaskRow copies a task row under another id, so two tasks share a
// prefix: the API mints the ids itself.
func cloneTaskRow(t *testing.T, from, to *DB, src, id string) {
	t.Helper()
	// The store's pool holds one connection, so ATTACH and the copy run
	// on the same one.
	for _, q := range []string{
		`ATTACH DATABASE '` + from.path + `' AS src`,
		`CREATE TEMP TABLE clone AS SELECT * FROM src.tasks WHERE id = '` + src + `'`,
		`UPDATE clone SET id = '` + id + `'`,
		`INSERT INTO main.tasks SELECT * FROM clone`,
		`DROP TABLE clone`,
		`DETACH DATABASE src`,
	} {
		if _, err := to.sql.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestResolveTaskIDPrefix(t *testing.T) {
	r := newTestRegistry(t)
	a, _ := r.Open("proj-a")
	b, _ := r.Open("proj-b")
	ta := mustCreate(t, a, "in a", "k-a")
	short := ta.ID[:TaskIDPrefixMin]

	// A full id, or anything not shaped as the start of one, is passed on.
	for _, arg := range []string{ta.ID, "nope", strings.ToUpper(short)} {
		if got, err := r.ResolveTaskID(arg); err != nil || got != arg {
			t.Fatalf("ResolveTaskID(%q) = %q, %v", arg, got, err)
		}
	}
	// A unique prefix is the full id, from any project.
	if got, err := r.ResolveTaskID(short); err != nil || got != ta.ID {
		t.Fatalf("unique prefix: %q %v", got, err)
	}
	if got, err := a.ResolveProjectTaskID(short); err != nil || got != ta.ID {
		t.Fatalf("unique prefix in its project: %q %v", got, err)
	}
	if _, err := b.ResolveProjectTaskID(short); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("prefix in another project: %v", err)
	}
	// Shorter than the rule.
	if _, err := r.ResolveTaskID(short[:12]); !errors.Is(err, ErrTaskIDShort) || !errors.Is(err, ErrTaskInvalid) {
		t.Fatalf("short prefix: %v", err)
	}
	// A miss scans once, is remembered, and a create under the prefix
	// voids it.
	miss := "00000000-0000"
	before := r.taskScans.Load()
	for i := 0; i < 3; i++ {
		if _, err := r.ResolveTaskID(miss); !errors.Is(err, ErrTaskNotFound) {
			t.Fatalf("miss %d: %v", i, err)
		}
	}
	if n := r.taskScans.Load() - before; n != 1 {
		t.Fatalf("a repeated prefix miss scanned %d times, want 1", n)
	}
	r.hints.put(miss+"-7000-8000-000000000000", "proj-a")
	if r.hints.missed(miss) {
		t.Fatal("a create under a missed prefix must void the miss")
	}

	// Two tasks under one prefix, in two projects: both are named, with
	// their projects; the longer prefix still resolves.
	twin := ta.ID[:35] + map[bool]string{true: "1", false: "0"}[ta.ID[35] == '0']
	cloneTaskRow(t, a, b, ta.ID, twin)
	_, err := r.ResolveTaskID(short)
	var amb *AmbiguousIDError
	if !errors.As(err, &amb) || !errors.Is(err, ErrTaskIDAmbiguous) || len(amb.Matches) != 2 || !strings.Contains(err.Error(), ta.ID) || !strings.Contains(err.Error(), twin) {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	projects := map[string]string{}
	for _, m := range amb.Matches {
		projects[m.ID] = m.Project
	}
	if projects[ta.ID] != "proj-a" || projects[twin] != "proj-b" {
		t.Fatalf("matches: %+v", amb.Matches)
	}
	if got, err := r.ResolveTaskID(ta.ID[:30]); !errors.As(err, &amb) || got != "" {
		t.Fatalf("a prefix both share is still ambiguous: %q %v", got, err)
	}
	// Inside one project the other one does not count.
	if got, err := a.ResolveProjectTaskID(short); err != nil || got != ta.ID {
		t.Fatalf("project-scoped prefix: %q %v", got, err)
	}

	// Comments: a prefix among the task's own comments.
	c, err := a.AddTaskComment(ta.ID, "first", TaskActor{Kind: "admin", Name: "local"}, "c-1")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.ResolveTaskCommentID(ta.ID, c.ID[:TaskIDPrefixMin]); err != nil || got != c.ID {
		t.Fatalf("comment prefix: %q %v", got, err)
	}
	other := mustCreate(t, a, "other", "k-o")
	if _, err := a.ResolveTaskCommentID(other.ID, c.ID[:TaskIDPrefixMin]); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("comment prefix under another task: %v", err)
	}
}
