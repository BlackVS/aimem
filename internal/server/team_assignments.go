package server

import (
	"errors"
	"net/http"
	"strings"

	"aimem/internal/store"
)

// Capture the current project instance before entering the project transaction.
// The callback reads only the separate access database, using persisted session
// bindings, never user/token IDs supplied in the assignment request.
func (s *Server) assignmentAuthority(w http.ResponseWriter, r *http.Request) store.WorkerAuthority {
	instance, err := s.reg.ExistingProjectAccessID(r.PathValue("p"))
	if err != nil || instance == "" {
		s.teamError(w, r, errors.New("project access identity unavailable"))
		return nil
	}
	acc, ok := s.accessStore(w)
	if !ok {
		return nil
	}
	return func(worker store.TeamSession) error {
		allowed, err := acc.CanWriteToken(worker.UserID, worker.TokenID, instance)
		if err != nil {
			return err
		}
		if !allowed {
			return store.ErrTeamSessionDenied
		}
		return nil
	}
}

func assignmentResponse(a store.TeamAssignment) any {
	return struct {
		Version       int                  `json:"protocol_version"`
		Assignment    store.TeamAssignment `json:"assignment"`
		WorkflowReady bool                 `json:"workflow_ready"`
	}{1, a, false}
}

func (s *Server) offerTeamAssignment(w http.ResponseWriter, r *http.Request) {
	db := s.sessionProject(w, r)
	if db == nil {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var req store.TeamOffer
	if err := decodeSessionBody(w, r, &req); err != nil {
		s.fail(w, 400, err)
		return
	}
	authorize := s.assignmentAuthority(w, r)
	if authorize == nil {
		return
	}
	audit, err := sessionAudit(w, r, db)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	out, err := db.OfferTeamAssignment(r.PathValue("team"), req, audit, key, authorize)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.created(w, assignmentResponse(out))
}

func (s *Server) changeTeamAssignment(w http.ResponseWriter, r *http.Request) {
	db := s.sessionProject(w, r)
	if db == nil {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var req store.TeamAssignmentCommand
	if err := decodeSessionBody(w, r, &req); err != nil {
		s.fail(w, 400, err)
		return
	}
	op := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	var authorize store.WorkerAuthority
	if op == "accept" {
		authorize = s.assignmentAuthority(w, r)
		if authorize == nil {
			return
		}
	}
	audit, err := sessionAudit(w, r, db)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	out, err := db.ChangeTeamAssignment(r.PathValue("team"), r.PathValue("attempt"), op, req, audit, key, authorize)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, assignmentResponse(out))
}

func (s *Server) getTeamAssignment(w http.ResponseWriter, r *http.Request) {
	db := s.sessionProject(w, r)
	if db == nil {
		return
	}
	h, ok := s.sessionHandleQuery(w, r)
	if !ok {
		return
	}
	out, err := db.GetTeamAssignment(r.PathValue("team"), r.PathValue("attempt"), h, taskActor(r))
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, assignmentResponse(out))
}
