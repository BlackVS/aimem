package curate

import (
	"fmt"
	"strings"
	"testing"

	"aimem/internal/store"
)

type fakeRefileSynth struct{ reply string }

func (f *fakeRefileSynth) Complete(string) (string, Usage, error) { return f.reply, Usage{}, nil }

func TestProposeChapters(t *testing.T) {
	reg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	db, err := reg.Open("group-x")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := range 3 {
		id, _, err := db.Remember(fmt.Sprintf("fact number %d about testing", i), "test", store.RememberOpts{})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	// One fact already filed — must not reach the model.
	if err := db.Tag(ids[2], "chapter:done", "test"); err != nil {
		t.Fatal(err)
	}
	unfiled, err := UnfiledFacts(db)
	if err != nil || len(unfiled) != 2 {
		t.Fatalf("unfiled = %d (%v), want 2", len(unfiled), err)
	}
	// Reply wrapped in prose+fences (tolerant extraction), one assignment
	// to an existing chapter, one new chapter, plus a hallucinated id
	// that must be dropped.
	syn := &fakeRefileSynth{reply: "Sure! ```json\n" + fmt.Sprintf(
		`{"assign":[{"fact_id":%q,"chapter":"ops"}],
		  "new_chapters":[{"name":"testing","about":"test facts","fact_ids":[%q,"01a0-hallucinated"]}]}`,
		ids[0], ids[1]) + "\n```"}
	plan, err := ProposeChapters(db, "x", "charter", []Chapter{{Name: "ops", About: "o"}}, syn, 0)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Unfiled != 2 || plan.Considered != 2 {
		t.Fatalf("counts: %+v", plan)
	}
	if len(plan.Assign) != 1 || plan.Assign[0].FactID != ids[0] {
		t.Fatalf("assign: %+v", plan.Assign)
	}
	if len(plan.NewChapters) != 1 || len(plan.NewChapters[0].FactIDs) != 1 || plan.NewChapters[0].FactIDs[0] != ids[1] {
		t.Fatalf("hallucinated id survived: %+v", plan.NewChapters)
	}
	// Filing honors the one-chapter invariant via Tag.
	if err := db.Tag(ids[1], "chapter:testing", "test"); err != nil {
		t.Fatal(err)
	}
	if u, _ := UnfiledFacts(db); len(u) != 1 {
		t.Fatalf("after filing, unfiled = %d, want 1", len(u))
	}
}

func TestProposeChaptersRefusal(t *testing.T) {
	reg, _ := store.NewRegistry(t.TempDir())
	defer reg.Close()
	db, _ := reg.Open("group-y")
	db.Remember("a fact", "test", store.RememberOpts{})
	syn := &fakeRefileSynth{reply: "I cannot help with that."}
	if _, err := ProposeChapters(db, "y", "", nil, syn, 0); err == nil ||
		!strings.Contains(err.Error(), "no JSON object") {
		t.Fatalf("refusal should error without writing, got %v", err)
	}
}
