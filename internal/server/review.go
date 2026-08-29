package server

// The staleness review surface (FEATURE-PROPOSALS #5): list the derived
// queue, confirm a fact. Supersede and forget — the other two verdicts —
// already exist as routes; review adds no state, only the query and the
// audited "still true" touch.

import (
	"net/http"
	"strconv"
	"time"

	"aimem/internal/store"
)

func (s *Server) reviewQueue(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 {
		days = store.DefaultReviewAgeDays
	}
	maxCorr := store.DefaultReviewMaxCorroboration
	if v := r.URL.Query().Get("max_corroboration"); v != "" {
		maxCorr, _ = strconv.Atoi(v)
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format(time.RFC3339)
	items, err := db.ReviewQueue(cutoff, maxCorr, limit)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if items == nil {
		items = []store.ReviewItem{}
	}
	s.ok(w, map[string]any{"items": items, "cutoff": cutoff})
}

func (s *Server) confirmMemory(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	req, ok := s.decodeMem(w, r)
	if !ok {
		return
	}
	if err := db.Confirm(r.PathValue("id"), req.Actor); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.log.Info("memory confirmed", "project", r.PathValue("p"), "id", r.PathValue("id"), "actor", req.Actor)
	s.ok(w, map[string]any{"confirmed": true})
}
