package process

// A tag or branch resolves to the commit it holds, an annotated tag peeled
// to its commit; an unknown or ambiguous name is refused; and the
// bootstrap names the selected ref beside the pinned commit.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestResolveTagsAndBranches(t *testing.T) {
	dir, first, second := tagFixture(t)
	ctx := context.Background()
	for name, want := range map[string]string{"v1": first, "v2": second, "feature": first, "main": second} {
		got, err := resolveNoValidate(ctx, dir, name)
		if err != nil || got != want {
			t.Fatalf("%s: %q %v, want %s", name, got, err, want)
		}
	}
	if _, err := resolveNoValidate(ctx, dir, "v1-light"); err == nil || !strings.Contains(err.Error(), "both a tag") {
		t.Fatalf("a name that is a tag and a branch on different commits: %v", err)
	}
	if _, err := resolveNoValidate(ctx, dir, "v9"); err == nil || !strings.Contains(err.Error(), `no tag or branch "v9"`) {
		t.Fatalf("an unknown name: %v", err)
	}
	// The public entry refuses what a selection could not hold.
	for _, c := range []struct{ repo, name, want string }{
		{dir, "v1", "repo must start with"},
		{"https://example.com/p.git", "-v1", "plain branch or tag"},
		{"https://example.com/p.git", "v1..v2", "plain branch or tag"},
		{"https://example.com/p.git", "v1.lock", "plain branch or tag"},
	} {
		if _, err := Resolve(ctx, c.repo, c.name); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("Resolve(%q, %q): %v", c.repo, c.name, err)
		}
	}
}

func TestBootstrapNamesTheSelectedRef(t *testing.T) {
	commit := strings.Repeat("c", 40)
	set := &Set{
		Ref:       Ref{Repo: "https://example.com/team/process.git", Ref: "v1.2.0", Commit: commit, Manifest: "processes/aimem/manifest.json"},
		Manifest:  Manifest{Version: 1, Handbook: "h.md", BudgetBytes: 4096},
		Handbook:  "# Handbook\n",
		Templates: map[string]json.RawMessage{},
	}
	none := func(string) SkillState { return SkillState{} }
	text, err := Bootstrap(set, "alpha", none)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Process set: aimem v1.2.0 @ " + commit[:12] + " (manifest processes/aimem/manifest.json)."; !strings.Contains(text, want) {
		t.Fatalf("bootstrap lacks %q:\n%s", want, text)
	}
	set.Ref.Ref = ""
	if text, _ = Bootstrap(set, "alpha", none); !strings.Contains(text, "Process set: aimem @ "+commit[:12]) {
		t.Fatalf("a selection without a ref:\n%s", text)
	}
}
