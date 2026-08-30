package curate

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"aimem/internal/store"
)

type fakeRefileSynth struct{ reply, prompt string }

func (f *fakeRefileSynth) Complete(p string) (string, Usage, error) {
	f.prompt = p
	return f.reply, Usage{}, nil
}

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
	plan, err := ProposeChapters(db, "x", "charter", []Chapter{{Name: "ops", About: "o"}}, syn, 0, false)
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
	if _, err := ProposeChapters(db, "y", "", nil, syn, 0, false); err == nil ||
		!strings.Contains(err.Error(), "no JSON object") {
		t.Fatalf("refusal should error without writing, got %v", err)
	}
}

// TestProposeChaptersRevisit: the re-label pass pools FILED facts with
// room under the cap, shows their current filings, and add-only assigns
// from the current chapter set.
func TestProposeChaptersRevisit(t *testing.T) {
	reg, _ := store.NewRegistry(t.TempDir())
	defer reg.Close()
	db, _ := reg.Open("group-z")
	idA, _, err := db.Remember("deploy is via installers", "test", store.RememberOpts{})
	if err != nil {
		t.Fatal(err)
	}
	idB, _, err := db.Remember("unfiled newcomer", "test", store.RememberOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Tag(idA, "chapter:ops", "test"); err != nil {
		t.Fatal(err)
	}
	cands, err := RefileCandidates(db)
	if err != nil || len(cands) != 1 || cands[0].ID != idA {
		t.Fatalf("candidates = %+v (%v), want just the filed fact", cands, err)
	}
	syn := &fakeRefileSynth{reply: fmt.Sprintf(
		`{"assign":[{"fact_id":%q,"chapter":"deploy"}],"new_chapters":[]}`, idA)}
	plan, err := ProposeChapters(db, "z", "charter",
		[]Chapter{{Name: "ops", About: "o"}, {Name: "deploy", About: "d"}}, syn, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Revisit || plan.Unfiled != 1 || plan.Considered != 1 {
		t.Fatalf("plan counts: %+v", plan)
	}
	if len(plan.Assign) != 1 || plan.Assign[0].Chapter != "deploy" {
		t.Fatalf("assign: %+v", plan.Assign)
	}
	// The prompt carried the existing filing so the model could judge.
	if !strings.Contains(syn.prompt, "[ops]") || strings.Contains(syn.prompt, idB) {
		t.Fatalf("prompt should show current filings and exclude unfiled facts:\n%s", syn.prompt)
	}
}

// TestRevisitRotation: the revisit pool does not drain, so successive
// runs must walk it with the persisted cursor instead of reconsidering
// the same first batch forever.
func TestRevisitRotation(t *testing.T) {
	reg, _ := store.NewRegistry(t.TempDir())
	defer reg.Close()
	db, _ := reg.Open("group-r")
	var ids []string
	for i := range 3 {
		id, _, err := db.Remember(fmt.Sprintf("rotating fact %d", i), "test", store.RememberOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Tag(id, "chapter:ops", "test"); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	sortedIDs := append([]string{}, ids...)
	sort.Strings(sortedIDs)
	syn := &fakeRefileSynth{reply: `{"assign":[],"new_chapters":[]}`}
	chapters := []Chapter{{Name: "ops", About: "o"}}

	seen := func() []string {
		var got []string
		for _, id := range sortedIDs {
			if strings.Contains(syn.prompt, id) {
				got = append(got, id)
			}
		}
		return got
	}
	if _, err := ProposeChapters(db, "r", "", chapters, syn, 2, true); err != nil {
		t.Fatal(err)
	}
	first := seen()
	if len(first) != 2 || first[0] != sortedIDs[0] || first[1] != sortedIDs[1] {
		t.Fatalf("first batch = %v, want first two of %v", first, sortedIDs)
	}
	if _, err := ProposeChapters(db, "r", "", chapters, syn, 2, true); err != nil {
		t.Fatal(err)
	}
	second := seen()
	// Second run continues at the third fact and wraps to the first.
	if len(second) != 2 || second[0] != sortedIDs[0] || second[1] != sortedIDs[2] {
		t.Fatalf("second batch = %v, want %v and %v (continue + wrap)", second, sortedIDs[2], sortedIDs[0])
	}
}
