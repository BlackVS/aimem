package curate

import (
	"fmt"
	"strings"
	"testing"

	"aimem/internal/store"
)

// fakeSynth echoes a recognizable body naming the section it was asked
// to write, so assembly order and inputs are checkable.
type fakeSynth struct{ calls []string }

func (f *fakeSynth) Complete(prompt string) (string, Usage, error) {
	f.calls = append(f.calls, prompt)
	return fmt.Sprintf("synthesized(%d)", len(f.calls)), Usage{InputTokens: 50, OutputTokens: 20}, nil
}

func TestGenerateDoc(t *testing.T) {
	reg, _ := store.NewRegistry(t.TempDir())
	defer reg.Close()
	db, _ := reg.Open("group-kb")
	db.SetMeta("about", "the kb charter")
	db.SetMeta("chapters", `[{"name":"ci","about":"pipelines"},{"name":"storage","about":"disks"}]`)
	db.Remember("woodpecker runs the builds", "curator", store.RememberOpts{Tags: []string{"chapter:ci"}})
	db.Remember("sqlite stores the journal", "curator", store.RememberOpts{Tags: []string{"chapter:storage"}})
	db.Remember("an unfiled stray fact", "curator", store.RememberOpts{})

	syn := &fakeSynth{}
	rep, err := GenerateDoc(db, "kb", syn, false)
	if err != nil || rep.Skipped || rep.Facts != 3 || rep.Sections != 3 {
		t.Fatalf("report: %+v err=%v", rep, err)
	}
	// Sections prompts carry charter, chapter description, and facts.
	if !strings.Contains(syn.calls[0], "the kb charter") ||
		!strings.Contains(syn.calls[0], "pipelines") ||
		!strings.Contains(syn.calls[0], "woodpecker runs the builds") {
		t.Fatalf("section prompt wrong:\n%s", syn.calls[0])
	}
	doc, _ := db.GetMeta("design_doc")
	for _, want := range []string{"# kb — design document", "## ci", "## storage",
		"## unfiled", "## Sources", "[1]", "woodpecker runs the builds"} {
		if !strings.Contains(doc, want) {
			t.Fatalf("doc missing %q:\n%s", want, doc[:min(400, len(doc))])
		}
	}
	// Usage accounted: 3 sections + 1 overview.
	if len(syn.calls) != 4 || rep.Usage.InputTokens != 200 {
		t.Fatalf("synth calls=%d usage=%+v", len(syn.calls), rep.Usage)
	}

	// Unchanged facts: regeneration skips; force overrides.
	rep, err = GenerateDoc(db, "kb", syn, false)
	if err != nil || !rep.Skipped {
		t.Fatalf("staleness skip: %+v err=%v", rep, err)
	}
	if rep, err = GenerateDoc(db, "kb", syn, true); err != nil || rep.Skipped {
		t.Fatalf("force: %+v err=%v", rep, err)
	}
}

func TestFeatureEnabled(t *testing.T) {
	reg, _ := store.NewRegistry(t.TempDir())
	defer reg.Close()
	db, _ := reg.Open("group-f")
	if FeatureEnabled(db, "doc") {
		t.Fatal("feature on by default")
	}
	db.SetMeta("features", `["doc"]`)
	if !FeatureEnabled(db, "doc") || FeatureEnabled(db, "other") {
		t.Fatal("feature flag misread")
	}
}
