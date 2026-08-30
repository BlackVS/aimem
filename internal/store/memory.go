package store

// Curated knowledge (Phase 5 + knowledge-db structure). Design decisions
// from the proposal and the reference-app research:
//   - bi-temporal rows: created_at/expired_at are system time, valid_at/
//     invalid_at are event time; supersession is one UPDATE, never DELETE;
//   - provenance is a join table (memory_sources): re-asserting a fact
//     appends a source row and reinforces confidence instead of duplicating;
//   - typed facts (kind), entity tags, and inter-fact links give structure
//     without a graph server (Mem0/Graphiti lessons: value is the schema,
//     not the graph machinery);
//   - confidence follows a Hindsight-style rule: reinforcement bumps it,
//     supersession retires the old fact entirely;
//   - every mutation lands in an append-only audit table; recall trims to a
//     caller token budget; no LLM ever writes here destructively.
// User-scoped memories live in the reserved project "user"; group scopes in
// reserved projects "group-<name>".

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"aimem/internal/embed"
	"aimem/internal/redact"
	"aimem/internal/uuidv7"
)

// UserScopeProject is the reserved project id holding cross-project,
// user-scoped memories.
const UserScopeProject = "user"

// Memory kinds — a light taxonomy, not an epistemology.
var MemoryKinds = []string{"fact", "decision", "convention", "preference", "solution", "reference"}

// Memory is one curated fact.
type Memory struct {
	ID            string   `json:"id"`
	Text          string   `json:"text"`
	Kind          string   `json:"kind,omitempty"`
	Confidence    float64  `json:"confidence"`
	Tags          []string `json:"tags,omitempty"`
	Links         []string `json:"links,omitempty"` // "rel:memory-id"
	CreatedAt     string   `json:"created_at"`
	ExpiredAt     string   `json:"expired_at,omitempty"`
	ValidAt       string   `json:"valid_at,omitempty"`
	InvalidAt     string   `json:"invalid_at,omitempty"`
	SupersededBy  string   `json:"superseded_by,omitempty"`
	Pinned        bool     `json:"pinned,omitempty"`
	Actor         string   `json:"actor,omitempty"`
	Corroboration int      `json:"corroboration"`
	Sources       []string `json:"sources,omitempty"`
}

// RememberOpts carries the optional structure of a new fact.
type RememberOpts struct {
	ValidAt string
	Kind    string
	Tags    []string
	Sources []string // journal event ids
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func validKind(k string) string {
	for _, v := range MemoryKinds {
		if k == v {
			return k
		}
	}
	return "fact"
}

func normTag(t string) string {
	t = strings.ToLower(strings.TrimSpace(t))
	if len(t) > 64 {
		t = t[:64]
	}
	return t
}

// Remember stores a curated fact. If an identical active fact already
// exists, the assertion is folded into it: sources and tags are appended,
// confidence is reinforced, and the existing id is returned
// (reasserted=true).
func (d *DB) Remember(text, actor string, opts RememberOpts) (id string, reasserted bool, err error) {
	text, _ = redact.String(text, 8192)
	if text == "" {
		return "", false, errors.New("empty memory text")
	}
	var existing string
	err = d.sql.QueryRow(
		`SELECT id FROM memories WHERE text=? AND expired_at IS NULL AND superseded_by IS NULL`,
		text).Scan(&existing)
	if err == nil {
		// Hindsight-style reinforcement, clamped.
		if _, err := d.sql.Exec(`UPDATE memories SET confidence=MIN(1.0, confidence+0.15) WHERE id=?`, existing); err != nil {
			return "", false, err
		}
		if err := d.addSources(existing, opts.Sources); err != nil {
			return "", false, err
		}
		if err := d.addTags(existing, opts.Tags); err != nil {
			return "", false, err
		}
		if err := d.audit(existing, "reassert", "", text, actor); err != nil {
			return "", false, err
		}
		return existing, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", false, err
	}
	id = uuidv7.New()
	_, err = d.sql.Exec(`INSERT INTO memories(id, text, kind, created_at, valid_at, actor)
VALUES(?,?,?,?,?,?)`, id, text, validKind(opts.Kind), nowUTC(), nullable(opts.ValidAt), actor)
	if err != nil {
		return "", false, err
	}
	if err := d.addSources(id, opts.Sources); err != nil {
		return "", false, err
	}
	if err := d.addTags(id, opts.Tags); err != nil {
		return "", false, err
	}
	if err := d.audit(id, "remember", "", text, actor); err != nil {
		return "", false, err
	}
	return id, false, nil
}

// originAliases reads the DB's recorded project-id merges/renames
// ({old: new}). Source labels are unioned across copies during sync, so
// deleting an old label locally is not durable — a lagging peer pushes
// it right back. The alias makes the relabel permanent: every future
// import normalizes through it.
func (d *DB) originAliases() map[string]string {
	raw, _ := d.GetMeta("origin_aliases")
	if raw == "" {
		return nil
	}
	m := map[string]string{}
	json.Unmarshal([]byte(raw), &m)
	return m
}

func (d *DB) addSources(memoryID string, eventIDs []string) error {
	aliases := d.originAliases()
	for _, ev := range eventIDs {
		if ev == "" {
			continue
		}
		if len(aliases) > 0 {
			if id, ok := strings.CutPrefix(ev, "project:"); ok {
				if to, ok := aliases[id]; ok {
					ev = "project:" + to
				}
			}
		}
		if _, err := d.sql.Exec(`INSERT INTO memory_sources(memory_id, event_id) VALUES(?,?)
ON CONFLICT(memory_id, event_id) DO NOTHING`, memoryID, ev); err != nil {
			return err
		}
	}
	return nil
}

// MaxChaptersPerFact caps deliberate multi-filing. A fact may genuinely
// span a few chapters, but "everything everywhere" would destroy the
// reading order chapters exist to provide.
const MaxChaptersPerFact = 3

// ChapterCount reports how many knowledge-base chapters a fact is filed in.
func (d *DB) ChapterCount(memoryID string) int {
	var n int
	d.sql.QueryRow(`SELECT COUNT(*) FROM memory_tags
WHERE memory_id=? AND tag LIKE 'chapter:%'`, memoryID).Scan(&n)
	return n
}

func (d *DB) hasTag(memoryID, tag string) bool {
	var n int
	d.sql.QueryRow(`SELECT COUNT(*) FROM memory_tags WHERE memory_id=? AND tag=?`,
		memoryID, tag).Scan(&n)
	return n > 0
}

func (d *DB) addTags(memoryID string, tags []string) error {
	return d.addTagsOpt(memoryID, tags, false)
}

// addTagsOpt attaches tags, with one rule that depends on the caller.
// multiChapter=false is the MERGE path (dedup/reassert folding a twin's
// tags in): a fact keeps its FIRST filing, because a twin that was filed
// differently must never silently cross-file it. The explicit filing
// path (Tag) passes true: a human, or a reviewed refile plan, may place
// one fact in several chapters up to MaxChaptersPerFact. The first
// chapter filed stays PRIMARY — it is the one the design document files
// the fact under.
func (d *DB) addTagsOpt(memoryID string, tags []string, multiChapter bool) error {
	for _, t := range tags {
		if t = normTag(t); t == "" {
			continue
		}
		if strings.HasPrefix(t, "chapter:") && !d.hasTag(memoryID, t) {
			limit := 1
			if multiChapter {
				limit = MaxChaptersPerFact
			}
			if d.ChapterCount(memoryID) >= limit {
				continue
			}
		}
		if _, err := d.sql.Exec(`INSERT INTO memory_tags(memory_id, tag) VALUES(?,?)
ON CONFLICT(memory_id, tag) DO NOTHING`, memoryID, t); err != nil {
			return err
		}
	}
	return nil
}

// Tag attaches one tag to an active memory — the explicit filing path
// (human or reviewed refile plan). A fact may carry up to
// MaxChaptersPerFact chapters; the first one filed remains primary.
// Audited.
func (d *DB) Tag(memoryID, tag, actor string) error {
	var n int
	d.sql.QueryRow(`SELECT COUNT(*) FROM memories
		WHERE id=? AND expired_at IS NULL AND superseded_by IS NULL`, memoryID).Scan(&n)
	if n == 0 {
		return fmt.Errorf("no active memory %s", memoryID)
	}
	if t := normTag(tag); strings.HasPrefix(t, "chapter:") && !d.hasTag(memoryID, t) {
		if c := d.ChapterCount(memoryID); c >= MaxChaptersPerFact {
			return fmt.Errorf("memory %s is already filed in %d chapters (max %d) — unfile one first",
				memoryID, c, MaxChaptersPerFact)
		}
	}
	if err := d.addTagsOpt(memoryID, []string{tag}, true); err != nil {
		return err
	}
	return d.audit(memoryID, "tag", "", tag, actor)
}

// IsPinned reports whether an active memory is pinned — a human
// deliberately protected it, so automatic rewrites must leave it alone.
func (d *DB) IsPinned(id string) bool {
	var pinned int
	d.sql.QueryRow(`SELECT pinned FROM memories WHERE id=? AND expired_at IS NULL`, id).Scan(&pinned)
	return pinned != 0
}

// RemoveTag detaches one tag from a memory — the correction path for a
// mis-filed fact (drop its chapter:x tag, then reassert with the right
// one).
func (d *DB) RemoveTag(memoryID, tag string) error {
	res, err := d.sql.Exec(`DELETE FROM memory_tags WHERE memory_id=? AND tag=?`,
		memoryID, normTag(tag))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("memory %s has no tag %q", memoryID, tag)
	}
	return nil
}

// Link relates two memories (e.g. "related", "causes", "refines"). Both
// must be active rows in this database.
func (d *DB) Link(fromID, toID, rel, actor string) error {
	rel = normTag(rel)
	if rel == "" {
		rel = "related"
	}
	for _, id := range []string{fromID, toID} {
		var n int
		if err := d.sql.QueryRow(`SELECT COUNT(*) FROM memories WHERE id=? AND expired_at IS NULL`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no active memory %s", id)
		}
	}
	if _, err := d.sql.Exec(`INSERT INTO memory_links(from_id, to_id, rel) VALUES(?,?,?)
ON CONFLICT(from_id, to_id, rel) DO NOTHING`, fromID, toID, rel); err != nil {
		return err
	}
	return d.audit(fromID, "link", "", rel+" -> "+toID, actor)
}

// AuditEntry is one recorded mutation of curated knowledge.
type AuditEntry struct {
	TS       string `json:"ts"`
	MemoryID string `json:"memory_id"`
	Op       string `json:"op"`
	OldText  string `json:"old_text,omitempty"`
	NewText  string `json:"new_text,omitempty"`
	Actor    string `json:"actor,omitempty"`
}

// AuditLog returns recent knowledge mutations, newest first. The audit
// table is append-only; this is the read side the admin Log tab uses to
// show what the curator and operators actually changed.
func (d *DB) AuditLog(limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := d.sql.Query(`SELECT ts, memory_id, op, COALESCE(old_text,''),
		COALESCE(new_text,''), COALESCE(actor,'') FROM memory_audit
		ORDER BY ts DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.TS, &e.MemoryID, &e.Op, &e.OldText, &e.NewText, &e.Actor); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// NoteConflict records a divergence that was OBSERVED but not applied —
// an incoming fact that collided with a pinned one and was folded away.
// Nothing changed, so only the audit trail carries it; without this the
// clash would exist solely in the curate process's stderr.
func (d *DB) NoteConflict(memoryID, oldText, newText, actor string) error {
	return d.audit(memoryID, "conflict", oldText, newText, actor)
}

// audit appends one row to the append-only trail. The design promises
// every knowledge mutation is audited, so a failed write here is an
// error the mutation must surface — not a best-effort side note.
func (d *DB) audit(memoryID, op, oldText, newText, actor string) error {
	_, err := d.sql.Exec(`INSERT INTO memory_audit(id, memory_id, op, old_text, new_text, actor, ts)
VALUES(?,?,?,?,?,?,?)`, uuidv7.New(), memoryID, op, oldText, newText, actor, nowUTC())
	return err
}

// Forget retires a memory (bi-temporal expiry; the row and its audit trail
// remain). Pinned memories must be unpinned first.
func (d *DB) Forget(id, actor string) error {
	var text string
	var pinned int
	err := d.sql.QueryRow(`SELECT text, pinned FROM memories WHERE id=? AND expired_at IS NULL`, id).
		Scan(&text, &pinned)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no active memory %s", id)
	}
	if err != nil {
		return err
	}
	if pinned != 0 {
		return fmt.Errorf("memory %s is pinned; unpin before forgetting", id)
	}
	if _, err := d.sql.Exec(`UPDATE memories SET expired_at=?, invalid_at=COALESCE(invalid_at,?)
WHERE id=?`, nowUTC(), nowUTC(), id); err != nil {
		return err
	}
	return d.audit(id, "forget", text, "", actor)
}

// Supersede replaces a fact: the old row is marked stale (never deleted)
// and points at the new one; provenance of the old row is preserved and the
// pair is linked for lineage traversal.
func (d *DB) Supersede(oldID, newText, actor string, opts RememberOpts) (newID string, err error) {
	var oldText string
	err = d.sql.QueryRow(`SELECT text FROM memories WHERE id=? AND expired_at IS NULL AND superseded_by IS NULL`, oldID).
		Scan(&oldText)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("no active memory %s", oldID)
	}
	if err != nil {
		return "", err
	}
	newID, _, err = d.Remember(newText, actor, opts)
	if err != nil {
		return "", err
	}
	if newID == oldID {
		// Identical text: Remember folded the assertion into the very row
		// we were about to retire. Marking it superseded BY ITSELF would
		// expire the fact and point it at itself — the fact just vanishes.
		// It is a reassertion, which Remember has already recorded.
		return newID, nil
	}
	now := nowUTC()
	if _, err := d.sql.Exec(`UPDATE memories SET superseded_by=?, expired_at=?, invalid_at=COALESCE(invalid_at,?)
WHERE id=?`, newID, now, now, oldID); err != nil {
		return "", err
	}
	if _, err := d.sql.Exec(`INSERT INTO memory_links(from_id, to_id, rel) VALUES(?,?,'supersedes')
ON CONFLICT(from_id, to_id, rel) DO NOTHING`, newID, oldID); err != nil {
		return "", err
	}
	if err := d.audit(oldID, "supersede", oldText, newText, actor); err != nil {
		return "", err
	}
	return newID, nil
}

// Pin marks a memory as protected from Forget (and signals importance).
func (d *DB) Pin(id string, pinned bool, actor string) error {
	res, err := d.sql.Exec(`UPDATE memories SET pinned=? WHERE id=? AND expired_at IS NULL`,
		boolInt(pinned), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no active memory %s", id)
	}
	op := "pin"
	if !pinned {
		op = "unpin"
	}
	return d.audit(id, op, "", "", actor)
}

// Memories lists memories; active only unless includeStale.
func (d *DB) Memories(includeStale bool) ([]Memory, error) {
	where := "WHERE m.expired_at IS NULL AND m.superseded_by IS NULL"
	if includeStale {
		where = ""
	}
	return d.queryMemories(fmt.Sprintf(`SELECT %s FROM memories m %s ORDER BY m.pinned DESC, m.id DESC`,
		memCols, where))
}

// RecallOpts filters recall.
type RecallOpts struct {
	TokenBudget int
	Tag         string // restrict to memories carrying this tag
	Kind        string // restrict to one kind

	// Semantic leg (optional): when QueryVec is set, recall is hybrid —
	// BM25 and cosine-over-embeddings merged by reciprocal-rank fusion.
	// Callers embed the query themselves (the store makes no network calls).
	QueryVec   []float32
	EmbedModel string // embeddings to score against (rows for other models are ignored)
}

// EmbedTarget is a memory awaiting an embedding.
type EmbedTarget struct {
	ID   string
	Text string
}

// NeedingEmbedding lists live memories with no embedding for model.
func (d *DB) NeedingEmbedding(model string, limit int) ([]EmbedTarget, error) {
	rows, err := d.sql.Query(`SELECT m.id, m.text FROM memories m
LEFT JOIN memory_embeddings e ON e.memory_id = m.id AND e.model = ?
WHERE m.expired_at IS NULL AND m.superseded_by IS NULL AND e.memory_id IS NULL
ORDER BY m.id LIMIT ?`, model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EmbedTarget
	for rows.Next() {
		var t EmbedTarget
		if err := rows.Scan(&t.ID, &t.Text); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetEmbedding stores (or replaces) one memory's vector for a model.
func (d *DB) SetEmbedding(memoryID, model string, dim int, vec []byte) error {
	_, err := d.sql.Exec(`INSERT INTO memory_embeddings(memory_id, model, dim, vec)
VALUES(?,?,?,?) ON CONFLICT(memory_id, model) DO UPDATE SET dim=excluded.dim, vec=excluded.vec`,
		memoryID, model, dim, vec)
	return err
}

// Recall searches active memories and returns results trimmed to a token
// budget (~4 chars/token heuristic). Query terms are OR-combined and
// BM25-ranked (a knowledge query rarely matches every word of a stored
// fact); pinned facts surface first, then rank; confidence and
// corroboration ride along for the consumer. Every hit carries provenance.
func (d *DB) Recall(query string, opts RecallOpts) ([]Memory, error) {
	budget := opts.TokenBudget
	if budget <= 0 {
		budget = 1000
	}
	q := `SELECT ` + memCols + ` FROM memories m
JOIN (SELECT rowid, rank FROM memories_fts WHERE memories_fts MATCH ? ORDER BY rank LIMIT 100) f
  ON f.rowid = m.rowid
WHERE m.expired_at IS NULL AND m.superseded_by IS NULL`
	args := []any{ftsQuoteAny(query)}
	if opts.Tag != "" {
		q += ` AND m.id IN (SELECT memory_id FROM memory_tags WHERE tag=?)`
		args = append(args, normTag(opts.Tag))
	}
	if opts.Kind != "" {
		q += ` AND m.kind=?`
		args = append(args, opts.Kind)
	}
	q += ` ORDER BY m.pinned DESC, f.rank`
	hits, err := d.queryMemories(q, args...)
	if err != nil {
		return nil, err
	}
	if len(opts.QueryVec) > 0 {
		if hits, err = d.fuseVectorLeg(hits, opts); err != nil {
			return nil, err
		}
	}
	charBudget := budget * 4
	used := 0
	var out []Memory
	for _, m := range hits {
		cost := len(m.Text) + 64
		if used+cost > charBudget && len(out) > 0 {
			break
		}
		used += cost
		out = append(out, m)
	}
	return out, nil
}

// Embeddings returns id -> vector for every live memory embedded under
// model (retroactive dedup sweeps compare them pairwise).
func (d *DB) Embeddings(model string) (map[string][]float32, error) {
	rows, err := d.sql.Query(`SELECT m.id, e.vec FROM memory_embeddings e
JOIN memories m ON m.id = e.memory_id
WHERE e.model = ? AND m.expired_at IS NULL AND m.superseded_by IS NULL`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]float32{}
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		out[id] = embed.Decode(blob)
	}
	return out, rows.Err()
}

// Nearest returns the live memory most similar to vec among embeddings
// for model (brute force — exact, fine at this store's scale). sim is 0
// when the store has no embeddings for that model.
func (d *DB) Nearest(model string, vec []float32) (id, text string, sim float64, err error) {
	rows, err := d.sql.Query(`SELECT m.id, m.text, e.vec FROM memory_embeddings e
JOIN memories m ON m.id = e.memory_id
WHERE e.model = ? AND m.expired_at IS NULL AND m.superseded_by IS NULL`, model)
	if err != nil {
		return "", "", 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var mid, mtext string
		var blob []byte
		if err := rows.Scan(&mid, &mtext, &blob); err != nil {
			return "", "", 0, err
		}
		if s := embed.Cosine(vec, embed.Decode(blob)); s > sim {
			id, text, sim = mid, mtext, s
		}
	}
	return id, text, sim, rows.Err()
}

// fuseVectorLeg reranks recall as reciprocal-rank fusion of the BM25 list
// with a cosine-over-embeddings list (brute force — exact, and fast at this
// store's scale). Pinned memories still outrank everything.
func (d *DB) fuseVectorLeg(ftsHits []Memory, opts RecallOpts) ([]Memory, error) {
	vq := `SELECT m.id, e.vec FROM memory_embeddings e
JOIN memories m ON m.id = e.memory_id
WHERE e.model = ? AND m.expired_at IS NULL AND m.superseded_by IS NULL`
	args := []any{opts.EmbedModel}
	if opts.Tag != "" {
		vq += ` AND m.id IN (SELECT memory_id FROM memory_tags WHERE tag=?)`
		args = append(args, normTag(opts.Tag))
	}
	if opts.Kind != "" {
		vq += ` AND m.kind=?`
		args = append(args, opts.Kind)
	}
	rows, err := d.sql.Query(vq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scored struct {
		id  string
		sim float64
	}
	var cands []scored
	for rows.Next() {
		var id string
		var blob []byte
		if err := rows.Scan(&id, &blob); err != nil {
			return nil, err
		}
		if sim := embed.Cosine(opts.QueryVec, embed.Decode(blob)); sim > 0 {
			cands = append(cands, scored{id, sim})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].sim > cands[j].sim })
	if len(cands) > 50 {
		cands = cands[:50]
	}
	// RRF (k=60) over the two ranked lists.
	const k = 60
	score := map[string]float64{}
	for i, m := range ftsHits {
		score[m.ID] += 1 / float64(k+i+1)
	}
	for i, c := range cands {
		score[c.id] += 1 / float64(k+i+1)
	}
	byID := map[string]Memory{}
	for _, m := range ftsHits {
		byID[m.ID] = m
	}
	var missing []string
	for _, c := range cands {
		if _, ok := byID[c.id]; !ok {
			missing = append(missing, c.id)
		}
	}
	if len(missing) > 0 {
		q := `SELECT ` + memCols + ` FROM memories m WHERE m.id IN (?` +
			strings.Repeat(",?", len(missing)-1) + `)`
		vecOnly, err := d.queryMemories(q, toAny(missing)...)
		if err != nil {
			return nil, err
		}
		for _, m := range vecOnly {
			byID[m.ID] = m
		}
	}
	merged := make([]Memory, 0, len(byID))
	for _, m := range byID {
		merged = append(merged, m)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Pinned != merged[j].Pinned {
			return merged[i].Pinned
		}
		if score[merged[i].ID] != score[merged[j].ID] {
			return score[merged[i].ID] > score[merged[j].ID]
		}
		return merged[i].ID > merged[j].ID
	})
	return merged, nil
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// ftsQuoteAny quotes each token literally and ORs them: recall is ranked
// retrieval, not exact filtering (journal Search stays AND — see ftsQuote).
func ftsQuoteAny(q string) string {
	fields := strings.Fields(q)
	for i, f := range fields {
		fields[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	return strings.Join(fields, " OR ")
}

const memCols = `m.id, m.text, m.kind, m.confidence, m.created_at, COALESCE(m.expired_at,''),
COALESCE(m.valid_at,''), COALESCE(m.invalid_at,''), COALESCE(m.superseded_by,''), m.pinned,
COALESCE(m.actor,''),
(SELECT COUNT(*) FROM memory_sources s WHERE s.memory_id=m.id),
COALESCE((SELECT json_group_array(s.event_id) FROM memory_sources s WHERE s.memory_id=m.id),'[]'),
COALESCE((SELECT json_group_array(t.tag) FROM (SELECT tag FROM memory_tags
  WHERE memory_id=m.id ORDER BY rowid) t),'[]'),
COALESCE((SELECT json_group_array(l.rel || ':' || l.to_id) FROM memory_links l WHERE l.from_id=m.id),'[]')`

func (d *DB) queryMemories(q string, args ...any) ([]Memory, error) {
	rows, err := d.sql.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		var pinned int
		var srcJSON, tagJSON, linkJSON string
		if err := rows.Scan(&m.ID, &m.Text, &m.Kind, &m.Confidence, &m.CreatedAt, &m.ExpiredAt,
			&m.ValidAt, &m.InvalidAt, &m.SupersededBy, &pinned, &m.Actor,
			&m.Corroboration, &srcJSON, &tagJSON, &linkJSON); err != nil {
			return nil, err
		}
		m.Pinned = pinned != 0
		json.Unmarshal([]byte(srcJSON), &m.Sources)
		json.Unmarshal([]byte(tagJSON), &m.Tags)
		json.Unmarshal([]byte(linkJSON), &m.Links)
		out = append(out, m)
	}
	return out, rows.Err()
}

// DumpMemories writes memories and their structure as JSONL for
// cross-machine sync. Merge semantics live in ImportMemory.
func (d *DB) DumpMemories(enc *json.Encoder) error {
	mems, err := d.Memories(true)
	if err != nil {
		return err
	}
	for _, m := range mems {
		if err := enc.Encode(map[string]any{
			"project_id": d.projectID, "memory": m,
		}); err != nil {
			return err
		}
	}
	return nil
}

// ImportMemory merges one synced memory: insert if unknown (preserving its
// id and timestamps), and adopt remote supersession/expiry when the local
// row is still active — staleness must win across machines or forgotten
// facts resurrect on every sync. Confidence takes the max; tags, links, and
// sources union.
func (d *DB) ImportMemory(m *Memory) error {
	text, _ := redact.String(m.Text, 8192)
	_, err := d.sql.Exec(`INSERT INTO memories(id, text, kind, confidence, created_at, expired_at,
valid_at, invalid_at, superseded_by, pinned, actor)
VALUES(?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
  expired_at    = COALESCE(memories.expired_at, excluded.expired_at),
  invalid_at    = COALESCE(memories.invalid_at, excluded.invalid_at),
  superseded_by = COALESCE(memories.superseded_by, excluded.superseded_by),
  confidence    = MAX(memories.confidence, excluded.confidence),
  pinned        = MAX(memories.pinned, excluded.pinned)`,
		m.ID, text, validKind(m.Kind), m.Confidence, m.CreatedAt, nullable(m.ExpiredAt),
		nullable(m.ValidAt), nullable(m.InvalidAt), nullable(m.SupersededBy),
		boolInt(m.Pinned), m.Actor)
	if err != nil {
		return err
	}
	if err := d.addSources(m.ID, m.Sources); err != nil {
		return err
	}
	// Import is replication of the SAME memory id, not a twin merge: the
	// source machine's chapter filings were already made through an
	// authorized path, so adopt them all (the cap still applies). The
	// merge-path rule (first filing only) is for folding a DIFFERENT
	// fact's tags in; applying it here made multi-chapter filings
	// silently diverge between machines.
	if err := d.addTagsOpt(m.ID, m.Tags, true); err != nil {
		return err
	}
	for _, l := range m.Links {
		if rel, to, ok := strings.Cut(l, ":"); ok {
			if _, err := d.sql.Exec(`INSERT INTO memory_links(from_id, to_id, rel) VALUES(?,?,?)
ON CONFLICT(from_id, to_id, rel) DO NOTHING`, m.ID, to, rel); err != nil {
				return err
			}
		}
	}
	return nil
}
