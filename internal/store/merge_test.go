package store

import (
	"strings"
	"testing"
)

func TestMergeProject(t *testing.T) {
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// One real project under two ids: the old derived id and the pin.
	old, err := r.Open("rc-0f05aa6db5cf")
	if err != nil {
		t.Fatal(err)
	}
	dst, err := r.Open("rc")
	if err != nil {
		t.Fatal(err)
	}
	old.Append(testEvent("old-k1", "t1"))
	old.Append(testEvent("shared-k", "t2")) // also present in the target
	dst.Append(testEvent("shared-k", "t2"))
	dst.Append(testEvent("dst-k1", "t3"))
	if _, _, err := old.Remember("fact from the old id", "curator", RememberOpts{Kind: "fact"}); err != nil {
		t.Fatal(err)
	}
	old.AddCurateRun(&CurateRun{ID: "run-1", TS: "2026-08-01T00:00:00Z"})

	// A group cites the old id; the merge must relabel it.
	grp, err := r.Open("group-ai-infra")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := grp.Remember("shared fact", "curator", RememberOpts{
		Kind: "fact", Sources: []string{"project:rc-0f05aa6db5cf"},
	}); err != nil {
		t.Fatal(err)
	}

	events, mems, runs, cites, err := r.MergeProject("rc-0f05aa6db5cf", "rc")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	// The shared idempotency key dedups: only the genuinely-old event lands.
	if events != 1 || mems != 1 || runs != 1 || cites != 1 {
		t.Fatalf("merged events=%d mems=%d runs=%d cites=%d; want 1/1/1/1", events, mems, runs, cites)
	}
	if _, err := r.OpenExisting("rc-0f05aa6db5cf"); err == nil {
		t.Fatal("source still resolves after merge")
	}
	got, err := r.OpenExisting("rc")
	if err != nil {
		t.Fatal(err)
	}
	if evs, _ := got.EventsSince("", 100); len(evs) != 3 {
		t.Fatalf("target has %d events; want 3 (2 own + 1 merged, dup dropped)", len(evs))
	}
	if ms, _ := got.Memories(false); len(ms) != 1 || ms[0].Text != "fact from the old id" {
		t.Fatalf("target memories: %+v", ms)
	}
	gm, _ := grp.Memories(false)
	var cited string
	for _, s := range gm[0].Sources {
		if strings.HasPrefix(s, "project:") {
			cited = s
		}
	}
	if cited != "project:rc" {
		t.Fatalf("group fact cites %q; want project:rc", cited)
	}
}

func TestMergeProjectRefusals(t *testing.T) {
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	a, _ := r.Open("alpha")
	if _, err := r.Open("beta"); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ old, new, why string }{
		{"alpha", "missing", "target must exist (else it is a rename)"},
		{"alpha", "alpha", "self-merge"},
		{UserScopeProject, "alpha", "user DB reserved"},
		{"alpha", "group-x", "group DB reserved"},
		{"missing", "alpha", "no such source and nothing cites it"},
	}
	for _, c := range cases {
		if _, _, _, _, err := r.MergeProject(c.old, c.new); err == nil {
			t.Errorf("MergeProject(%q, %q) succeeded; want refusal (%s)", c.old, c.new, c.why)
		}
	}
	// A source holding a shared doc refuses (names could collide).
	if _, err := a.PutDoc("RUNBOOK", "content\n", "x", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := r.MergeProject("alpha", "beta"); err == nil || !strings.Contains(err.Error(), "document") {
		t.Errorf("merge with docs should refuse, got %v", err)
	}
	for _, id := range []string{"alpha", "beta"} {
		if _, err := r.OpenExisting(id); err != nil {
			t.Errorf("project %q disappeared after refused merge: %v", id, err)
		}
	}
}

// TestMergeOrphanedOrigin: the source project is already GONE but group
// facts still cite it (dropped before merge existed) — the merge
// degrades to a pure citation relabel.
func TestMergeOrphanedOrigin(t *testing.T) {
	r, err := NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.Open("rc"); err != nil {
		t.Fatal(err)
	}
	grp, err := r.Open("group-oboro")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := grp.Remember("orphan-cited fact", "curator", RememberOpts{
		Kind: "fact", Sources: []string{"project:RC-000668815ca9"},
	}); err != nil {
		t.Fatal(err)
	}
	events, mems, runs, cites, err := r.MergeProject("RC-000668815ca9", "rc")
	if err != nil || events != 0 || mems != 0 || runs != 0 || cites != 1 {
		t.Fatalf("orphan relabel: ev=%d mems=%d runs=%d cites=%d err=%v; want 0/0/0/1 nil",
			events, mems, runs, cites, err)
	}
	gm, _ := grp.Memories(false)
	var cited string
	for _, s := range gm[0].Sources {
		if strings.HasPrefix(s, "project:") {
			cited = s
		}
	}
	if cited != "project:rc" {
		t.Fatalf("citation is %q; want project:rc", cited)
	}

	// DURABILITY (the resurrection case, lived live): sync UNIONS
	// sources, so a lagging peer re-pushes the old label. The recorded
	// alias must normalize it on import instead of resurrecting it.
	peer := gm[0]
	peer.Sources = []string{"project:RC-000668815ca9"}
	if err := grp.ImportMemory(&peer); err != nil {
		t.Fatal(err)
	}
	gm, _ = grp.Memories(false)
	for _, s := range gm[0].Sources {
		if strings.Contains(s, "RC-000") {
			t.Fatalf("old label resurrected through import: %v", gm[0].Sources)
		}
	}
	// And a brand-new fact citing the old id files under the new one.
	nid, _, err := grp.Remember("late fact from a lagging peer", "curator", RememberOpts{
		Kind: "fact", Sources: []string{"project:RC-000668815ca9"},
	})
	if err != nil {
		t.Fatal(err)
	}
	gm, _ = grp.Memories(false)
	for _, m := range gm {
		if m.ID != nid {
			continue
		}
		for _, s := range m.Sources {
			if strings.Contains(s, "RC-000") {
				t.Fatalf("new fact carries the dead label: %v", m.Sources)
			}
		}
	}
}
