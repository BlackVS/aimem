package server

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"aimem/internal/store"
)

func (s *Server) sendTeamMessage(w http.ResponseWriter, r *http.Request) {
	db := s.sessionProject(w, r)
	if db == nil {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var req struct {
		store.TeamSessionHandle
		store.TeamMessageContent
	}
	if err := decodeSessionBody(w, r, &req); err != nil {
		s.fail(w, 400, err)
		return
	}
	limit := store.MaxTeamMessages
	if raw := os.Getenv("AIMEM_TEAM_MAX_MESSAGES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000000 {
			s.teamError(w, r, errors.New("invalid operator message quota"))
			return
		}
		limit = n
	}
	audit, err := sessionAudit(w, r, db)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	m, err := db.SendTeamMessage(r.PathValue("team"), req.TeamSessionHandle, req.TeamMessageContent, audit, key, limit)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	s.created(w, map[string]any{"protocol_version": 1, "message": m})
}

func (s *Server) ackTeamMessages(w http.ResponseWriter, r *http.Request) {
	db := s.sessionProject(w, r)
	if db == nil {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var req struct {
		store.TeamSessionHandle
		MessageIDs []string `json:"message_ids"`
	}
	if err := decodeSessionBody(w, r, &req); err != nil {
		s.fail(w, 400, err)
		return
	}
	audit, err := sessionAudit(w, r, db)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	ack, err := db.AckTeamMessages(r.PathValue("team"), req.TeamSessionHandle, req.MessageIDs, audit, key)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	s.ok(w, struct {
		Version int `json:"protocol_version"`
		store.TeamAck
	}{1, ack})
}

func (s *Server) teamMessages(w http.ResponseWriter, r *http.Request) {
	inbox := strings.HasSuffix(r.URL.Path, "/inbox")
	q := r.URL.Query()
	for k, values := range q {
		if len(values) != 1 || (k != "session_id" && k != "generation" && k != "after" && k != "limit" && !(inbox && k == "wait_seconds")) {
			s.fail(w, 400, errors.New("invalid message query"))
			return
		}
	}
	number := func(name string, fallback, min, max int64) (int64, error) {
		if _, exists := q[name]; !exists {
			return fallback, nil
		}
		n, err := strconv.ParseInt(q.Get(name), 10, 64)
		if err != nil || n < min || n > max {
			return 0, errors.New("invalid " + name)
		}
		return n, nil
	}
	generation, err := number("generation", 0, 1, 1<<63-1)
	if err != nil || generation == 0 {
		s.fail(w, 400, errors.New("session generation required"))
		return
	}
	after, err := number("after", 0, 0, 1<<63-1)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	limit, err := number("limit", 50, 1, 100)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	wait, err := number("wait_seconds", 0, 0, 25)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		if r.Context().Err() != nil {
			return
		}
		// A wait holds no transaction. Recheck enablement, live token/grant and
		// session binding on every poll, including the final response.
		db := s.sessionProject(w, r)
		if db == nil {
			return
		}
		audit, err := sessionAudit(w, r, db)
		if err != nil {
			s.teamError(w, r, err)
			return
		}
		page, err := db.TeamMessages(r.PathValue("team"), store.TeamSessionHandle{SessionID: q.Get("session_id"), Generation: generation}, after, int(limit), inbox, audit)
		if err != nil {
			s.teamError(w, r, err)
			return
		}
		if len(page.Messages) != 0 || !time.Now().Before(deadline) {
			w.Header().Set("Cache-Control", "no-store")
			s.ok(w, struct {
				Version int `json:"protocol_version"`
				store.TeamMessagePage
			}{1, page})
			return
		}
		timer := time.NewTimer(min(time.Second, time.Until(deadline)))
		select {
		case <-r.Context().Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
