// Design-doc synthesis: the top-down pass over a knowledge base.
// Curation distills events into facts (bottom-up); GenerateDoc turns the
// accumulated facts back into a coherent document — charter as scope,
// chapters as sections, facts as the source material, corroboration as
// the certainty signal. The result is stored in the KB's own DB meta
// ("design_doc" + "design_doc_ts"), so the GUI can render it and staleness
// is a timestamp comparison away.
package curate

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"aimem/internal/store"
)

// KnownFeatures are the optional per-group behaviors a KB can switch on
// (meta "features", a JSON array of names). "doc": nightly design-doc
// synthesis. Future optional features register here.
var KnownFeatures = []string{"doc"}

// FeatureEnabled reports whether a KB has opted into an optional feature.
func FeatureEnabled(db *store.DB, name string) bool {
	raw, _ := db.GetMeta("features")
	if raw == "" {
		return false
	}
	var fs []string
	if json.Unmarshal([]byte(raw), &fs) != nil {
		return false
	}
	return slices.Contains(fs, name)
}

// Synthesizer produces prose from a prompt (both extractor backends
// implement it via their Complete methods).
type Synthesizer interface {
	Complete(prompt string) (string, Usage, error)
}

// DocReport summarizes one document generation.
type DocReport struct {
	Project  string `json:"project"`
	Facts    int    `json:"facts"`
	Sections int    `json:"sections"`
	Chars    int    `json:"chars"`
	Skipped  bool   `json:"skipped,omitempty"` // up to date, nothing regenerated
	Usage    Usage  `json:"usage"`
}

const docSectionPrompt = `You are writing one section of the design document for %q.
Scope of the whole document: %s
Section %q covers: %s

Below are the curated facts for this section, one per line, each prefixed
with [n] (its citation number) and its corroboration count (x2 means two
independent sessions confirmed it; higher means more certain).

%s

Write the section as coherent Markdown prose (### subheadings allowed,
no top-level heading — the assembler adds it). Organize by theme, not by
fact order. Cite facts inline as [n]. When facts come from different
member repos ("from ..."), attribute repo-specific behavior to its repo
by name. State only what the facts support; where facts conflict, say
so. Do not invent content, do not pad.`

const docOverviewPrompt = `You are writing the opening overview of the design document for %q.
Scope: %s

The document's sections and their fact counts:
%s

A sample of the highest-corroboration facts across the whole knowledge base:
%s

Write 1-3 Markdown paragraphs introducing the system this document
describes: what it is, its major parts, and how the sections relate.
No headings. State only what the material supports.`

// GenerateDoc synthesizes the design doc for one knowledge-base DB (a
// group DB, or any project DB with chapters/about meta). force skips the
// staleness check; otherwise generation is skipped when the stored doc is
// newer than the newest live fact.
func GenerateDoc(db *store.DB, name string, syn Synthesizer, force bool) (*DocReport, error) {
	rep := &DocReport{Project: name}
	about, _ := db.GetMeta("about")
	var chapters []Chapter
	if raw, _ := db.GetMeta("chapters"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &chapters); err != nil {
			return nil, fmt.Errorf("chapters meta: %w", err)
		}
	}
	mems, err := db.Memories(false)
	if err != nil {
		return nil, err
	}
	rep.Facts = len(mems)
	if len(mems) == 0 {
		rep.Skipped = true
		return rep, nil
	}
	sort.Slice(mems, func(i, j int) bool { return mems[i].CreatedAt < mems[j].CreatedAt })
	if !force {
		ts, _ := db.GetMeta("design_doc_ts")
		if ts != "" && mems[len(mems)-1].CreatedAt <= ts {
			rep.Skipped = true
			return rep, nil
		}
	}
	// Bucket facts by chapter; a stable citation number spans the doc.
	type fact struct {
		n int
		m store.Memory
	}
	byChap := map[string][]fact{}
	for i, m := range mems {
		ch := ""
		for _, t := range m.Tags {
			if v, ok := strings.CutPrefix(t, "chapter:"); ok {
				ch = v
				break
			}
		}
		byChap[ch] = append(byChap[ch], fact{n: i + 1, m: m})
	}
	origins := func(m store.Memory) string {
		var out []string
		for _, s := range m.Sources {
			if v, ok := strings.CutPrefix(s, "project:"); ok && !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
		return strings.Join(out, ",")
	}
	factLines := func(fs []fact) string {
		var b strings.Builder
		for _, f := range fs {
			if o := origins(f.m); o != "" {
				fmt.Fprintf(&b, "[%d] (x%d, from %s) %s\n", f.n, f.m.Corroboration+1, o, f.m.Text)
			} else {
				fmt.Fprintf(&b, "[%d] (x%d) %s\n", f.n, f.m.Corroboration+1, f.m.Text)
			}
		}
		return b.String()
	}
	// Section order: declared chapters first, then an unfiled tail.
	type section struct{ title, about, body string }
	var sections []section
	order := append([]Chapter{}, chapters...)
	if len(byChap[""]) > 0 {
		order = append(order, Chapter{Name: "unfiled", About: "facts not yet filed into a chapter"})
	}
	for _, c := range order {
		key := c.Name
		if key == "unfiled" {
			key = ""
		}
		fs := byChap[key]
		if len(fs) == 0 {
			continue
		}
		body, u, err := syn.Complete(fmt.Sprintf(docSectionPrompt,
			name, orText(about, "a shared knowledge base"), c.Name, c.About, factLines(fs)))
		rep.Usage.add(u)
		if err != nil {
			return rep, fmt.Errorf("section %q: %w", c.Name, err)
		}
		sections = append(sections, section{title: c.Name, about: c.About, body: strings.TrimSpace(body)})
	}
	rep.Sections = len(sections)
	// Overview from the section map plus the strongest facts.
	top := append([]store.Memory{}, mems...)
	sort.Slice(top, func(i, j int) bool { return top[i].Corroboration > top[j].Corroboration })
	if len(top) > 12 {
		top = top[:12]
	}
	var toc, sample strings.Builder
	for _, s := range sections {
		fmt.Fprintf(&toc, "- %s: %s\n", s.title, s.about)
	}
	for _, m := range top {
		fmt.Fprintf(&sample, "- (x%d) %s\n", m.Corroboration+1, m.Text)
	}
	overview, u, err := syn.Complete(fmt.Sprintf(docOverviewPrompt,
		name, orText(about, "a shared knowledge base"), toc.String(), sample.String()))
	rep.Usage.add(u)
	if err != nil {
		return rep, fmt.Errorf("overview: %w", err)
	}
	// Assemble, with a citation appendix so [n] resolves without the GUI.
	now := time.Now().UTC().Format(time.RFC3339)
	var doc strings.Builder
	fmt.Fprintf(&doc, "# %s — design document\n\n", name)
	fmt.Fprintf(&doc, "%s\n\n", strings.TrimSpace(overview))
	for _, s := range sections {
		fmt.Fprintf(&doc, "## %s\n\n%s\n\n", s.title, s.body)
	}
	fmt.Fprintf(&doc, "## Sources\n\n")
	for i, m := range mems {
		fmt.Fprintf(&doc, "[%d] %s _(x%d, %s)_\n\n", i+1, m.Text, m.Corroboration+1, m.CreatedAt[:10])
	}
	fmt.Fprintf(&doc, "---\n_Generated %s from %d curated facts._\n", now, len(mems))
	out := doc.String()
	rep.Chars = len(out)
	if err := db.SetMeta("design_doc", out); err != nil {
		return rep, err
	}
	if err := db.SetMeta("design_doc_ts", now); err != nil {
		return rep, err
	}
	return rep, nil
}

func orText(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
