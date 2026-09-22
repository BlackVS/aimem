package server

import (
	"errors"
	"net/http"
	"strings"

	"aimem/internal/store"
)

func managedTaskResponse(project string, t store.Task) any {
	return struct {
		Version       int          `json:"protocol_version"`
		Task          taskResponse `json:"task"`
		WorkflowReady bool         `json:"workflow_ready"`
	}{1, taskView(project, t), true}
}

// handoffTeamCoordinator serves both authorities the design names: the
// current coordinator with its bound ordinary session, or an admin closing
// a lost coordinator with recorded reconciliation. The transport proves the
// credential is live; storage tells the two apart by the actor kind it is
// handed and refuses reconciliation from one and requires it from the other.
func (s *Server) handoffTeamCoordinator(w http.ResponseWriter, r *http.Request) {
	id, ok := IdentityFrom(r.Context())
	var db *store.DB
	switch {
	case ok && id.Role == "user":
		db = s.sessionProject(w, r)
	case !ok || id.Role == "admin":
		_, db = s.adminTeamProject(w, r)
	default:
		s.fail(w, 403, errors.New("coordinator handoff requires an ordinary project write token or an admin token"))
		return
	}
	if db == nil {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var req store.TeamHandoffCommand
	if err := decodeSessionBody(w, r, &req); err != nil {
		s.fail(w, 400, err)
		return
	}
	_, timeout, err := sessionPolicy()
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	audit, err := sessionAudit(w, r, db)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	out, err := db.HandoffTeamCoordinator(r.PathValue("team"), req, audit, key)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.sessionResult(w, r, out, timeout, false)
}

// changeManagedTask routes the coordinator's edit and finalize commands. Only
// an ordinary bound coordinator session may call them; storage checks the
// coordinator generation, management, reservation and revision.
func (s *Server) changeManagedTask(w http.ResponseWriter, r *http.Request) {
	db := s.sessionProject(w, r)
	if db == nil {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	team, task := r.PathValue("team"), r.PathValue("task")
	var run func(store.TeamAuditContext) (store.Task, error)
	if strings.HasSuffix(r.URL.Path, "/finalize") {
		var req store.TeamFinalizeCommand
		if err := decodeSessionBody(w, r, &req); err != nil {
			s.fail(w, 400, err)
			return
		}
		run = func(a store.TeamAuditContext) (store.Task, error) {
			return db.FinalizeManagedTask(team, task, req, a, key)
		}
	} else {
		var req store.TeamManagedEditCommand
		if err := decodeSessionBody(w, r, &req); err != nil {
			s.fail(w, 400, err)
			return
		}
		run = func(a store.TeamAuditContext) (store.Task, error) { return db.EditManagedTask(team, task, req, a, key) }
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
	s.ok(w, managedTaskResponse(r.PathValue("p"), out))
}

// recoverTeamAssignment is an admin route: the gate never lets an ordinary
// token reach it, and storage additionally refuses any non-admin actor.
func (s *Server) recoverTeamAssignment(w http.ResponseWriter, r *http.Request) {
	_, db := s.adminTeamProject(w, r)
	if db == nil {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var req store.TeamRecoveryCommand
	if err := decodeSessionBody(w, r, &req); err != nil {
		s.fail(w, 400, err)
		return
	}
	audit, err := sessionAudit(w, r, db)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	out, err := db.RecoverTeamAssignment(r.PathValue("team"), r.PathValue("attempt"), req, audit, key)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, assignmentResponse(out))
}

// unmanageTask is an admin route. The URL names the managing team; storage
// checks it inside the transaction and binds it into the receipt scope, so a
// task another team took over is refused and a retry replays only through
// the same team prefix.
func (s *Server) unmanageTask(w http.ResponseWriter, r *http.Request) {
	p, db := s.adminTeamProject(w, r)
	if db == nil {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var req store.TeamUnmanageCommand
	if err := decodeSessionBody(w, r, &req); err != nil {
		s.fail(w, 400, err)
		return
	}
	audit, err := sessionAudit(w, r, db)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	out, err := db.UnmanageTask(r.PathValue("team"), r.PathValue("task"), req, audit, key)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, managedTaskResponse(p, out))
}
