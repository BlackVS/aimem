package store

// Task search: every indexed field and a comment find the task, an edit
// replaces the old text, the usual filters apply, pages resume exactly,
// and a cursor serves only the search that issued it.

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func searchIDs(t *testing.T, db *DB, f TaskFilter) []string {
	t.Helper()
	page, err := db.ListTasks(f)
	if err != nil {
		t.Fatalf("search %+v: %v", f, err)
	}
	var out []string
	for _, s := range page.Tasks {
		out = append(out, s.ID)
	}
	return out
}

func TestTaskSearchFindsEveryField(t *testing.T) {
	_, db := taskDB(t)
	c := content("Rotate the quokka certificates")
	c.Objective, c.AcceptanceCriteria, c.NonGoals = "keep the wombat online", "the platypus answers", "no echidna changes"
	byFields, err := db.CreateTask(c, aliceActor, "k-fields")
	if err != nil {
		t.Fatal(err)
	}
	byComment := mustCreate(t, db, "unrelated", "k-comment")
	if _, err := db.AddTaskComment(byComment.ID, "the numbat was seen at noon", aliceActor, "k-c1"); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, db, "noise", "k-noise")
	for q, want := range map[string]string{
		"quokka": "task", "WOMBAT": "task", "platypus": "task", "echidna": "task", "numbat": "comment",
	} {
		page, err := db.ListTasks(TaskFilter{Query: q})
		if err != nil || len(page.Tasks) != 1 {
			t.Fatalf("%q: %+v %v", q, page, err)
		}
		hit := page.Tasks[0]
		if hit.Matched != want || !strings.Contains(strings.ToLower(hit.Snippet), "["+strings.ToLower(q)+"]") || strings.ContainsRune(hit.Snippet, 10) || hit.Snippet != strings.TrimSpace(hit.Snippet) {
			t.Fatalf("%q: matched %q snippet %q", q, hit.Matched, hit.Snippet)
		}
		if wantID := map[string]string{"task": byFields.ID, "comment": byComment.ID}[want]; hit.ID != wantID {
			t.Fatalf("%q found %s, want %s", q, hit.ID, wantID)
		}
	}
	// Terms are ANDed and taken literally: FTS syntax is not an injection.
	if got := searchIDs(t, db, TaskFilter{Query: "quokka numbat"}); len(got) != 0 {
		t.Fatalf("terms in different tasks matched %v", got)
	}
	if got := searchIDs(t, db, TaskFilter{Query: `quokka OR numbat`}); len(got) != 0 {
		t.Fatalf("OR was an operator: %v", got)
	}
	if got := searchIDs(t, db, TaskFilter{Query: `"quokka" NEAR(`}); len(got) != 0 {
		t.Fatalf("syntax characters: %v", got)
	}
	// A plain list carries no search fields.
	page, _ := db.ListTasks(TaskFilter{})
	for _, s := range page.Tasks {
		if s.Matched != "" || s.Snippet != "" {
			t.Fatalf("list row has search fields: %+v", s)
		}
	}
}

func TestTaskSearchFollowsEditsAndFilters(t *testing.T) {
	_, db := taskDB(t)
	tk := mustCreate(t, db, "find the kakapo", "k-edit")
	if _, err := db.UpdateTask(tk.ID, content("find the kea"), 1, aliceActor, "k-edit-2"); err != nil {
		t.Fatal(err)
	}
	if got := searchIDs(t, db, TaskFilter{Query: "kakapo"}); len(got) != 0 {
		t.Fatalf("old title still found: %v", got)
	}
	if got := searchIDs(t, db, TaskFilter{Query: "kea"}); !slices.Equal(got, []string{tk.ID}) {
		t.Fatalf("new title: %v", got)
	}
	// A comment survives the edit of its task's body.
	if _, err := db.AddTaskComment(tk.ID, "the takahe note", aliceActor, "k-edit-c"); err != nil {
		t.Fatal(err)
	}
	done := content("find the kea")
	done.State, done.Archived = "DONE", true
	if _, err := db.UpdateTask(tk.ID, done, 2, aliceActor, "k-edit-3"); err != nil {
		t.Fatal(err)
	}
	if got := searchIDs(t, db, TaskFilter{Query: "takahe"}); len(got) != 0 {
		t.Fatalf("archived task found by default: %v", got)
	}
	if got := searchIDs(t, db, TaskFilter{Query: "takahe", IncludeArchived: true}); !slices.Equal(got, []string{tk.ID}) {
		t.Fatalf("archived comment: %v", got)
	}
	if got := searchIDs(t, db, TaskFilter{Query: "kea", IncludeArchived: true, State: "READY"}); len(got) != 0 {
		t.Fatalf("state filter ignored: %v", got)
	}
	if got := searchIDs(t, db, TaskFilter{Query: "kea", IncludeArchived: true, State: "DONE"}); !slices.Equal(got, []string{tk.ID}) {
		t.Fatalf("state filter: %v", got)
	}
}

func TestTaskSearchPagesAndRefusesForeignCursors(t *testing.T) {
	_, db := taskDB(t)
	var all []string
	for i := 0; i < 7; i++ {
		// Equal and different relevance: some titles repeat the term.
		title := "kiwi " + strings.Repeat("kiwi ", i%3) + fmt.Sprint(i)
		all = append(all, mustCreate(t, db, title, fmt.Sprintf("k-page-%d", i)).ID)
	}
	full := searchIDs(t, db, TaskFilter{Query: "kiwi", Limit: 100})
	if len(full) != 7 {
		t.Fatalf("one page: %v", full)
	}
	var paged []string
	f := TaskFilter{Query: "kiwi", Limit: 2}
	for n := 0; ; n++ {
		page, err := db.ListTasks(f)
		if err != nil || n > 7 {
			t.Fatalf("page %d: %v", n, err)
		}
		for _, s := range page.Tasks {
			paged = append(paged, s.ID)
		}
		if page.Next == "" {
			break
		}
		f.After = page.Next
	}
	if !slices.Equal(paged, full) {
		t.Fatalf("pages %v, want %v", paged, full)
	}
	sorted := slices.Clone(full)
	slices.Sort(sorted)
	slices.Sort(all)
	if !slices.Equal(sorted, all) {
		t.Fatalf("not every match: %v", full)
	}
	first, _ := db.ListTasks(TaskFilter{Query: "kiwi", Limit: 2})
	for name, f := range map[string]TaskFilter{
		"other search":   {Query: "kiwi 3", After: first.Next},
		"no search":      {Order: TaskOrderUpdatedDesc, After: first.Next},
		"order + search": {Query: "kiwi", Order: TaskOrderTitle},
		"blank":          {Query: "   "},
		"too long":       {Query: strings.Repeat("a", MaxTaskQueryBytes+1)},
		"invalid utf-8":  {Query: "kiwi \xff"},
		"nul":            {Query: "kiwi \x00"},
	} {
		if _, err := db.ListTasks(f); !errors.Is(err, ErrTaskInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// The v25 step indexes the tasks and comments a v24 file already holds,
// and RebuildFTS restores a damaged index.
func TestTaskSearchMigrationAndRebuild(t *testing.T) {
	root := t.TempDir()
	r, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	db, _ := r.Open("proj-a")
	tk := mustCreate(t, db, "migrate the moa", "k-mig")
	if _, err := db.AddTaskComment(tk.ID, "a huia remark", aliceActor, "k-mig-c"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TRIGGER tasks_fts_ai`, `DROP TRIGGER tasks_fts_au`, `DROP TRIGGER tasks_fts_ad`, `DROP TRIGGER task_comments_fts_ai`,
		`DROP TABLE task_fts`, `UPDATE meta SET value='24' WHERE key='schema_version'`,
	} {
		if _, err := db.sql.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	r.Close()
	r2, err := NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r2.Close)
	db2, err := r2.Open("proj-a")
	if err != nil {
		t.Fatalf("reopen migrates: %v", err)
	}
	for _, q := range []string{"moa", "huia"} {
		if got := searchIDs(t, db2, TaskFilter{Query: q}); !slices.Equal(got, []string{tk.ID}) {
			t.Fatalf("%q after migration: %v", q, got)
		}
	}
	if _, err := db2.sql.Exec(`DELETE FROM task_fts`); err != nil {
		t.Fatal(err)
	}
	if err := db2.RebuildFTS(); err != nil {
		t.Fatal(err)
	}
	if got := searchIDs(t, db2, TaskFilter{Query: "huia"}); !slices.Equal(got, []string{tk.ID}) {
		t.Fatalf("after rebuild: %v", got)
	}
}
