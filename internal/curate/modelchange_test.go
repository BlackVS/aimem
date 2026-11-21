package curate

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/embed"
	"aimem/internal/store"
)

// The first curation run after a change of the embedding model: the
// existing fact has a vector only under the old key, so a dedup that read
// the new key as it stands would find no neighbour and admit the
// rephrased twin. The run backfills the target first, then dedups.
func TestCurateFirstRunAfterAModelChangeAdmitsNoTwin(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	seed(t, reg, "proj-mc", 2)
	db, _ := reg.Open("proj-mc")
	oldID, _, err := db.Remember("sqlite is the chosen journal storage engine", "curator", store.RememberOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetEmbedding(oldID, "old-embed", 2, embed.Encode([]float32{1, 0})); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(reg, root, "proj-mc", &fakeExtractor{}, RunOpts{
		Embedder: &fakeEmbedder{}, EmbedModel: "new-embed", DedupSim: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Backfilled != 1 || rep.Superseded != 1 || len(rep.DedupSkipped) != 0 {
		t.Fatalf("report: backfilled %d, superseded %d, dedup skipped %v; want 1, 1, none",
			rep.Backfilled, rep.Superseded, rep.DedupSkipped)
	}
	mems, _ := db.Memories(false)
	if len(mems) != 1 || mems[0].Text != "team decided to use sqlite for journals" {
		t.Fatalf("active facts after the first run under the new key: %+v (a twin was admitted)", mems)
	}
	if s, _ := db.Stats("new-embed"); s.Embedded != s.Memories {
		t.Fatalf("coverage under the new key: %d of %d", s.Embedded, s.Memories)
	}
}

// failingBackfill embeds proposals but refuses the stored fact's text,
// as an outage in the middle of a run would.
type failingBackfill struct{ fakeEmbedder }

func (f *failingBackfill) Embed(texts []string) ([][]float32, int64, error) {
	for _, t := range texts {
		if strings.Contains(t, "chosen journal storage") {
			return nil, 0, errors.New("proxy down")
		}
	}
	return f.fakeEmbedder.Embed(texts)
}

// A target whose space cannot be completed gets no dedup in that run,
// and the report names it with the reason; so does a key that holds
// vectors of another dimension than the proposals'.
func TestCurateNamesATargetItCouldNotComplete(t *testing.T) {
	for name, tc := range map[string]struct {
		emb  Embedder
		prep func(db *store.DB, id string) error
		want string
	}{
		"backfill fails": {&failingBackfill{}, func(*store.DB, string) error { return nil }, "backfill under new-embed incomplete: proxy down"},
		"another dimension": {&fakeEmbedder{}, func(db *store.DB, id string) error {
			return db.SetEmbedding(id, "new-embed", 3, embed.Encode([]float32{1, 0, 0}))
		}, store.ErrEmbedDim.Error()},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			reg, _ := store.NewRegistry(root)
			defer reg.Close()
			seed(t, reg, "proj-sk", 2)
			db, _ := reg.Open("proj-sk")
			id, _, err := db.Remember("sqlite is the chosen journal storage engine", "curator", store.RememberOpts{})
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.prep(db, id); err != nil {
				t.Fatal(err)
			}
			rep, err := Run(reg, root, "proj-sk", &fakeExtractor{}, RunOpts{
				Embedder: tc.emb, EmbedModel: "new-embed", DedupSim: 0.9})
			if err != nil {
				t.Fatal(err)
			}
			var named bool
			for _, s := range rep.DedupSkipped {
				named = named || (strings.HasPrefix(s, "proj-sk: ") && strings.Contains(s, tc.want))
			}
			if !named || rep.Superseded != 0 {
				t.Fatalf("dedup skipped %v, superseded %d; want proj-sk named with %q and no dedup", rep.DedupSkipped, rep.Superseded, tc.want)
			}
		})
	}
}

// The user scope and a declared group are dedup targets too: after a
// change, the run backfills them before it compares, so a rephrased twin
// of a fact stored there under the old key supersedes it.
func TestCurateBackfillsUserAndGroupScopesBeforeTheirDedup(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	seed(t, reg, "proj-ug", 2)
	db, _ := reg.Open("proj-ug")
	if err := db.SetMeta("groups", `["group-webstack"]`); err != nil {
		t.Fatal(err)
	}
	old := map[string]string{
		store.UserScopeProject: "sqlite user note: concise commit messages preferred",
		"group-webstack":       "sqlite webstack services write logs as JSON",
	}
	for scope, text := range old {
		sdb, _ := reg.Open(scope)
		id, _, err := sdb.Remember(text, "curator", store.RememberOpts{})
		if err != nil {
			t.Fatal(err)
		}
		if err := sdb.SetEmbedding(id, "old-embed", 2, embed.Encode([]float32{1, 0})); err != nil {
			t.Fatal(err)
		}
	}
	fx := &fakeExtractor{extra: []Proposal{
		{Text: "sqlite webstack services log in JSON", Kind: "convention", Scope: "group:webstack"},
		{Text: "sqlite user prefers concise commit messages", Kind: "preference", Scope: "user"},
	}}
	rep, err := Run(reg, root, "proj-ug", fx, RunOpts{Embedder: &fakeEmbedder{}, EmbedModel: "new-embed", DedupSim: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.DedupSkipped) != 0 {
		t.Fatalf("dedup skipped: %v", rep.DedupSkipped)
	}
	for scope, text := range old {
		sdb, _ := reg.Open(scope)
		live, _ := sdb.Memories(false)
		for _, m := range live {
			if m.Text == text {
				t.Errorf("%s: the old fact is still active beside its twin: %+v", scope, live)
			}
		}
		if s, _ := sdb.Stats("new-embed"); s.Embedded != s.Memories {
			t.Errorf("%s: coverage under the new key %d of %d", scope, s.Embedded, s.Memories)
		}
	}
}

// proposalsFail embeds stored facts but fails on the run's proposals.
type proposalsFail struct{ fakeEmbedder }

func (f *proposalsFail) Embed(texts []string) ([][]float32, int64, error) {
	for _, t := range texts {
		if strings.Contains(t, "team decided") {
			return nil, 0, errors.New("proxy down")
		}
	}
	return f.fakeEmbedder.Embed(texts)
}

// When the proposals cannot be embedded, every target the run would
// have deduplicated is named in dedup_skipped with the reason.
func TestCurateNamesEveryTargetWhenTheProposalsCannotBeEmbedded(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	seed(t, reg, "proj-pf", 2)
	rep, err := Run(reg, root, "proj-pf", &fakeExtractor{}, RunOpts{Embedder: &proposalsFail{}, EmbedModel: "new-embed", DedupSim: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"proj-pf", store.UserScopeProject} {
		named := false
		for _, s := range rep.DedupSkipped {
			named = named || (strings.HasPrefix(s, id+": ") && strings.Contains(s, "embedding the proposals failed: proxy down"))
		}
		if !named {
			t.Errorf("%s not named: %v", id, rep.DedupSkipped)
		}
	}
}

// A backfill writes one run-history row per call, also when it stops on
// an error, and each batch's budget check counts the call's spend so far:
// a cap the whole backfill exceeds stops it although every batch alone
// would fit.
func TestBackfillMetersOnceAndStopsAtTheCap(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	db, _ := reg.Open("proj-bf")
	for i := range 10 {
		if _, _, err := db.Remember("fact number "+string(rune('a'+i)), "test", store.RememberOpts{}); err != nil {
			t.Fatal(err)
		}
	}
	// Each batch of 2 projects 300 tokens and spends 20 (fakeEmbedder).
	// Nothing is recorded until the call ends, so a check that counted
	// only the recorded window would admit all five batches; counting the
	// spend so far, the fourth (60 + 300 > 350) is refused.
	if err := db.SetMeta("budget", `{"daily":{"tokens":350}}`); err != nil {
		t.Fatal(err)
	}
	n, tokens, err := Backfill(reg, db, &fakeEmbedder{}, "k", "k", 2)
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") || n != 6 || tokens != 60 {
		t.Fatalf("backfill under a cap: n=%d tokens=%d err=%v; want 6, 60 and the cap's refusal", n, tokens, err)
	}
	runs, _ := db.CurateRuns()
	rows := 0
	for _, r := range runs {
		if r.Model == "k" {
			rows++
			if r.Written != n || r.InputTokens != tokens {
				t.Errorf("the run row says %d written, %d tokens; the call did %d, %d", r.Written, r.InputTokens, n, tokens)
			}
		}
	}
	if rows != 1 {
		t.Fatalf("history rows for one backfill: %d, want 1", rows)
	}
}

// A legacy key holding two lengths: the backfill refuses it before it
// spends, and curation names the target instead of deduplicating against
// a space whose rows its proposals cannot all score.
func TestCurateNamesATargetWhoseKeyHoldsTwoLengths(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	seed(t, reg, "proj-mx", 2)
	db, _ := reg.Open("proj-mx")
	a, _, _ := db.Remember("sqlite is the chosen journal storage engine", "curator", store.RememberOpts{})
	b, _, _ := db.Remember("the hub runs as a user service", "curator", store.RememberOpts{})
	if err := db.SetEmbedding(a, "new-embed", 2, embed.Encode([]float32{1, 0})); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", filepath.Join(root, "projects", "proj-mx", "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO memory_embeddings(memory_id, model, dim, vec) VALUES(?,?,?,?)`,
		b, "new-embed", 3, embed.Encode([]float32{0, 1, 0})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Backfill(reg, db, &fakeEmbedder{}, "new-embed", "new-embed", 64); !errors.Is(err, store.ErrEmbedDim) {
		t.Fatalf("backfill of a key of two lengths: %v", err)
	}
	rep, err := Run(reg, root, "proj-mx", &fakeExtractor{}, RunOpts{Embedder: &fakeEmbedder{}, EmbedModel: "new-embed", DedupSim: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	named := false
	for _, s := range rep.DedupSkipped {
		named = named || (strings.HasPrefix(s, "proj-mx: ") && strings.Contains(s, "rebuild the key"))
	}
	if !named {
		t.Fatalf("dedup skipped %v; want proj-mx named with the rebuild reason", rep.DedupSkipped)
	}
}
