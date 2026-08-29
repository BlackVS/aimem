// Package curate is the asynchronous knowledge curator (proposal Phase 5b,
// Letta's "sleep-time agent" pattern): it reads recent journal events and
// distills durable facts into the curated knowledge store. It NEVER runs in
// the checkpoint path, and it cannot destroy anything — proposals land
// through the same audited Remember path as human facts, marked with
// actor "curator". Egress policy: the default backend is headless
// `claude -p` (subscription-covered); projects that must not egress simply
// never run curation.
package curate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"aimem/internal/embed"
	"aimem/internal/ident"
	"aimem/internal/store"
)

// Proposal is one fact the extractor wants to remember.
type Proposal struct {
	Text           string   `json:"text"`
	Kind           string   `json:"kind"`
	Scope          string   `json:"scope"`             // "project" | "user"
	Chapter        string   `json:"chapter,omitempty"` // knowledge-base chapter, when a group declares chapters
	Tags           []string `json:"tags"`
	SourceEventIDs []string `json:"source_event_ids"`
}

// Usage is what one extraction call cost — the meter for knowledge-base
// maintenance. LiteLLM's per-key spend accounting is the authoritative
// ledger; this makes each run's cost visible in its own report/logs.
type Usage struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd,omitempty"` // claude backend only
}

func (u *Usage) add(o Usage) {
	u.InputTokens += o.InputTokens
	u.OutputTokens += o.OutputTokens
	u.CostUSD += o.CostUSD
}

// Chapter is one section of a group's curated knowledge base: the
// description says which facts belong in it, so the extractor can file
// each fact where readers will look for it.
type Chapter struct {
	Name  string `json:"name"`
	About string `json:"about"`
}

// GroupHint describes one declared knowledge group to the extractor: the
// bare name plus the group's charter (its "about" meta) and knowledge-base
// chapters, when set. The charter and chapters are what let the extractor
// route facts by domain instead of guessing from a bare name.
type GroupHint struct {
	Name     string
	About    string
	Chapters []Chapter
}

// Extractor turns a batch of journal events into fact proposals. groups
// lists the shared knowledge groups the project has declared;
// proposals may target them via scope "group:<name>".
type Extractor interface {
	Extract(events []store.StoredEvent, maxFacts int, groups []GroupHint) ([]Proposal, Usage, error)
}

// Embedder produces embedding vectors (implemented by embed.Client).
// The curator uses it for semantic dedup: a proposal whose vector is
// near-identical to an existing memory reasserts that memory instead of
// inserting a rephrased twin.
type Embedder interface {
	Embed(texts []string) ([][]float32, int64, error)
}

// RunOpts controls one curation run.
type RunOpts struct {
	DryRun    bool
	MaxFacts  int    // cap per run (default 10)
	MaxEvents int    // events consumed per run (default 50)
	Model     string // recorded in run history for per-model cost breakdown
	Force     bool   // bypass budget caps for a deliberate manual run

	// Semantic dedup (all three set => enabled): proposals at cosine >=
	// DedupSim to an existing memory reassert it (confidence boost +
	// tag/source merge, audited) rather than inserting a near-duplicate.
	// Fresh inserts store their vector immediately, so the embed backfill
	// has nothing left to do for curator facts.
	Embedder   Embedder
	EmbedModel string
	DedupSim   float64
}

// Report summarizes a run.
type Report struct {
	EventsRead  int            `json:"events_read"`
	Proposals   []Proposal     `json:"proposals"`
	Written     int            `json:"written"`
	Reasserted  int            `json:"reasserted"`
	Skipped     int            `json:"skipped"`
	Mirrored    int            `json:"mirrored,omitempty"`   // project facts copied into policy-all groups
	Deduped     int            `json:"deduped,omitempty"`    // divergent proposals folded onto a PINNED existing memory
	Superseded  int            `json:"superseded,omitempty"` // near-identical existing facts replaced by an updated wording
	Conflicts   []ConflictPair `json:"conflicts,omitempty"`
	EmbedTokens int64          `json:"embed_tokens,omitempty"` // dedup embedding spend (embed model)
	DryRun      bool           `json:"dry_run"`
	Usage       Usage          `json:"usage"`
	NewCursor   string         `json:"new_cursor,omitempty"`
	// The consumed journal window, recorded so a zero-yield run (events
	// consumed, nothing landed — possibly a guardrail's clean []) stays
	// auditable and re-curable.
	FirstEvent string `json:"first_event,omitempty"`
	LastEvent  string `json:"last_event,omitempty"`
}

// ConflictPair records a high-similarity divergence: an incoming fact
// that landed on top of an existing one. Visible in the run report so a
// silent rewrite can never happen unnoticed.
type ConflictPair struct {
	OldID   string  `json:"old_id"`
	OldText string  `json:"old_text"`
	NewText string  `json:"new_text"`
	Sim     float64 `json:"sim"`
	Action  string  `json:"action"` // "superseded" | "kept-pinned"
}

// ZeroYield reports whether the run consumed events but landed nothing —
// indistinguishable from a legitimate "nothing qualifies" batch, which is
// exactly why it must be VISIBLE rather than auto-retried
// (DESIGN-unfiled-remediation.md Part A).
func (r *Report) ZeroYield() bool {
	return r.EventsRead > 0 && r.Written == 0 && r.Reasserted == 0 && r.Skipped == 0
}

func cursorPath(root, projectID string) string {
	return filepath.Join(root, "curate", projectID+".cursor")
}

// Run performs one curation pass over a project's journal. The cursor only
// advances on a non-dry run, so dry runs are repeatable previews.
func Run(reg *store.Registry, root, projectID string, ex Extractor, opts RunOpts) (*Report, error) {
	if opts.MaxFacts <= 0 {
		opts.MaxFacts = 10
	}
	if opts.MaxEvents <= 0 {
		opts.MaxEvents = 50
	}
	db, err := reg.Open(projectID)
	if err != nil {
		return nil, err
	}
	cursor := ""
	if b, err := os.ReadFile(cursorPath(root, projectID)); err == nil {
		cursor = strings.TrimSpace(string(b))
	}
	events, err := db.EventsSince(cursor, opts.MaxEvents)
	if err != nil {
		return nil, err
	}
	rep := &Report{EventsRead: len(events), DryRun: opts.DryRun}
	if len(events) == 0 {
		return rep, nil
	}
	// Budget gate: refuse BEFORE spending when the window's usage plus a
	// worst-case projection of this run would cross a cap.
	if !opts.DryRun && !opts.Force {
		budget, projectOwned, err := LoadBudget(reg, db)
		if err != nil {
			return nil, err
		}
		if !budget.Empty() {
			if err := CheckBudget(reg, db, budget, !projectOwned,
				ProjectRun(opts.MaxEvents, opts.MaxFacts), time.Now()); err != nil {
				return nil, err
			}
		}
	}
	// Declared knowledge groups, stamped into project meta by event pushes
	// (.aimem.json -> adapter -> append). Backing ids like "group-x";
	// the prompt gets bare names.
	var groupIDs []string
	if raw, err := db.GetMeta("groups"); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &groupIDs)
	}
	// Each group's own DB carries its config (synced everywhere like any
	// project): "about" is the charter handed to the extractor for domain
	// routing; policy "all" mirrors every curated project fact into the
	// group — the meta-project case, where members share one framework
	// and everything important is group knowledge.
	hints := make([]GroupHint, 0, len(groupIDs))
	type mirrorDB struct {
		db       *store.DB
		chapters []Chapter
	}
	var mirrors []mirrorDB
	chaptersByName := map[string][]Chapter{} // bare group name -> chapters
	for _, id := range groupIDs {
		h := GroupHint{Name: strings.TrimPrefix(id, "group-")}
		if gdb, gerr := reg.Open(id); gerr == nil {
			h.About, _ = gdb.GetMeta("about")
			if raw, _ := gdb.GetMeta("chapters"); raw != "" {
				_ = json.Unmarshal([]byte(raw), &h.Chapters)
			}
			chaptersByName[h.Name] = h.Chapters
			if pol, _ := gdb.GetMeta("policy"); pol == "all" {
				mirrors = append(mirrors, mirrorDB{db: gdb, chapters: h.Chapters})
			}
		}
		hints = append(hints, h)
	}
	// chapterTag returns the fact's chapter as a tag when the target group
	// actually declares that chapter (junk chapter names never stick).
	chapterTag := func(chapters []Chapter, want string) []string {
		for _, c := range chapters {
			if want != "" && c.Name == want {
				return []string{"chapter:" + want}
			}
		}
		return nil
	}
	proposals, usage, err := ex.Extract(events, opts.MaxFacts, hints)
	rep.Usage.add(usage) // partial usage still counts on error paths
	if err != nil {
		return nil, err
	}
	if len(proposals) > opts.MaxFacts {
		proposals = proposals[:opts.MaxFacts]
	}
	rep.Proposals = proposals
	rep.FirstEvent, rep.LastEvent = events[0].ID, events[len(events)-1].ID
	newCursor := events[len(events)-1].ID
	if opts.DryRun {
		return rep, nil
	}
	// Batch-embed proposal texts for semantic dedup (best-effort: an
	// embedding outage must not block curation, it just skips dedup).
	var vecs [][]float32
	if opts.Embedder != nil && opts.EmbedModel != "" && opts.DedupSim > 0 && len(proposals) > 0 {
		texts := make([]string, len(proposals))
		for i, p := range proposals {
			texts[i] = p.Text
		}
		v, used, verr := opts.Embedder.Embed(texts)
		rep.EmbedTokens += used
		if verr == nil && len(v) == len(proposals) {
			vecs = v
		}
	}
	// remember writes one fact with dedup: at DedupSim or above to an
	// existing memory, reassert THAT memory (confidence boost, tag/source
	// merge — including retroactive chapter filing) instead of inserting
	// a rephrased twin; fresh inserts store their vector right away.
	remember := func(target *store.DB, i int, text, kind string, tags, sources []string) (reasserted bool, err error) {
		if vecs != nil {
			nid, ntext, sim, nerr := target.Nearest(opts.EmbedModel, vecs[i])
			// Same neighborhood, DIFFERENT words: either a rephrasing or an
			// update of the same fact — cosine cannot tell them apart. The
			// retroactive sweep already resolves this newest-wins
			// (DedupProject's survivor order), so write time must agree:
			// keeping the older text here silently discarded updates and
			// reinforced stale facts. Supersession is bi-temporal, linked
			// and audited, so the old wording stays recoverable.
			if nerr == nil && sim >= opts.DedupSim && ntext != "" && ntext != text {
				pair := ConflictPair{OldID: nid, OldText: ntext, NewText: text, Sim: sim}
				// A pinned fact is one a human deliberately protected: never
				// rewrite it automatically — keep it and report the clash.
				if target.IsPinned(nid) {
					pair.Action = "kept-pinned"
					rep.Conflicts = append(rep.Conflicts, pair)
					rep.Deduped++
					// Nothing is written in this branch, so the audit trail
					// is the only durable record of the clash.
					if aerr := target.NoteConflict(nid, ntext, text, "curator"); aerr != nil {
						fmt.Fprintf(os.Stderr, "curate: conflict audit failed: %v\n", aerr)
					}
					text = ntext
				} else if newID, serr := target.Supersede(nid, text, "curator", store.RememberOpts{
					Kind: kind, Tags: tags, Sources: sources,
				}); serr == nil {
					pair.Action = "superseded"
					rep.Conflicts = append(rep.Conflicts, pair)
					rep.Superseded++
					_ = target.SetEmbedding(newID, opts.EmbedModel, len(vecs[i]), embed.Encode(vecs[i]))
					return false, nil
				}
				// Supersede failed: fall through to a plain write rather
				// than losing the proposal.
			}
		}
		id, reasserted, err := target.Remember(text, "curator", store.RememberOpts{
			Kind: kind, Tags: tags, Sources: sources,
		})
		if err == nil && !reasserted && vecs != nil {
			_ = target.SetEmbedding(id, opts.EmbedModel, len(vecs[i]), embed.Encode(vecs[i]))
		}
		return reasserted, err
	}
	for pi, p := range proposals {
		if strings.TrimSpace(p.Text) == "" {
			rep.Skipped++
			continue
		}
		target := db
		sources := p.SourceEventIDs
		tags := p.Tags
		switch {
		case p.Scope == "user":
			if target, err = reg.Open(store.UserScopeProject); err != nil {
				rep.Skipped++
				continue
			}
		case strings.HasPrefix(p.Scope, "group:"):
			// Membership is the sharing gate: only groups this project
			// declared (present in its meta) may receive its facts.
			gname := strings.TrimPrefix(p.Scope, "group:")
			gid, gerr := ident.GroupProject(gname)
			if gerr != nil || !slices.Contains(groupIDs, gid) {
				rep.Skipped++
				continue
			}
			if target, err = reg.Open(gid); err != nil {
				rep.Skipped++
				continue
			}
			// Group facts carry their origin project as provenance and
			// file into the knowledge base chapter the extractor picked.
			sources = append(append([]string{}, sources...), "project:"+projectID)
			tags = append(append([]string{}, tags...), chapterTag(chaptersByName[gname], p.Chapter)...)
		}
		reasserted, err := remember(target, pi, p.Text, p.Kind, tags, sources)
		switch {
		case err != nil:
			rep.Skipped++
		case reasserted:
			rep.Reasserted++
		default:
			rep.Written++
		}
		// Policy-all groups receive a copy of every project-scoped fact
		// (idempotent: near-identical content reasserts, not duplicates),
		// filed into the chapter the extractor picked when the group has it.
		if err == nil && target == db {
			for _, m := range mirrors {
				gsources := append(append([]string{}, sources...), "project:"+projectID)
				gtags := append(append([]string{}, p.Tags...), chapterTag(m.chapters, p.Chapter)...)
				if re, merr := remember(m.db, pi, p.Text, p.Kind, gtags, gsources); merr == nil && !re {
					rep.Mirrored++
				}
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(cursorPath(root, projectID)), 0o700); err != nil {
		return rep, err
	}
	if err := os.WriteFile(cursorPath(root, projectID), []byte(newCursor+"\n"), 0o600); err != nil {
		return rep, err
	}
	rep.NewCursor = newCursor
	// Record the run in history (best-effort): the token/cost meter that
	// dashboards aggregate per day/week/month and per group.
	host, _ := os.Hostname()
	_ = db.AddCurateRun(&store.CurateRun{
		TS: time.Now().UTC().Format(time.RFC3339), Host: host, Model: opts.Model,
		EventsRead: rep.EventsRead, Written: rep.Written,
		Reasserted: rep.Reasserted, Skipped: rep.Skipped,
		InputTokens: rep.Usage.InputTokens, OutputTokens: rep.Usage.OutputTokens,
		CostUSD: rep.Usage.CostUSD, FirstEvent: rep.FirstEvent, LastEvent: rep.LastEvent,
	})
	// Dedup embedding spend is metered under the embed model, matching
	// how the embed backfill records its usage.
	if rep.EmbedTokens > 0 {
		_ = db.AddCurateRun(&store.CurateRun{
			TS: time.Now().UTC().Format(time.RFC3339), Host: host,
			Model: opts.EmbedModel, InputTokens: rep.EmbedTokens,
		})
	}
	return rep, nil
}

// ClaudeExtractor runs headless Claude Code as the extraction backend —
// covered by the user's subscription, no per-token API billing.
type ClaudeExtractor struct {
	Model   string // e.g. "haiku" (default): cheap and sufficient for extraction
	WorkDir string // run outside any real project so curator turns don't pollute its journal
}

const promptHeader = `You are a knowledge curator for a software project. Below are recent
coding-session events (JSON). Extract ONLY durable knowledge worth
remembering for months: decisions made (and why), conventions adopted, user
preferences, solutions to recurring problems, important references.
Do NOT extract transient activity (ran tests, edited file X), restatements
of code, or anything a git log already answers.

Reply with ONLY a JSON array (no prose, no markdown fences):
[{"text":"one self-contained sentence","kind":"decision|convention|preference|solution|reference|fact","scope":"%s","tags":["topic",...],"source_event_ids":["<id of supporting event>",...]}]
Use scope "user" only for facts about the user or their machines, not this
project.%s At most %d facts; reply [] if nothing qualifies.

Events:
%s`

// DedupPair records one retroactive fold: the older twin's tags/sources
// merged onto the kept memory, then the twin retired (bi-temporal).
type DedupPair struct {
	KeptID      string  `json:"kept_id"`
	KeptText    string  `json:"kept_text"`
	DroppedID   string  `json:"dropped_id"`
	DroppedText string  `json:"dropped_text"`
	Sim         float64 `json:"sim"`
}

// DedupResult summarizes one project's retroactive dedup sweep.
type DedupResult struct {
	Examined int         `json:"examined"` // live memories with an embedding
	Folded   int         `json:"folded"`
	Pairs    []DedupPair `json:"pairs,omitempty"`
	DryRun   bool        `json:"dry_run,omitempty"`
}

// DedupProject folds near-identical live memories (cosine >= minSim over
// stored embeddings for model) onto a single survivor: pinned wins, then
// the newer phrasing. The loser's tags and sources merge onto the kept
// fact before the loser is retired — audited, never destructive. Cleans
// up twins that predate write-time dedup.
func DedupProject(db *store.DB, model string, minSim float64, dryRun bool) (*DedupResult, error) {
	mems, err := db.Memories(false)
	if err != nil {
		return nil, err
	}
	vecs, err := db.Embeddings(model)
	if err != nil {
		return nil, err
	}
	// Survivor preference order: pinned first, then newest.
	sort.SliceStable(mems, func(i, j int) bool {
		if mems[i].Pinned != mems[j].Pinned {
			return mems[i].Pinned
		}
		return mems[i].CreatedAt > mems[j].CreatedAt
	})
	res := &DedupResult{DryRun: dryRun}
	folded := map[string]bool{}
	for i := range mems {
		vi, ok := vecs[mems[i].ID]
		if !ok || folded[mems[i].ID] {
			continue
		}
		res.Examined++
		for j := i + 1; j < len(mems); j++ {
			vj, ok := vecs[mems[j].ID]
			if !ok || folded[mems[j].ID] || mems[j].Pinned {
				continue
			}
			sim := embed.Cosine(vi, vj)
			if sim < minSim {
				continue
			}
			if !dryRun {
				if _, _, err := db.Remember(mems[i].Text, "dedup", store.RememberOpts{
					Kind: mems[i].Kind, Tags: mems[j].Tags, Sources: mems[j].Sources,
				}); err != nil {
					return res, err
				}
				if err := db.Forget(mems[j].ID, "dedup"); err != nil {
					return res, err
				}
			}
			folded[mems[j].ID] = true
			res.Folded++
			res.Pairs = append(res.Pairs, DedupPair{
				KeptID: mems[i].ID, KeptText: mems[i].Text,
				DroppedID: mems[j].ID, DroppedText: mems[j].Text, Sim: sim,
			})
		}
	}
	return res, nil
}

// buildPrompt renders the shared extraction prompt for any backend.
func buildPrompt(events []store.StoredEvent, maxFacts int, groups []GroupHint) (string, error) {
	type slimEvent struct {
		ID    string   `json:"id"`
		User  string   `json:"user_request,omitempty"`
		Reply string   `json:"assistant_response,omitempty"`
		Tools []string `json:"tools,omitempty"`
		Kind  string   `json:"kind,omitempty"`
	}
	slim := make([]slimEvent, 0, len(events))
	for _, e := range events {
		slim = append(slim, slimEvent{
			ID: e.ID, User: clip(e.UserRequest, 600), Reply: clip(e.AssistantReply, 1200),
			Tools: e.ToolSummary, Kind: e.Kind,
		})
	}
	evJSON, err := json.Marshal(slim)
	if err != nil {
		return "", err
	}
	scopes, groupNote := "project|user", ""
	if len(groups) > 0 {
		scopes += "|group:<name>"
		names := make([]string, 0, len(groups))
		var charters []string
		haveChapters := false
		for _, g := range groups {
			names = append(names, g.Name)
			if g.About != "" {
				charters = append(charters, fmt.Sprintf("%q is about: %s", g.Name, g.About))
			}
			if len(g.Chapters) > 0 {
				haveChapters = true
				secs := make([]string, 0, len(g.Chapters))
				for _, c := range g.Chapters {
					secs = append(secs, fmt.Sprintf("%s (%s)", c.Name, c.About))
				}
				charters = append(charters, fmt.Sprintf("%q knowledge-base chapters: %s",
					g.Name, strings.Join(secs, "; ")))
			}
		}
		groupNote = fmt.Sprintf(" Scope \"group:<name>\" (allowed: %s) is for durable"+
			" knowledge that belongs to that group's shared domain — infrastructure,"+
			" conventions, tooling, framework-wide decisions other member projects"+
			" would benefit from — rather than this codebase's internals.",
			strings.Join(names, ", "))
		if len(charters) > 0 {
			groupNote += " " + strings.Join(charters, ". ") + "."
		} else {
			groupNote += " A group without a stated domain gets only facts that" +
				" hold for every member project; on any doubt use \"project\"."
		}
		if haveChapters {
			groupNote += " On every fact, also set \"chapter\" to the best-matching" +
				" knowledge-base chapter name (omit it when none fits)."
		}
	}
	return fmt.Sprintf(promptHeader, scopes, groupNote, maxFacts, evJSON), nil
}

func (c *ClaudeExtractor) Extract(events []store.StoredEvent, maxFacts int, groups []GroupHint) ([]Proposal, Usage, error) {
	prompt, err := buildPrompt(events, maxFacts, groups)
	if err != nil {
		return nil, Usage{}, err
	}
	content, u, err := c.Complete(prompt)
	if err != nil {
		return nil, u, err
	}
	props, err := parseProposals(content)
	return props, u, err
}

// Complete runs one headless claude turn — shared by extraction and by
// design-doc synthesis.
func (c *ClaudeExtractor) Complete(prompt string) (string, Usage, error) {
	var u Usage
	model := c.Model
	if model == "" {
		model = "haiku"
	}
	cmd := exec.Command("claude", "-p", prompt,
		"--model", model, "--output-format", "json")
	if c.WorkDir != "" {
		cmd.Dir = c.WorkDir
	}
	out, err := cmd.Output()
	if err != nil {
		return "", u, fmt.Errorf("claude -p failed: %w", err)
	}
	return parseClaudeResult(out)
}

// parseClaudeResult decodes the headless CLI's JSON wrapper. The
// top-level usage's input_tokens is only the UNCACHED slice of the
// final API call — in a `claude -p` run nearly the whole prompt rides
// prompt caching, so that number alone reads as ~40-80 tokens for a
// multi-thousand-token extraction and made cross-backend comparison
// meaningless (FEATURE-PROPOSALS #7). Real input = uncached +
// cache-creation + cache-read. Cache-read tokens are far cheaper per
// token, so token counts slightly OVERSTATE relative cost — the safe
// direction for the budget brake; total_cost_usd stays the money
// truth when the CLI reports one.
func parseClaudeResult(out []byte) (string, Usage, error) {
	var u Usage
	var wrapper struct {
		Result  string  `json:"result"`
		IsError bool    `json:"is_error"`
		CostUSD float64 `json:"total_cost_usd"`
		Usage   struct {
			InputTokens         int64 `json:"input_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(out, &wrapper); err != nil {
		return "", u, fmt.Errorf("unexpected claude output: %w", err)
	}
	u = Usage{
		InputTokens:  wrapper.Usage.InputTokens + wrapper.Usage.CacheCreationTokens + wrapper.Usage.CacheReadTokens,
		OutputTokens: wrapper.Usage.OutputTokens,
		CostUSD:      wrapper.CostUSD,
	}
	if wrapper.IsError {
		return "", u, fmt.Errorf("completion turn errored: %s", clip(wrapper.Result, 200))
	}
	return wrapper.Result, u, nil
}

// OpenAIExtractor calls an OpenAI-compatible chat-completions endpoint —
// typically the lab's LiteLLM proxy. This is Mem0's economics: extraction is
// a narrow task that small/fast models handle well, so the model is
// configurable and cheap. It is also the egress-policy backend: BaseURL is
// pointed at the project's approved proxy, nothing else.
type OpenAIExtractor struct {
	BaseURL string // e.g. https://api.openai.com/v1
	APIKey  string
	Model   string // e.g. a small routed model on the proxy
}

func (o *OpenAIExtractor) Extract(events []store.StoredEvent, maxFacts int, groups []GroupHint) ([]Proposal, Usage, error) {
	prompt, err := buildPrompt(events, maxFacts, groups)
	if err != nil {
		return nil, Usage{}, err
	}
	content, u, err := o.Complete(prompt)
	if err != nil {
		return nil, u, err
	}
	props, err := parseProposals(content)
	return props, u, err
}

// Complete runs one plain text completion — shared by extraction and by
// design-doc synthesis.
func (o *OpenAIExtractor) Complete(prompt string) (string, Usage, error) {
	var u Usage
	// No temperature field: GPT-5-family models reject anything but the
	// default, and extraction quality doesn't depend on it.
	body, _ := json.Marshal(map[string]any{
		"model":    o.Model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequest("POST", strings.TrimRight(o.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", u, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
	resp, err := (&http.Client{Timeout: 300 * time.Second}).Do(req)
	if err != nil {
		return "", u, err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", u, fmt.Errorf("bad completions response (HTTP %d): %w", resp.StatusCode, err)
	}
	u = Usage{InputTokens: out.Usage.PromptTokens, OutputTokens: out.Usage.CompletionTokens}
	// LiteLLM reports the computed request cost in a response header;
	// capture it so USD budgets meter real spend where available.
	if c, err := strconv.ParseFloat(resp.Header.Get("x-litellm-response-cost"), 64); err == nil && c > 0 {
		u.CostUSD = c
	}
	if out.Error != nil {
		return "", u, fmt.Errorf("completions error (HTTP %d): %s", resp.StatusCode, clip(out.Error.Message, 200))
	}
	if len(out.Choices) == 0 {
		return "", u, fmt.Errorf("completions returned no choices (HTTP %d)", resp.StatusCode)
	}
	return out.Choices[0].Message.Content, u, nil
}

// parseProposals tolerates markdown fences and surrounding prose around the
// JSON array — model output discipline is good but not guaranteed.
func parseProposals(s string) ([]Proposal, error) {
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON array in extractor reply: %s", clip(s, 200))
	}
	var out []Proposal
	if err := json.Unmarshal([]byte(s[start:end+1]), &out); err != nil {
		return nil, fmt.Errorf("bad extractor JSON: %w", err)
	}
	return out, nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
