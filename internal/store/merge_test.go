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

	events, mems, runs, err := r.MergeProject("rc-0f05aa6db5cf", "rc")
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	// The shared idempotency key dedups: only the genuinely-old event lands.
	if events != 1 || mems != 1 || runs != 1 {
		t.Fatalf("merged events=%d mems=%d runs=%d; want 1/1/1", events, mems, runs)
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
		{"missing", "alpha", "no such source"},
	}
	for _, c := range cases {
		if _, _, _, err := r.MergeProject(c.old, c.new); err == nil {
			t.Errorf("MergeProject(%q, %q) succeeded; want refusal (%s)", c.old, c.new, c.why)
		}
	}
	// A source holding a shared doc refuses (names could collide).
	if _, err := a.PutDoc("RUNBOOK", "content\n", "x", 0, false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.MergeProject("alpha", "beta"); err == nil || !strings.Contains(err.Error(), "document") {
		t.Errorf("merge with docs should refuse, got %v", err)
	}
	for _, id := range []string{"alpha", "beta"} {
		if _, err := r.OpenExisting(id); err != nil {
			t.Errorf("project %q disappeared after refused merge: %v", id, err)
		}
	}
}
