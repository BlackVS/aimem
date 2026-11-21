package curate

import (
	"errors"
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
