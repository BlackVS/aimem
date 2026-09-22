package server

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"aimem/internal/store"
)

// sessionHandleQuery reads the only two query parameters a session-scoped
// read accepts. Handles are concurrency keys, never authority: the caller's
// token was already authenticated as an ordinary project writer.
func (s *Server) sessionHandleQuery(w http.ResponseWriter, r *http.Request) (store.TeamSessionHandle, bool) {
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		s.fail(w, 400, errors.New("invalid assignment query"))
		return store.TeamSessionHandle{}, false
	}
	for k, values := range q {
		if len(values) != 1 || (k != "session_id" && k != "generation") {
			s.fail(w, 400, errors.New("invalid assignment query"))
			return store.TeamSessionHandle{}, false
		}
	}
	generation, err := strconv.ParseInt(q.Get("generation"), 10, 64)
	if err != nil || generation < 1 || q.Get("session_id") == "" {
		s.fail(w, 400, errors.New("session_id and positive generation required"))
		return store.TeamSessionHandle{}, false
	}
	return store.TeamSessionHandle{SessionID: q.Get("session_id"), Generation: generation}, true
}

// reservedTeamAssignment returns the calling session's outstanding attempt:
// the first read a resumed worker makes before reconciling local state.
func (s *Server) reservedTeamAssignment(w http.ResponseWriter, r *http.Request) {
	db := s.sessionProject(w, r)
	if db == nil {
		return
	}
	h, ok := s.sessionHandleQuery(w, r)
	if !ok {
		return
	}
	out, err := db.ReservedTeamAssignment(r.PathValue("team"), h, taskActor(r))
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, assignmentResponse(out))
}

// changeTeamExecution routes worker and coordinator execution commands to the
// storage methods that own their authority and transitions. The caller is
// authenticated as an ordinary project writer on every request, including
// retries; role, session, generation, ownership and revision checks belong to
// storage, so every transport refuses identically.
func (s *Server) changeTeamExecution(w http.ResponseWriter, r *http.Request) {
	db := s.sessionProject(w, r)
	if db == nil {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	team, attempt := r.PathValue("team"), r.PathValue("attempt")
	op := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	var run func(store.TeamAuditContext) (store.TeamAssignment, error)
	switch op {
	case "submit":
		var req store.TeamResultSubmission
		if err := decodeSessionBody(w, r, &req); err != nil {
			s.fail(w, 400, err)
			return
		}
		run = func(a store.TeamAuditContext) (store.TeamAssignment, error) {
			return db.SubmitTeamResult(team, attempt, req, a, key)
		}
	case "review":
		var req store.TeamResultDecision
		if err := decodeSessionBody(w, r, &req); err != nil {
			s.fail(w, 400, err)
			return
		}
		run = func(a store.TeamAuditContext) (store.TeamAssignment, error) {
			return db.ReviewTeamResult(team, attempt, req, a, key)
		}
	default:
		var req store.TeamWorkCommand
		if err := decodeSessionBody(w, r, &req); err != nil {
			s.fail(w, 400, err)
			return
		}
		run = func(a store.TeamAuditContext) (store.TeamAssignment, error) {
			return db.ChangeTeamWork(team, attempt, op, req, a, key)
		}
	}
	audit, err := sessionAudit(w, r, db)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	out, err := run(audit)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, assignmentResponse(out))
}
