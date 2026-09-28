package curate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"aimem/internal/embed"
	"aimem/internal/schema"
	"aimem/internal/store"
)

type fakeExtractor struct {
	calls  int
	groups []GroupHint // groups seen on the last Extract call
	extra  []Proposal
}

func (f *fakeExtractor) Extract(events []store.StoredEvent, maxFacts int, groups []GroupHint) ([]Proposal, Usage, error) {
	f.calls++
	f.groups = groups
	return append([]Proposal{
		{Text: "team decided to use sqlite for journals", Kind: "decision",
			Scope: "project", Tags: []string{"storage"}, SourceEventIDs: []string{events[0].ID}},
		{Text: "user prefers concise commit messages", Kind: "preference", Scope: "user"},
	}, f.extra...), Usage{InputTokens: 100, OutputTokens: 20}, nil
}

func seed(t *testing.T, reg *store.Registry, proj string, n int) {
	t.Helper()
	db, err := reg.Open(proj)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		_, _, err := db.Append(&schema.Event{
			SchemaVersion: schema.Version, IdempotencyKey: "k" + string(rune('a'+i)),
			Client: "claude-code", SessionID: "s1", TurnID: "t" + string(rune('a'+i)),
			Kind: schema.KindTurn, Outcome: schema.OutcomeOK,
			TS: time.Now().UTC().Format(time.RFC3339), UserRequest: "req",
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestCurateDryRunAndWrite(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	seed(t, reg, "proj-a", 3)
	fx := &fakeExtractor{}

	// Dry run: proposals visible, nothing written, cursor untouched.
	rep, err := Run(reg, root, "proj-a", fx, RunOpts{DryRun: true})
	if err != nil || len(rep.Proposals) != 2 || rep.Written != 0 {
		t.Fatalf("dry run: %+v err=%v", rep, err)
	}
	if _, err := os.Stat(filepath.Join(root, "curate", "proj-a.cursor")); !os.IsNotExist(err) {
		t.Fatal("dry run advanced cursor")
	}

	// Real run: writes to project and user scopes, advances cursor.
	rep, err = Run(reg, root, "proj-a", fx, RunOpts{})
	if err != nil || rep.Written != 2 || rep.NewCursor == "" {
		t.Fatalf("write run: %+v err=%v", rep, err)
	}
	db, _ := reg.Open("proj-a")
	hits, _ := db.Recall("sqlite", store.RecallOpts{})
	if len(hits) != 1 || hits[0].Actor != "curator" || hits[0].Kind != "decision" || hits[0].Corroboration != 1 {
		t.Fatalf("project fact wrong: %+v", hits)
	}
	udb, _ := reg.Open(store.UserScopeProject)
	if uh, _ := udb.Recall("commit", store.RecallOpts{}); len(uh) != 1 {
		t.Fatalf("user fact missing: %+v", uh)
	}

	// Second run: no new events past cursor -> extractor not called again.
	before := fx.calls
	rep, err = Run(reg, root, "proj-a", fx, RunOpts{})
	if err != nil || rep.EventsRead != 0 || fx.calls != before {
		t.Fatalf("cursor not honored: %+v calls=%d", rep, fx.calls)
	}

	// New events + same proposals -> reassert, not duplicate.
	seed(t, reg, "proj-a", 5)
	rep, err = Run(reg, root, "proj-a", fx, RunOpts{})
	if err != nil || rep.Reasserted != 2 || rep.Written != 0 {
		t.Fatalf("reassert run: %+v err=%v", rep, err)
	}
}

func TestParseProposalsTolerant(t *testing.T) {
	for _, in := range []string{
		`[{"text":"x","kind":"fact","scope":"project"}]`,
		"```json\n[{\"text\":\"x\"}]\n```",
		"Here are the facts:\n[{\"text\":\"x\"}]\nDone.",
	} {
		got, err := parseProposals(in)
		if err != nil || len(got) != 1 {
			t.Errorf("parse %q: %v %v", in, got, err)
		}
	}
	if _, err := parseProposals("no json here"); err == nil {
		t.Error("accepted garbage")
	}
}

func TestCurateGroupPromotion(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	seed(t, reg, "proj-g", 2)
	db, _ := reg.Open("proj-g")
	if err := db.SetMeta("groups", `["group-webstack"]`); err != nil {
		t.Fatal(err)
	}
	fx := &fakeExtractor{extra: []Proposal{
		{Text: "all webstack services log in JSON", Kind: "convention", Scope: "group:webstack"},
		{Text: "sneaky fact for a group this project never declared", Kind: "fact", Scope: "group:undeclared"},
	}}
	rep, err := Run(reg, root, "proj-g", fx, RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	// The extractor is offered bare group names from project meta.
	if len(fx.groups) != 1 || fx.groups[0].Name != "webstack" {
		t.Errorf("groups offered to extractor: %v", fx.groups)
	}
	// Declared group received its fact; membership gate skipped the other.
	gdb, _ := reg.Open("group-webstack")
	if hits, _ := gdb.Recall("webstack", store.RecallOpts{}); len(hits) != 1 {
		t.Fatalf("group fact not landed: %+v", hits)
	}
	udb, _ := reg.Open("group-undeclared")
	if all, _ := udb.Memories(false); len(all) != 0 {
		t.Errorf("undeclared group received a fact: %+v", all)
	}
	if rep.Written != 3 || rep.Skipped != 1 {
		t.Errorf("written=%d skipped=%d, want 3/1", rep.Written, rep.Skipped)
	}
}

func TestCurateGroupCharterAndPolicyAll(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	seed(t, reg, "proj-m", 2)
	db, _ := reg.Open("proj-m")
	if err := db.SetMeta("groups", `["group-meta"]`); err != nil {
		t.Fatal(err)
	}
	gdb, _ := reg.Open("group-meta")
	if err := gdb.SetMeta("about", "the meta framework"); err != nil {
		t.Fatal(err)
	}
	if err := gdb.SetMeta("policy", "all"); err != nil {
		t.Fatal(err)
	}
	fx := &fakeExtractor{}
	rep, err := Run(reg, root, "proj-m", fx, RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	// The charter rides into the extractor's group hints.
	if len(fx.groups) != 1 || fx.groups[0].About != "the meta framework" {
		t.Errorf("charter not offered: %+v", fx.groups)
	}
	// Policy all: the project-scoped fact mirrored into the group with
	// origin provenance; the user-scoped one did not.
	if rep.Mirrored != 1 {
		t.Errorf("mirrored=%d, want 1", rep.Mirrored)
	}
	hits, _ := gdb.Recall("sqlite", store.RecallOpts{})
	if len(hits) != 1 || !slices.Contains(hits[0].Sources, "project:proj-m") {
		t.Fatalf("mirrored fact wrong: %+v", hits)
	}
	if uh, _ := gdb.Recall("commit", store.RecallOpts{}); len(uh) != 0 {
		t.Errorf("user fact leaked into group: %+v", uh)
	}

	// Re-running over new events reasserts the mirror, never duplicates.
	seed(t, reg, "proj-m", 4)
	rep, err = Run(reg, root, "proj-m", fx, RunOpts{})
	if err != nil || rep.Mirrored != 0 {
		t.Fatalf("mirror duplicated: %+v err=%v", rep, err)
	}
	if all, _ := gdb.Memories(false); len(all) != 1 {
		t.Errorf("group memories=%d, want 1", len(all))
	}
}

func TestCurateChapterRouting(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	seed(t, reg, "proj-c", 2)
	db, _ := reg.Open("proj-c")
	if err := db.SetMeta("groups", `["group-kb"]`); err != nil {
		t.Fatal(err)
	}
	gdb, _ := reg.Open("group-kb")
	if err := gdb.SetMeta("policy", "all"); err != nil {
		t.Fatal(err)
	}
	if err := gdb.SetMeta("chapters",
		`[{"name":"storage","about":"databases and files"},{"name":"ci","about":"pipelines"}]`); err != nil {
		t.Fatal(err)
	}
	fx := &fakeExtractor{extra: []Proposal{
		{Text: "all kb members deploy via woodpecker", Kind: "convention",
			Scope: "group:kb", Chapter: "ci"},
		{Text: "fact with a chapter the group never declared", Kind: "fact",
			Scope: "group:kb", Chapter: "bogus"},
	}}
	if _, err := Run(reg, root, "proj-c", fx, RunOpts{}); err != nil {
		t.Fatal(err)
	}
	// The extractor is offered the chapters.
	if len(fx.groups) != 1 || len(fx.groups[0].Chapters) != 2 ||
		fx.groups[0].Chapters[0].Name != "storage" {
		t.Errorf("chapters not offered: %+v", fx.groups)
	}
	// Group-scoped fact filed into its declared chapter.
	hits, _ := gdb.Recall("woodpecker", store.RecallOpts{})
	if len(hits) != 1 || !slices.Contains(hits[0].Tags, "chapter:ci") {
		t.Fatalf("chapter tag missing: %+v", hits)
	}
	// Undeclared chapter name never sticks as a tag.
	hits, _ = gdb.Recall("never declared", store.RecallOpts{})
	if len(hits) != 1 || slices.ContainsFunc(hits[0].Tags,
		func(s string) bool { return strings.HasPrefix(s, "chapter:") }) {
		t.Fatalf("bogus chapter stuck: %+v", hits)
	}
	// The fakeExtractor's default project fact mirrors with no chapter
	// (it proposed none) — mirroring still works alongside chapters.
	if hits, _ := gdb.Recall("sqlite", store.RecallOpts{}); len(hits) != 1 {
		t.Fatalf("mirror missing: %+v", hits)
	}
}

// fakeEmbedder maps texts to fixed 2-d vectors by keyword, so tests
// control similarity exactly: "sqlite" texts collide, others don't.
type fakeEmbedder struct{ calls int }

func (f *fakeEmbedder) Embed(texts []string) ([][]float32, int64, error) {
	f.calls++
	out := make([][]float32, len(texts))
	for i, t := range texts {
		switch {
		case strings.Contains(t, "sqlite"):
			out[i] = []float32{1, 0}
		default:
			out[i] = []float32{0, 1}
		}
	}
	return out, int64(len(texts) * 10), nil
}

func TestCurateSemanticDedup(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	seed(t, reg, "proj-dd", 2)
	db, _ := reg.Open("proj-dd")
	// An existing, differently-phrased fact with the same meaning (same
	// fake vector) as the extractor's "team decided to use sqlite ..."
	// proposal.
	oldID, _, err := db.Remember("sqlite is the chosen journal storage engine", "curator",
		store.RememberOpts{Tags: []string{"storage"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetEmbedding(oldID, "fake-embed", 2, embed.Encode([]float32{1, 0})); err != nil {
		t.Fatal(err)
	}
	fx := &fakeExtractor{}
	fe := &fakeEmbedder{}
	rep, err := Run(reg, root, "proj-dd", fx, RunOpts{
		Embedder: fe, EmbedModel: "fake-embed", DedupSim: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	// Same neighborhood, different wording: the incoming statement wins
	// and the old one is superseded (newest-wins, matching the
	// retroactive sweep) instead of the update being silently dropped.
	if rep.Superseded != 1 || rep.Deduped != 0 || rep.Reasserted != 0 || rep.EmbedTokens == 0 {
		t.Fatalf("dedup report: %+v", rep)
	}
	if len(rep.Conflicts) != 1 || rep.Conflicts[0].Action != "superseded" ||
		rep.Conflicts[0].OldID != oldID {
		t.Fatalf("conflict not reported: %+v", rep.Conflicts)
	}
	mems, _ := db.Memories(false)
	if len(mems) != 1 || mems[0].ID == oldID ||
		mems[0].Text != "team decided to use sqlite for journals" {
		t.Fatalf("memories after supersede: %+v", mems)
	}
	// Tags ride onto the replacement, and the old row survives
	// bi-temporally (evidence is never deleted).
	if !slices.Contains(mems[0].Tags, "storage") {
		t.Fatalf("tags not carried over: %+v", mems[0].Tags)
	}
	all, _ := db.Memories(true)
	var oldRow *store.Memory
	for i := range all {
		if all[i].ID == oldID {
			oldRow = &all[i]
		}
	}
	if oldRow == nil || oldRow.SupersededBy != mems[0].ID {
		t.Fatalf("old fact not retired bi-temporally: %+v", oldRow)
	}
	// The distinct user-scope fact inserted fresh WITH its embedding
	// stored, so backfill has nothing to do.
	udb, _ := reg.Open(store.UserScopeProject)
	if targets, _ := udb.NeedingEmbedding("fake-embed", 10); len(targets) != 0 {
		t.Fatalf("fresh insert missing embedding: %+v", targets)
	}
	// A dedup-embed usage row is metered under the embed model.
	runs, _ := db.CurateRuns()
	var sawEmbedRow bool
	for _, r := range runs {
		if r.Model == "fake-embed" && r.InputTokens > 0 {
			sawEmbedRow = true
		}
	}
	if !sawEmbedRow {
		t.Fatalf("embed spend not metered: %+v", runs)
	}
}

// A pinned fact is human-protected: a divergent proposal must never
// rewrite it automatically — it folds and the clash is reported.
func TestCurateDivergenceKeepsPinned(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	seed(t, reg, "proj-pin", 2)
	db, _ := reg.Open("proj-pin")
	oldID, _, err := db.Remember("sqlite is the chosen journal storage engine", "human",
		store.RememberOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetEmbedding(oldID, "fake-embed", 2, embed.Encode([]float32{1, 0})); err != nil {
		t.Fatal(err)
	}
	if err := db.Pin(oldID, true, "human"); err != nil {
		t.Fatal(err)
	}
	rep, err := Run(reg, root, "proj-pin", &fakeExtractor{}, RunOpts{
		Embedder: &fakeEmbedder{}, EmbedModel: "fake-embed", DedupSim: 0.9})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Superseded != 0 || rep.Deduped != 1 || rep.Reasserted != 1 {
		t.Fatalf("pinned fact was not protected: %+v", rep)
	}
	if len(rep.Conflicts) != 1 || rep.Conflicts[0].Action != "kept-pinned" {
		t.Fatalf("clash not reported: %+v", rep.Conflicts)
	}
	mems, _ := db.Memories(false)
	if len(mems) != 1 || mems[0].ID != oldID {
		t.Fatalf("pinned memory replaced: %+v", mems)
	}
}

func TestDedupProjectRetroactive(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	db, _ := reg.Open("proj-sweep")
	put := func(text string, vec []float32, tags ...string) string {
		id, _, err := db.Remember(text, "curator", store.RememberOpts{Tags: tags})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetEmbedding(id, "fake-embed", len(vec), embed.Encode(vec)); err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldID := put("sqlite chosen as journal store", []float32{1, 0}, "storage")
	newID := put("the journal store is sqlite", []float32{1, 0.01}, "db")
	keep := put("deploys run via woodpecker", []float32{0, 1}, "ci")

	// Dry run reports the pair, changes nothing.
	res, err := DedupProject(db, "fake-embed", 0.9, true)
	if err != nil || res.Folded != 1 || len(res.Pairs) != 1 {
		t.Fatalf("dry run: %+v err=%v", res, err)
	}
	if mems, _ := db.Memories(false); len(mems) != 3 {
		t.Fatalf("dry run mutated: %d memories", len(mems))
	}

	// Real run: newer survives with merged tags, older retired, distinct kept.
	res, err = DedupProject(db, "fake-embed", 0.9, false)
	if err != nil || res.Folded != 1 || res.Pairs[0].KeptID != newID || res.Pairs[0].DroppedID != oldID {
		t.Fatalf("sweep: %+v err=%v", res, err)
	}
	mems, _ := db.Memories(false)
	if len(mems) != 2 {
		t.Fatalf("after sweep: %d memories", len(mems))
	}
	for _, m := range mems {
		switch m.ID {
		case newID:
			if !slices.Contains(m.Tags, "storage") || !slices.Contains(m.Tags, "db") {
				t.Fatalf("tags not merged: %+v", m.Tags)
			}
		case keep:
		default:
			t.Fatalf("unexpected survivor %s", m.ID)
		}
	}
	// Idempotent: nothing left to fold.
	if res, _ := DedupProject(db, "fake-embed", 0.9, false); res.Folded != 0 {
		t.Fatalf("not idempotent: %+v", res)
	}
}

func seedEvent(t *testing.T, db *store.DB, key string) {
	t.Helper()
	_, _, err := db.Append(&schema.Event{
		SchemaVersion: schema.Version, IdempotencyKey: "bud-" + key,
		Client: "claude-code", SessionID: "sb", TurnID: "t-" + key,
		Kind: schema.KindTurn, Outcome: schema.OutcomeOK,
		TS: time.Now().UTC().Format(time.RFC3339), UserRequest: "req " + key,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBudgetGate(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	seed(t, reg, "proj-b", 2)
	db, _ := reg.Open("proj-b")

	// Global token cap on the user DB; usage already near the cap.
	udb, _ := reg.Open(store.UserScopeProject)
	if err := SaveBudget(udb, &Budget{Daily: &Cap{Tokens: 30_000}}); err != nil {
		t.Fatal(err)
	}
	db.AddCurateRun(&store.CurateRun{TS: time.Now().UTC().Format(time.RFC3339),
		InputTokens: 25_000, OutputTokens: 1_000})

	fx := &fakeExtractor{}
	// Projection (~26k) + used (26k) crosses 30k -> refused before extract.
	_, err := Run(reg, root, "proj-b", fx, RunOpts{})
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("expected budget refusal, got err=%v", err)
	}
	if fx.calls != 0 {
		t.Fatal("extractor was called despite exhausted budget")
	}
	// Force bypasses.
	if _, err := Run(reg, root, "proj-b", fx, RunOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if fx.calls != 1 {
		t.Fatal("force did not run")
	}
	// Reset (epoch now) restarts the window and unblocks.
	// Epoch strictly after the recorded runs (RFC3339 has second
	// resolution, so same-second runs would still be counted).
	if err := SaveBudget(udb, &Budget{Daily: &Cap{Tokens: 30_000},
		Epoch: time.Now().UTC().Add(2 * time.Second).Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	seedEvent(t, db, "reset-1")
	if _, err := Run(reg, root, "proj-b", fx, RunOpts{}); err != nil {
		t.Fatalf("post-reset run refused: %v", err)
	}
	// USD cap without prices refuses (never spend unpriced).
	if err := SaveBudget(udb, &Budget{Monthly: &Cap{USD: 5}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEM_PRICE_IN", "")
	t.Setenv("AIMEM_PRICE_OUT", "")
	seedEvent(t, db, "usd-1")
	if _, err := Run(reg, root, "proj-b", fx, RunOpts{}); err == nil ||
		!strings.Contains(err.Error(), "AIMEM_PRICE_IN") {
		t.Fatalf("USD cap without prices not refused: %v", err)
	}
	// With prices set and room in the cap, it runs.
	t.Setenv("AIMEM_PRICE_IN", "1.0")
	t.Setenv("AIMEM_PRICE_OUT", "4.0")
	if _, err := Run(reg, root, "proj-b", fx, RunOpts{}); err != nil {
		t.Fatalf("priced USD run refused: %v", err)
	}
}

func TestBudgetDirectionalCaps(t *testing.T) {
	root := t.TempDir()
	reg, _ := store.NewRegistry(root)
	defer reg.Close()
	db, _ := reg.Open("proj-d")
	seedEvent(t, db, "d1")
	udb, _ := reg.Open(store.UserScopeProject)
	// Output cap tiny, input cap roomy: the out dimension must block alone.
	if err := SaveBudget(udb, &Budget{Daily: &Cap{TokensIn: 10_000_000, TokensOut: 500}}); err != nil {
		t.Fatal(err)
	}
	db.AddCurateRun(&store.CurateRun{TS: time.Now().UTC().Format(time.RFC3339),
		InputTokens: 100, OutputTokens: 400})
	fx := &fakeExtractor{}
	_, err := Run(reg, root, "proj-d", fx, RunOpts{})
	if err == nil || !strings.Contains(err.Error(), "output cap") {
		t.Fatalf("out cap did not block: %v", err)
	}
	// Raise the out cap: passes.
	if err := SaveBudget(udb, &Budget{Daily: &Cap{TokensIn: 10_000_000, TokensOut: 50_000}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(reg, root, "proj-d", fx, RunOpts{}); err != nil {
		t.Fatalf("roomy caps blocked: %v", err)
	}
}

func TestParseClaudeResultCountsCachedTokens(t *testing.T) {
	// The headless CLI's top-level input_tokens is only the UNCACHED
	// slice; a real extraction rides prompt caching almost entirely.
	out := []byte(`{"type":"result","is_error":false,"result":"[]",
		"total_cost_usd":0.0123,
		"usage":{"input_tokens":42,"cache_creation_input_tokens":18000,
		         "cache_read_input_tokens":9500,"output_tokens":310}}`)
	content, u, err := parseClaudeResult(out)
	if err != nil || content != "[]" {
		t.Fatalf("parse: %q err=%v", content, err)
	}
	if u.InputTokens != 42+18000+9500 {
		t.Fatalf("input tokens must include cache creation+read: %d", u.InputTokens)
	}
	if u.OutputTokens != 310 || u.CostUSD != 0.0123 {
		t.Fatalf("usage: %+v", u)
	}
	// An errored turn still reports its usage (spend happened).
	out = []byte(`{"is_error":true,"result":"boom","usage":{"input_tokens":5,"output_tokens":1}}`)
	if _, u, err := parseClaudeResult(out); err == nil || u.InputTokens != 5 {
		t.Fatalf("errored turn: err=%v usage=%+v", err, u)
	}
}

func TestSetCursor(t *testing.T) {
	root := t.TempDir()
	if err := SetCursor(root, "proj-x", "0199-abc"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(cursorPath(root, "proj-x"))
	if err != nil || strings.TrimSpace(string(b)) != "0199-abc" {
		t.Fatalf("cursor content %q err=%v", b, err)
	}
	if err := SetCursor(root, "proj-x", ""); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(cursorPath(root, "proj-x"))
	if strings.TrimSpace(string(b)) != "" {
		t.Fatalf("reset cursor not empty: %q", b)
	}
}

// helperStamp names a file the sleeping helper process writes its start
// time and PID to, and reads it back: when the helper actually began
// running, or false if it never ran far enough to write it.
func helperStamp(t *testing.T, name string) (string, func() (time.Time, int, bool)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	return path, func() (time.Time, int, bool) {
		b, err := os.ReadFile(path)
		if err != nil {
			return time.Time{}, 0, false
		}
		f := strings.Fields(string(b))
		if len(f) != 2 {
			t.Fatalf("helper stamp %s: %q", name, b)
		}
		ns, err1 := strconv.ParseInt(f[0], 10, 64)
		pid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			t.Fatalf("helper stamp %s: %q", name, b)
		}
		return time.Unix(0, ns), pid, true
	}
}

// TestClaudeExtractorTimeout: a hung CLI must be killed at the bound
// instead of blocking the hourly sweep forever (arch review S4). Uses
// the helper-process pattern: the test binary re-runs itself as a
// "claude" that sleeps 30 s, past the (shrunk) timeout.
//
// The bound is measured from when the helper began running, not from
// before Complete: creating the process is the OS's time, not the kill's,
// and on a loaded Windows machine it once pushed a wall-clock measure to
// 8.5 s (task 01a0e686). A helper that never ran was killed at start,
// which bounds it too; returning before its 30 s sleep is the proof.
func TestClaudeExtractorTimeout(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEM_LLM_INTERVAL", "0") // keep llmrate.Wait out of the measured window
	t.Setenv("GO_AIMEM_HELPER", "sleep")
	stamp, started := helperStamp(t, "helper-started")
	t.Setenv("GO_AIMEM_HELPER_STAMP", stamp)
	c := &ClaudeExtractor{WorkDir: t.TempDir(), Bin: exe, Timeout: 300 * time.Millisecond}
	begin := time.Now()
	_, _, err = c.Complete("prompt")
	done := time.Now()
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout must wrap context.DeadlineExceeded for callers: %v", err)
	}
	if total := done.Sub(begin); total >= 30*time.Second {
		t.Fatalf("Complete took %s: the hung helper ran to its own exit, it was never killed", total)
	}
	if at, _, ok := started(); ok {
		if ran := done.Sub(at); ran > 5*time.Second {
			t.Fatalf("kill took %s after the helper started (process start took %s): process not bounded", ran, at.Sub(begin))
		}
	}
}

// TestClaudeExtractorStdinRoundTrip: the prompt must reach the CLI over
// stdin (never argv — Windows 32K limit, cmd.exe unquoting), and a
// clean completion must parse and report success. The helper echoes its
// stdin back inside the CLI's JSON wrapper.
func TestClaudeExtractorStdinRoundTrip(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEM_LLM_INTERVAL", "0")
	t.Setenv("GO_AIMEM_HELPER", "echo")
	c := &ClaudeExtractor{WorkDir: t.TempDir(), Bin: exe}
	prompt := "line one\nline two with \"quotes\" and %PATH%"
	res, u, err := c.Complete(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if res != prompt {
		t.Fatalf("stdin round trip mangled: %q", res)
	}
	if u.OutputTokens != 7 {
		t.Fatalf("usage not parsed: %+v", u)
	}
}

// TestClaudeExtractorTimeoutWithGrandchild: the case the plain kill
// does NOT cover (max-review finding on PR #17): ctx expiry kills only
// the direct child, and a spawned grandchild inheriting stdout keeps
// cmd.Output() blocked past the kill — only cmd.WaitDelay force-closes
// the pipes and bounds Complete. The helper spawns a sleeping child
// wired to its own stdout, then sleeps.
func TestClaudeExtractorTimeoutWithGrandchild(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEM_LLM_INTERVAL", "0")
	t.Setenv("GO_AIMEM_HELPER", "spawn")
	old := claudeWaitDelay
	claudeWaitDelay = 1 * time.Second
	defer func() { claudeWaitDelay = old }()

	stamp, started := helperStamp(t, "grandchild-started")
	t.Setenv("GO_AIMEM_HELPER_STAMP", stamp)
	// The timeout leaves the helper ample time to start and spawn its
	// grandchild before the kill; the kill still lands long before the
	// grandchild would let go of the pipe on its own.
	c := &ClaudeExtractor{WorkDir: t.TempDir(), Bin: exe, Timeout: 3 * time.Second}
	_, _, err = c.Complete("prompt")
	done := time.Now()
	born, pid, ok := started()
	if ok {
		// On Windows a live grandchild wedges TempDir removal and the test
		// binary's deletion; it is ended here rather than slept out.
		t.Cleanup(func() {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
				p.Wait()
			}
		})
	}
	if err == nil {
		t.Fatal("want an error from the killed CLI")
	}
	// The anti-vacuous check: the grandchild really started and held the
	// pipe, or this test proved nothing about WaitDelay.
	if !ok {
		t.Fatal("the grandchild never started: the helper's spawn failed, and nothing held the pipe")
	}
	// The point of the test, measured from the grandchild's own start so
	// the OS's process-creation time is outside it: the grandchild holds
	// the pipe for 60 s, and only WaitDelay's forced close lets Complete
	// return within half of that.
	if held := done.Sub(born); held >= 30*time.Second {
		t.Fatalf("Complete blocked %s after the grandchild started: it held the pipe past WaitDelay", held)
	}
}

// stampHelperStart records, for the timeout tests, when the sleeping helper
// began running and its PID: GO_AIMEM_HELPER_STAMP names the file. It is
// written aside and renamed into place, so a kill mid-write leaves no
// stamp rather than a partial one.
func stampHelperStart() {
	if p := os.Getenv("GO_AIMEM_HELPER_STAMP"); p != "" {
		if os.WriteFile(p+".tmp", []byte(fmt.Sprintf("%d %d", time.Now().UnixNano(), os.Getpid())), 0o600) == nil {
			os.Rename(p+".tmp", p)
		}
	}
}

// TestMain doubles as the helper process: re-invoked with -p by the
// tests above it plays a claude CLI that hangs ("sleep": 30s, killed
// by the ctx), hangs after spawning a stdout-inheriting grandchild
// ("spawn"; the grandchild, "hold", keeps the pipe for 60s from the
// system temp dir until the test ends it), or echoes stdin back inside
// the CLI's JSON wrapper ("echo") — it gates every test run in this
// package, so keep the guard exact.
func TestMain(m *testing.M) {
	if mode := os.Getenv("GO_AIMEM_HELPER"); mode != "" && len(os.Args) > 1 && os.Args[1] == "-p" {
		switch mode {
		case "echo":
			in, _ := io.ReadAll(os.Stdin)
			json.NewEncoder(os.Stdout).Encode(map[string]any{
				"result": string(in), "is_error": false,
				"usage": map[string]any{"input_tokens": 3, "output_tokens": 7},
			})
		case "spawn":
			exe, _ := os.Executable()
			child := exec.Command(exe, "-p")
			child.Env = append(os.Environ(), "GO_AIMEM_HELPER=hold")
			child.Dir = os.TempDir() // never hold the test's TempDir
			child.Stdout = os.Stdout // inherit the pipe: the orphan that used to wedge Output()
			if err := child.Start(); err != nil {
				os.Exit(3) // loud, immediate: no grandchild stamp, and the test says so
			}
			time.Sleep(30 * time.Second)
		case "hold":
			stampHelperStart()
			time.Sleep(60 * time.Second)
		default: // "sleep"
			stampHelperStart()
			time.Sleep(30 * time.Second)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}
