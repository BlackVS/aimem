package server

// Chapter propose/approve/refile endpoints: an LLM proposes chapter
// filings for unfiled facts, a human approves, apply refiles.
// Propose is read-only: the plan lands in the group's
// chapter_proposal meta for review. Apply writes only the human-approved
// subset — new chapters into the chapters meta, chapter tags onto facts —
// through the same audited paths every other write uses.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"aimem/internal/curate"
	"aimem/internal/provider"
)

// chapterName rules: lowercase slug, ≤56 chars so "chapter:"+name fits
// normTag's 64-char cap, and never the reserved bucket name.
var chapterName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,55}$`)

// curateSynth builds the synthesis backend the same way `aimem curate`
// picks it: the configured curate model's registry binding decides by
// provider kind; env backend is the fallback for unbound models.
func (s *Server) curateSynth() (curate.Synthesizer, string, error) {
	root := s.reg.Root()
	m := os.Getenv("AIMEM_CURATE_MODEL")
	claude := func(model string) curate.Synthesizer {
		workDir := filepath.Join(root, "curator-workdir")
		os.MkdirAll(workDir, 0o700)
		return &curate.ClaudeExtractor{Model: model, WorkDir: workDir}
	}
	if ep, bound := provider.ResolveBound(root, m); bound {
		if ep.Kind == "claude" {
			return claude(ep.Model), m, nil
		}
		return &curate.OpenAIExtractor{BaseURL: ep.BaseURL, APIKey: ep.Token, Model: ep.Model}, m, nil
	}
	switch os.Getenv("AIMEM_CURATE_BACKEND") {
	case "openai":
		ep, ok := provider.Resolve(root, m)
		if m == "" || !ok || ep.Kind != "openai" {
			return nil, "", fmt.Errorf("no curate endpoint: bind AIMEM_CURATE_MODEL in providers.json or set AIMEM_OPENAI_API_KEY")
		}
		return &curate.OpenAIExtractor{BaseURL: ep.BaseURL, APIKey: ep.Token, Model: ep.Model}, m, nil
	default: // claude
		if m == "" {
			m = "haiku"
		}
		return claude(m), m, nil
	}
}

// proposeChapters generates a fresh plan and parks it in the group's
// chapter_proposal meta for review. Synchronous: a claude CLI turn can
// take ~30s, which an admin clicking a button will tolerate.
func (s *Server) proposeChapters(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	p := r.PathValue("p")
	if !strings.HasPrefix(p, "group-") {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("chapter workflows apply to groups only (got %q)", p))
		return
	}
	syn, model, err := s.curateSynth()
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	about, _ := db.GetMeta("about")
	var chapters []curate.Chapter
	if raw, _ := db.GetMeta("chapters"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &chapters)
	}
	// ?revisit=1 runs the RE-LABEL variant: already-filed facts get
	// additional chapters from the evolved set (add-only; the model can
	// never move or unfile). Same review/apply flow either way.
	revisit := r.URL.Query().Get("revisit") == "1"
	plan, err := curate.ProposeChapters(db, strings.TrimPrefix(p, "group-"), about, chapters, syn, 0, revisit)
	if err != nil {
		s.fail(w, http.StatusBadGateway, err)
		return
	}
	plan.Model = model
	raw, _ := json.Marshal(plan)
	if err := db.SetMeta("chapter_proposal", string(raw)); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Warn("chapter proposal generated", "project", p, "model", model, "revisit", revisit,
		"pool", plan.Unfiled, "assign", len(plan.Assign), "new_chapters", len(plan.NewChapters))
	s.ok(w, plan)
}

// applyChapters writes the approved subset of a plan: append approved new
// chapters to the group's chapters meta, then tag each approved fact.
func (s *Server) applyChapters(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	p := r.PathValue("p")
	if !strings.HasPrefix(p, "group-") {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("chapter workflows apply to groups only (got %q)", p))
		return
	}
	var req curate.ChapterPlan
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	var chapters []curate.Chapter
	if raw, _ := db.GetMeta("chapters"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &chapters)
	}
	valid := map[string]bool{}
	for _, c := range chapters {
		valid[strings.ToLower(c.Name)] = true
	}
	// Validate every new chapter BEFORE writing anything.
	for _, nc := range req.NewChapters {
		n := strings.ToLower(strings.TrimSpace(nc.Name))
		if !chapterName.MatchString(n) || n == "unfiled" {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("chapter name %q: want lowercase letters/digits/dashes, max 56 chars, not \"unfiled\"", nc.Name))
			return
		}
		if valid[n] {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("chapter %q already exists", n))
			return
		}
		valid[n] = true
	}
	for _, a := range req.Assign {
		if !valid[strings.ToLower(a.Chapter)] {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("assignment to unknown chapter %q", a.Chapter))
			return
		}
	}
	for _, nc := range req.NewChapters {
		chapters = append(chapters, curate.Chapter{Name: strings.ToLower(strings.TrimSpace(nc.Name)), About: strings.TrimSpace(nc.About)})
	}
	if len(req.NewChapters) > 0 {
		raw, _ := json.Marshal(chapters)
		if err := db.SetMeta("chapters", string(raw)); err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
	}
	filed, failed := 0, 0
	file := func(factID, chapter string) {
		if db.Tag(factID, "chapter:"+strings.ToLower(chapter), "admin-refile") == nil {
			filed++
		} else {
			failed++
		}
	}
	for _, nc := range req.NewChapters {
		for _, id := range nc.FactIDs {
			file(id, nc.Name)
		}
	}
	for _, a := range req.Assign {
		file(a.FactID, a.Chapter)
	}
	// The reviewed plan is spent either way.
	_ = db.SetMeta("chapter_proposal", "")
	s.log.Warn("chapter plan applied", "project", p,
		"new_chapters", len(req.NewChapters), "filed", filed, "failed", failed)
	s.ok(w, map[string]any{"new_chapters": len(req.NewChapters), "filed": filed, "failed": failed})
}
