package store

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"aimem/internal/embed"
)

func TestRememberRecallForget(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	id, re, err := db.Remember("user prefers table-driven tests in Go", "test", RememberOpts{Sources: []string{"ev1"}, Kind: "preference", Tags: []string{"testing", "go"}})
	if err != nil || re {
		t.Fatalf("remember: %v re=%v", err, re)
	}
	hits, err := db.Recall("table-driven", RecallOpts{})
	if err != nil || len(hits) != 1 || hits[0].ID != id || hits[0].Corroboration != 1 {
		t.Fatalf("recall: err=%v hits=%+v", err, hits)
	}
	if err := db.Forget(id, "test"); err != nil {
		t.Fatal(err)
	}
	if hits, _ := db.Recall("table-driven", RecallOpts{}); len(hits) != 0 {
		t.Errorf("forgotten memory still recalled: %+v", hits)
	}
	// Row survives with bi-temporal expiry.
	all, _ := db.Memories(true)
	if len(all) != 1 || all[0].ExpiredAt == "" || all[0].InvalidAt == "" {
		t.Errorf("expiry not bi-temporal: %+v", all)
	}
}

func TestReassertAppendsSource(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	id1, _, _ := db.Remember("fact one", "a1", RememberOpts{Sources: []string{"ev1"}})
	id2, re, err := db.Remember("fact one", "a2", RememberOpts{Sources: []string{"ev2"}})
	if err != nil || !re || id2 != id1 {
		t.Fatalf("reassert: %v re=%v id1=%s id2=%s", err, re, id1, id2)
	}
	hits, _ := db.Recall("fact", RecallOpts{})
	if len(hits) != 1 || hits[0].Corroboration != 2 {
		t.Fatalf("corroboration: %+v", hits)
	}
}

func TestSupersede(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	oldID, _, _ := db.Remember("we deploy on fridays", "test", RememberOpts{})
	newID, err := db.Supersede(oldID, "we deploy on tuesdays", "test", RememberOpts{})
	if err != nil {
		t.Fatal(err)
	}
	hits, _ := db.Recall("deploy", RecallOpts{})
	if len(hits) != 1 || hits[0].ID != newID {
		t.Fatalf("recall after supersede: %+v", hits)
	}
	all, _ := db.Memories(true)
	var old *Memory
	for i := range all {
		if all[i].ID == oldID {
			old = &all[i]
		}
	}
	if old == nil || old.SupersededBy != newID || old.ExpiredAt == "" {
		t.Errorf("old row not superseded correctly: %+v", old)
	}
}

func TestPinProtectsAndRanksFirst(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	idA, _, _ := db.Remember("build uses make target alpha", "t", RememberOpts{})
	idB, _, _ := db.Remember("build cache lives in beta dir; build build build", "t", RememberOpts{})
	_ = idB
	if err := db.Pin(idA, true, "t"); err != nil {
		t.Fatal(err)
	}
	if err := db.Forget(idA, "t"); err == nil {
		t.Error("forget succeeded on pinned memory")
	}
	hits, _ := db.Recall("build", RecallOpts{})
	if len(hits) != 2 || hits[0].ID != idA {
		t.Fatalf("pinned not first: %+v", hits)
	}
}

func TestRecallTokenBudget(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	for i := range 20 {
		db.Remember("budget filler fact number "+strings.Repeat("x", 200)+string(rune('a'+i)), "t", RememberOpts{})
	}
	hits, _ := db.Recall("budget", RecallOpts{TokenBudget: 100}) // ~400 chars budget
	if len(hits) == 0 || len(hits) > 3 {
		t.Fatalf("budget not enforced: %d hits", len(hits))
	}
}

func TestMemoryRedactionOnWrite(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	db.Remember("proxy needs Authorization: Bearer topsecrettoken99 header", "t", RememberOpts{})
	hits, _ := db.Recall("proxy", RecallOpts{})
	if len(hits) != 1 || strings.Contains(hits[0].Text, "topsecrettoken99") {
		t.Fatalf("secret persisted in memory: %+v", hits)
	}
}

func TestMemoryImportMerge(t *testing.T) {
	r := newTestRegistry(t)
	dbA, _ := r.Open("proj-a")
	dbB, _ := r.Open("proj-b")
	id, _, _ := dbA.Remember("shared fact", "t", RememberOpts{Sources: []string{"ev1"}})
	memsA, _ := dbA.Memories(true)
	// First import into B.
	if err := dbB.ImportMemory(&memsA[0]); err != nil {
		t.Fatal(err)
	}
	// A forgets; re-import must propagate staleness to B.
	dbA.Forget(id, "t")
	memsA, _ = dbA.Memories(true)
	if err := dbB.ImportMemory(&memsA[0]); err != nil {
		t.Fatal(err)
	}
	if hits, _ := dbB.Recall("shared", RecallOpts{}); len(hits) != 0 {
		t.Errorf("staleness did not propagate on import: %+v", hits)
	}
	// Idempotent re-import.
	if err := dbB.ImportMemory(&memsA[0]); err != nil {
		t.Fatal(err)
	}
	all, _ := dbB.Memories(true)
	if len(all) != 1 {
		t.Errorf("import not idempotent: %d rows", len(all))
	}
}

func TestUserScopeReservedProject(t *testing.T) {
	r := newTestRegistry(t)
	db, err := r.Open(UserScopeProject)
	if err != nil {
		t.Fatal(err)
	}
	db.Remember("user-level preference", "t", RememberOpts{})
	ids, _ := r.Projects()
	found := false
	for _, id := range ids {
		if id == UserScopeProject {
			found = true
		}
	}
	if !found {
		t.Errorf("user scope project missing: %v", ids)
	}
}

func TestKnowledgeStructure(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	id1, _, _ := db.Remember("we chose SQLite over Postgres for zero ops", "t",
		RememberOpts{Kind: "decision", Tags: []string{"storage", "SQLite"}})
	id2, _, _ := db.Remember("journal.db lives under the state root", "t",
		RememberOpts{Kind: "fact", Tags: []string{"storage"}})
	if err := db.Link(id1, id2, "related", "t"); err != nil {
		t.Fatal(err)
	}
	// Tag filter narrows; kind filter narrows; links and structure surface.
	hits, err := db.Recall("storage sqlite", RecallOpts{Tag: "storage", Kind: "decision"})
	if err != nil || len(hits) != 1 || hits[0].ID != id1 {
		t.Fatalf("filtered recall: err=%v hits=%+v", err, hits)
	}
	if hits[0].Kind != "decision" || len(hits[0].Tags) != 2 || len(hits[0].Links) != 1 {
		t.Errorf("structure missing: %+v", hits[0])
	}
	// Reassertion reinforces confidence.
	before := hits[0].Confidence
	db.Remember("we chose SQLite over Postgres for zero ops", "t2", RememberOpts{})
	hits, _ = db.Recall("sqlite", RecallOpts{})
	if len(hits) == 0 || hits[0].Confidence <= before {
		t.Errorf("confidence not reinforced: before=%v after=%+v", before, hits)
	}
	// OR semantics: a query where only one term matches still hits.
	orHits, _ := db.Recall("sqlite zeppelin xylophone", RecallOpts{})
	if len(orHits) == 0 {
		t.Error("OR recall failed")
	}
	// Unknown kind coerces to fact rather than erroring.
	id3, _, _ := db.Remember("misc note", "t", RememberOpts{Kind: "invented"})
	all, _ := db.Memories(false)
	for _, m := range all {
		if m.ID == id3 && m.Kind != "fact" {
			t.Errorf("kind not coerced: %+v", m)
		}
	}
}

func TestMigrationV2ToV3(t *testing.T) {
	// Simulate an existing v2 database by rolling meta back after creation
	// is impossible cleanly; instead verify a fresh open lands on the
	// current schema and old v2-shaped ImportMemory payloads (no kind/tags)
	// still import.
	r := newTestRegistry(t)
	db, _ := r.Open("proj-a")
	var v string
	db.sql.QueryRow("SELECT value FROM meta WHERE key='schema_version'").Scan(&v)
	if v != fmt.Sprint(currentSchema) {
		t.Fatalf("schema version = %s, want %d", v, currentSchema)
	}
	if err := db.ImportMemory(&Memory{ID: "01a0-legacy", Text: "old shape", CreatedAt: nowUTC()}); err != nil {
		t.Fatalf("legacy import: %v", err)
	}
}

func TestHybridRecallVectorLeg(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-v")
	// Fact with NO keyword overlap with the query — only the vector leg can
	// surface it; a keyword-matching decoy proves BM25 still participates.
	semID, _, err := db.Remember("deploys go out via the blue-green pipeline", "test", RememberOpts{Kind: "fact"})
	if err != nil {
		t.Fatal(err)
	}
	decoyID, _, err := db.Remember("release notes are written by hand", "test", RememberOpts{Kind: "fact"})
	if err != nil {
		t.Fatal(err)
	}
	// Synthetic 3-dim embeddings: semantic fact ~ query, decoy orthogonal.
	set := func(id string, v []float32) {
		t.Helper()
		if err := db.SetEmbedding(id, "test-model", 3, embed.Encode(v)); err != nil {
			t.Fatal(err)
		}
	}
	set(semID, []float32{1, 0, 0})
	set(decoyID, []float32{0, 1, 0})

	hits, err := db.Recall("how does release shipping work", RecallOpts{
		QueryVec: []float32{0.9, 0.1, 0}, EmbedModel: "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	found := false
	for _, id := range ids {
		if id == semID {
			found = true
		}
	}
	if !found {
		t.Fatalf("vector-only fact not recalled; got %v (sem=%s decoy=%s)", ids, semID, decoyID)
	}
	// Without a query vector, the semantic fact is invisible to this query.
	bm25, err := db.Recall("how does release shipping work", RecallOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range bm25 {
		if h.ID == semID {
			t.Fatal("BM25-only recall unexpectedly matched the no-overlap fact")
		}
	}
}

// Chapters are labels: a fact may be filed in several deliberately, but
// the MERGE path (dedup/reassert folding a twin's tags in) must never
// cross-file it, and the first filing stays primary.
func TestMultiLabelChapters(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("group-multi")
	id, _, err := db.Remember("gemini embeddings ride the OpenAI-compat endpoint", "test",
		chapterOpts("chapter:model-routing"))
	if err != nil {
		t.Fatal(err)
	}
	// Merge path: a twin filed elsewhere must not add its chapter.
	db.Remember("gemini embeddings ride the OpenAI-compat endpoint", "test",
		chapterOpts("chapter:hub-operations"))
	if n := db.ChapterCount(id); n != 1 {
		t.Fatalf("merge path cross-filed: %d chapters", n)
	}
	// Explicit path: deliberate multi-filing up to the cap.
	if err := db.Tag(id, "chapter:hub-operations", "human"); err != nil {
		t.Fatal(err)
	}
	if err := db.Tag(id, "chapter:client-setup", "human"); err != nil {
		t.Fatal(err)
	}
	if n := db.ChapterCount(id); n != 3 {
		t.Fatalf("explicit filing blocked: %d chapters", n)
	}
	// Cap holds, and re-filing an existing chapter is a no-op, not an error.
	if err := db.Tag(id, "chapter:fourth", "human"); err == nil {
		t.Fatal("cap not enforced")
	}
	if err := db.Tag(id, "chapter:model-routing", "human"); err != nil {
		t.Fatalf("re-filing an existing chapter should be a no-op: %v", err)
	}
	// Primary = first filed: tag order is deterministic, and the doc
	// generator takes the first chapter tag.
	mems, _ := db.Memories(false)
	var tags []string
	for _, m := range mems {
		if m.ID == id {
			tags = m.Tags
		}
	}
	first := ""
	for _, tg := range tags {
		if strings.HasPrefix(tg, "chapter:") {
			first = tg
			break
		}
	}
	if first != "chapter:model-routing" {
		t.Fatalf("primary chapter drifted: %q (tags %v)", first, tags)
	}
	// Unfiling one label leaves the others.
	if err := db.RemoveTag(id, "chapter:client-setup"); err != nil {
		t.Fatal(err)
	}
	if n := db.ChapterCount(id); n != 2 {
		t.Fatalf("after unfile: %d chapters", n)
	}
}

func chapterOpts(chapter string) RememberOpts {
	return RememberOpts{Tags: []string{chapter}}
}

// The audit trail is how curation's own process reports what it changed:
// supersession and observed-but-not-applied conflicts must both land.
func TestAuditLogRecordsConflicts(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-audit")
	oldID, _, err := db.Remember("hub has 512MB RAM", "curator", RememberOpts{})
	if err != nil {
		t.Fatal(err)
	}
	newID, err := db.Supersede(oldID, "hub has 1GB RAM", "curator", RememberOpts{})
	if err != nil {
		t.Fatal(err)
	}
	pinned, _, _ := db.Remember("embeddings are gemini-embedding-001", "human", RememberOpts{})
	db.Pin(pinned, true, "human")
	db.NoteConflict(pinned, "embeddings are gemini-embedding-001", "embeddings are text-embedding-3-large", "curator")

	entries, err := db.AuditLog(50)
	if err != nil {
		t.Fatal(err)
	}
	var sup, conf *AuditEntry
	for i := range entries {
		switch entries[i].Op {
		case "supersede":
			sup = &entries[i]
		case "conflict":
			conf = &entries[i]
		}
	}
	if sup == nil || sup.MemoryID != oldID || sup.OldText != "hub has 512MB RAM" ||
		sup.NewText != "hub has 1GB RAM" || sup.Actor != "curator" {
		t.Fatalf("supersede not auditable: %+v", sup)
	}
	if conf == nil || conf.MemoryID != pinned || conf.Actor != "curator" ||
		!strings.Contains(conf.NewText, "text-embedding-3-large") {
		t.Fatalf("pinned clash not auditable: %+v", conf)
	}
	if newID == "" {
		t.Fatal("supersede returned no id")
	}
	// Newest first, so the GUI shows recent activity without scrolling.
	if len(entries) < 2 || entries[0].TS < entries[len(entries)-1].TS {
		t.Fatalf("audit order not newest-first: %+v", entries)
	}
}

// Superseding a fact with UNCHANGED text must not destroy it: Remember
// folds the assertion back into the same row, and retiring that row
// would expire the fact and point it at itself (observed live in the
// admin GUI, where the prompt defaults to the current text).
func TestSupersedeWithIdenticalTextIsReassertion(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-sup")
	id, _, err := db.Remember("home hub has 1GB RAM", "curator", RememberOpts{})
	if err != nil {
		t.Fatal(err)
	}
	newID, err := db.Supersede(id, "home hub has 1GB RAM", "human", RememberOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if newID != id {
		t.Fatalf("identical text should fold onto the same row: %s vs %s", newID, id)
	}
	live, _ := db.Memories(false)
	if len(live) != 1 || live[0].ID != id || live[0].SupersededBy != "" {
		t.Fatalf("fact destroyed by self-supersession: %+v", live)
	}
	// A genuine change still supersedes.
	if _, err := db.Supersede(id, "home hub has 2GB RAM", "human", RememberOpts{}); err != nil {
		t.Fatal(err)
	}
	live, _ = db.Memories(false)
	if len(live) != 1 || live[0].Text != "home hub has 2GB RAM" {
		t.Fatalf("real supersede broken: %+v", live)
	}
}

func TestImportPreservesMultiChapterFilings(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-import-chapters")
	// Import replicates the SAME memory id, so the source machine's
	// explicit multi-chapter filing must survive — up to the cap.
	m := &Memory{
		ID: "mem-multi", Text: "fact filed in several chapters", Kind: "fact",
		Confidence: 0.9, CreatedAt: "2026-08-29T00:00:00Z", Actor: "test",
		Tags: []string{"chapter:alpha", "chapter:beta", "chapter:gamma", "chapter:delta", "topic"},
	}
	if err := db.ImportMemory(m); err != nil {
		t.Fatal(err)
	}
	if n := db.ChapterCount("mem-multi"); n != MaxChaptersPerFact {
		t.Fatalf("import kept %d chapters, want %d", n, MaxChaptersPerFact)
	}
	if !db.hasTag("mem-multi", "chapter:beta") || !db.hasTag("mem-multi", "topic") {
		t.Fatal("secondary chapter or plain tag lost on import")
	}
	// Re-import is idempotent.
	if err := db.ImportMemory(m); err != nil {
		t.Fatal(err)
	}
	if n := db.ChapterCount("mem-multi"); n != MaxChaptersPerFact {
		t.Fatalf("re-import changed chapter count to %d", n)
	}
}

func TestReviewQueueAndConfirm(t *testing.T) {
	r := newTestRegistry(t)
	db, _ := r.Open("proj-review")
	thin, _, err := db.Remember("thin old fact", "test", RememberOpts{Sources: []string{"e1"}})
	if err != nil {
		t.Fatal(err)
	}
	pinned, _, _ := db.Remember("pinned fact", "test", RememberOpts{})
	if err := db.Pin(pinned, true, "test"); err != nil {
		t.Fatal(err)
	}
	corr, _, _ := db.Remember("well-corroborated fact", "test",
		RememberOpts{Sources: []string{"a", "b", "c"}})
	// A fact that arrived by sync has no local audit history: last_seen
	// falls back to its created_at, so old imports queue immediately.
	if err := db.ImportMemory(&Memory{ID: "imported-old", Text: "synced years ago",
		Kind: "fact", Confidence: 0.5, CreatedAt: "2020-01-01T00:00:00Z", Actor: "peer"}); err != nil {
		t.Fatal(err)
	}

	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	q, err := db.ReviewQueue(future, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, it := range q {
		ids[it.ID] = true
	}
	if !ids[thin] || !ids["imported-old"] {
		t.Fatalf("queue missing candidates: %v", ids)
	}
	if ids[pinned] {
		t.Fatal("pinned fact must never queue")
	}
	if ids[corr] {
		t.Fatal("well-corroborated fact should be filtered at max 2")
	}
	// Oldest first: the 2020 import outranks today's fact.
	if q[0].ID != "imported-old" {
		t.Fatalf("ordering: %s first, want imported-old", q[0].ID)
	}

	// A confirm is an audited touch: the fact leaves the queue for any
	// cutoff before now, and confidence is modestly reinforced.
	var before float64
	db.sql.QueryRow(`SELECT confidence FROM memories WHERE id=?`, thin).Scan(&before)
	if err := db.Confirm(thin, "reviewer"); err != nil {
		t.Fatal(err)
	}
	recent := time.Now().UTC().Add(-time.Second).Format(time.RFC3339)
	q2, _ := db.ReviewQueue(recent, 2, 0)
	for _, it := range q2 {
		if it.ID == thin {
			t.Fatal("confirmed fact still queued")
		}
	}
	var after float64
	db.sql.QueryRow(`SELECT confidence FROM memories WHERE id=?`, thin).Scan(&after)
	if after <= before {
		t.Fatalf("confirm did not reinforce: %.2f -> %.2f", before, after)
	}
	log, _ := db.AuditLog(5)
	if len(log) == 0 || log[0].Op != "confirm" || log[0].Actor != "reviewer" {
		t.Fatalf("confirm not audited: %+v", log)
	}
	// Expired facts never queue; confirming a missing id refuses.
	if err := db.Forget("imported-old", "test"); err != nil {
		t.Fatal(err)
	}
	q3, _ := db.ReviewQueue(future, 2, 0)
	for _, it := range q3 {
		if it.ID == "imported-old" {
			t.Fatal("forgotten fact still queued")
		}
	}
	if err := db.Confirm("nope", "test"); err == nil {
		t.Fatal("confirming a missing id must refuse")
	}
}
