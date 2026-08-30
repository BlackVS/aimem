// Chapter-proposal pass (DESIGN-unfiled-remediation.md Part B): give a
// group's stranded unfiled facts a path into chapters — including brand
// new ones — under human control. The model only ever PROPOSES a plan;
// nothing is written until a person approves it in /admin.
package curate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aimem/internal/store"
)

// ChapterAssign files one fact into an existing chapter.
type ChapterAssign struct {
	FactID  string `json:"fact_id"`
	Chapter string `json:"chapter"`
}

// NewChapter clusters facts into a proposed new chapter.
type NewChapter struct {
	Name    string   `json:"name"`
	About   string   `json:"about"`
	FactIDs []string `json:"fact_ids"`
}

// ChapterPlan is the model's proposal — reviewed, edited, and applied
// (or rejected) by a human.
type ChapterPlan struct {
	Assign      []ChapterAssign `json:"assign"`
	NewChapters []NewChapter    `json:"new_chapters"`
	Unfiled     int             `json:"unfiled"`         // candidate-pool size at proposal time
	Considered  int             `json:"considered"`      // facts actually shown to the model
	Revisit     bool            `json:"revisit"`         // pass ran over FILED facts, add-only
	TS          string          `json:"ts,omitempty"`    // proposal generation time
	Model       string          `json:"model,omitempty"` // attribution
	Usage       Usage           `json:"usage"`
}

const refilePrompt = `You are the knowledge librarian for the shared knowledge base %q.
Its charter: %s

Existing chapters:
%s
Below are UNFILED facts, one per line as "<id>\t<text>".
Assign each fact to an existing chapter where one clearly fits. Cluster
the remainder into a SMALL set of proposed new chapters (prefer few,
broad chapters over many narrow ones). A fact that genuinely spans two
topics may be listed under at most 2 chapters, but prefer a single home.
Each new chapter needs a short lowercase-slug name (letters/digits/
dashes, max 56 chars, never "unfiled") and a one-sentence "about". A fact that fits nowhere may be
left out entirely.

Reply with ONLY a JSON object (no prose, no markdown fences):
{"assign":[{"fact_id":"...","chapter":"<existing name>"}],
 "new_chapters":[{"name":"...","about":"...","fact_ids":["..."]}]}

Facts:
%s`

// revisitPrompt is the RE-LABEL variant (user request, 2026-08-30): a
// chapter set evolves as the KB grows, and facts filed under the early
// chapters may belong in the newer ones too. Deliberately ADD-only —
// moving or unfiling a fact stays a human act in the console, so an
// over-eager model can only ever add a reversible extra label.
const revisitPrompt = `You are the knowledge librarian for the shared knowledge base %q.
Its charter: %s

Current chapters:
%s
Below are ALREADY-FILED facts, one per line as "<id>\t[current chapters]\t<text>".
The chapter set has evolved since these were filed. Propose ADDITIONAL
chapters from the current list for facts that clearly also belong there
(at most 3 chapters per fact including its current ones). Do NOT
propose removing or changing existing filings. Only add where the fit
is clear — most facts should get nothing. If a distinct cluster has no
fitting chapter, you may propose a new chapter for it (short
lowercase-slug name, one-sentence "about"), but prefer the existing set.

Reply with ONLY a JSON object (no prose, no markdown fences):
{"assign":[{"fact_id":"...","chapter":"<existing name>"}],
 "new_chapters":[{"name":"...","about":"...","fact_ids":["..."]}]}

Facts:
%s`

// RefileCandidates lists live facts that ARE filed but still have room
// under the cap — the revisit pool.
func RefileCandidates(db *store.DB) ([]store.Memory, error) {
	mems, err := db.Memories(false)
	if err != nil {
		return nil, err
	}
	var out []store.Memory
	for _, m := range mems {
		n := 0
		for _, t := range m.Tags {
			if strings.HasPrefix(t, "chapter:") {
				n++
			}
		}
		if n > 0 && n < store.MaxChaptersPerFact {
			out = append(out, m)
		}
	}
	return out, nil
}

// UnfiledFacts lists a group's live facts carrying no chapter tag.
func UnfiledFacts(db *store.DB) ([]store.Memory, error) {
	mems, err := db.Memories(false)
	if err != nil {
		return nil, err
	}
	var out []store.Memory
	for _, m := range mems {
		filed := false
		for _, t := range m.Tags {
			if strings.HasPrefix(t, "chapter:") {
				filed = true
				break
			}
		}
		if !filed {
			out = append(out, m)
		}
	}
	return out, nil
}

// ProposeChapters runs the proposal pass: over the group's unfiled
// bucket by default, or (revisit) over already-filed facts with room
// under the cap, proposing additional labels from the evolved chapter
// set. maxFacts caps how many facts one call considers (token bound);
// 0 means the default of 80.
func ProposeChapters(db *store.DB, group, about string, chapters []Chapter, syn Synthesizer, maxFacts int, revisit bool) (*ChapterPlan, error) {
	if maxFacts <= 0 {
		maxFacts = 80
	}
	pool, err := UnfiledFacts(db)
	if revisit {
		pool, err = RefileCandidates(db)
	}
	if err != nil {
		return nil, err
	}
	plan := &ChapterPlan{Unfiled: len(pool), Revisit: revisit, TS: time.Now().UTC().Format(time.RFC3339)}
	if len(pool) == 0 {
		return plan, nil
	}
	batch := pool
	if len(batch) > maxFacts {
		batch = batch[:maxFacts]
	}
	plan.Considered = len(batch)
	chapterList := "(none declared yet)\n"
	if len(chapters) > 0 {
		var b strings.Builder
		for _, c := range chapters {
			fmt.Fprintf(&b, "- %s: %s\n", c.Name, c.About)
		}
		chapterList = b.String()
	}
	var facts strings.Builder
	for _, m := range batch {
		text := strings.ReplaceAll(m.Text, "\n", " ")
		if len(text) > 400 {
			text = text[:400]
		}
		if revisit {
			var cur []string
			for _, t := range m.Tags {
				if c, ok := strings.CutPrefix(t, "chapter:"); ok {
					cur = append(cur, c)
				}
			}
			fmt.Fprintf(&facts, "%s\t[%s]\t%s\n", m.ID, strings.Join(cur, ","), text)
		} else {
			fmt.Fprintf(&facts, "%s\t%s\n", m.ID, text)
		}
	}
	if about == "" {
		about = "(no charter recorded)"
	}
	prompt := refilePrompt
	if revisit {
		prompt = revisitPrompt
	}
	reply, usage, err := syn.Complete(fmt.Sprintf(prompt, group, about, chapterList, facts.String()))
	plan.Usage = usage
	if err != nil {
		return nil, err
	}
	// Tolerant extraction, object-shaped (parseProposals' array trick
	// doesn't fit here — the plan is a JSON object).
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end <= start {
		return nil, fmt.Errorf("no JSON object in proposal reply: %s", clip(reply, 200))
	}
	if err := json.Unmarshal([]byte(reply[start:end+1]), plan); err != nil {
		return nil, fmt.Errorf("bad proposal JSON: %w", err)
	}
	// Only known fact ids may appear (a hallucinated id must not survive
	// into the reviewed plan).
	known := map[string]bool{}
	for _, m := range batch {
		known[m.ID] = true
	}
	assign := plan.Assign[:0]
	for _, a := range plan.Assign {
		if known[a.FactID] {
			assign = append(assign, a)
		}
	}
	plan.Assign = assign
	for i := range plan.NewChapters {
		ids := plan.NewChapters[i].FactIDs[:0]
		for _, id := range plan.NewChapters[i].FactIDs {
			if known[id] {
				ids = append(ids, id)
			}
		}
		plan.NewChapters[i].FactIDs = ids
	}
	return plan, nil
}
