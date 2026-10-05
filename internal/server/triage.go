package server

// Coordinator triage (docs/DESIGN-AIFORGE-PILOT-1.md §4, decision (a)): a
// partial task update that moves a task between BACKLOG and READY and sets
// its next action. In team mode it is the coordinator's only task write,
// with comments; the verified context's role must be coordinator and the
// profile must hold a live grant on the task's project (locateTask). A task
// under any reservation refuses both with task_held. Personal mode keeps the
// ordinary task-write authorization.

import (
	"errors"
	"fmt"
	"net/http"

	"aimem/internal/store"
)

// teamTaskWriteRole is the one team role that may triage.
const teamTaskWriteRole = "coordinator"

// authorizeTriageWrite decides a triage or comment write: in team mode the
// coordinator role (the grant was checked by locateTask), otherwise the
// ordinary task-write check. It reports whether the request is in team
// mode, and has answered the refusal when ok is false.
func (s *Server) authorizeTriageWrite(w http.ResponseWriter, r *http.Request, project string) (team, ok bool) {
	tc, team := teamContextFrom(r.Context())
	if !team {
		return false, s.authorizeTaskWrite(w, r, project)
	}
	if tc.Role != teamTaskWriteRole {
		id, _ := IdentityFrom(r.Context())
		s.teamDeny(w, r, tc.Role, id, "role_forbidden", teamAuditDetail(tc.Context, r)+fmt.Sprintf(" project=%q reason=triage_coordinator_only", project), tc.CorrelationID)
		return true, false
	}
	return true, s.tasksEnabledFor(w, project)
}

// teamTaskHeld answers a team triage write on a held task.
func (s *Server) teamTaskHeld(w http.ResponseWriter, r *http.Request, project string) {
	tc, _ := teamContextFrom(r.Context())
	id, _ := IdentityFrom(r.Context())
	s.teamDeny(w, r, tc.Role, id, "task_held", teamAuditDetail(tc.Context, r)+fmt.Sprintf(" project=%q", project), tc.CorrelationID)
}

func heldTask(err error) bool {
	return errors.Is(err, store.ErrTaskReserved) || errors.Is(err, store.ErrManagedTask)
}

// triageTask is POST /v1/tasks/{id}/triage.
func (s *Server) triageTask(w http.ResponseWriter, r *http.Request) {
	project, db, ok := s.locateTask(w, r)
	if !ok {
		return
	}
	team, ok := s.authorizeTriageWrite(w, r, project)
	if !ok {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var body struct {
		ExpectedRevision int64   `json:"expected_revision"`
		State            *string `json:"state"`
		NextAction       *string `json:"next_action"`
	}
	if !s.decodeTaskBody(w, r, &body) {
		return
	}
	task, err := db.TriageTask(r.PathValue("id"), store.TaskTriage{State: body.State, NextAction: body.NextAction}, body.ExpectedRevision, taskActor(r), key)
	if err != nil {
		var conflict *store.TaskConflict
		switch {
		case errors.As(err, &conflict):
			s.taskConflict(w, project, err, conflict)
		case team && heldTask(err):
			s.teamTaskHeld(w, r, project)
		default:
			s.taskError(w, err)
		}
		return
	}
	s.log.Info("task triaged", "project", project, "task", task.ID, "revision", task.Revision, "state", task.State, "actor", taskActor(r).Name)
	s.ok(w, taskView(project, task))
}
