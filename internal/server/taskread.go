package server

// task.read: one task's text for aicrewd, which freezes it on an attempt at
// the offer (aicrew 01a157fb-4942). The scope is board.read's: the projects
// granted to the calling peer's enabled team profiles. An unknown project,
// one not granted, a reserved one and an unknown task get one answer, so a
// probe learns nothing; the audit keeps the exact reason. The answer carries
// the task's text, state and revision, nothing else; its size is bounded by
// the task's own write limits (store.MaxTaskTitleBytes, store.MaxTaskBytes).

import (
	"errors"
	"fmt"
	"net/http"
	"os"

	"aimem/internal/store"
)

// taskTextView is task.read's answer.
type taskTextView struct {
	Project            string `json:"project"`
	TaskID             string `json:"task_id"`
	Revision           int64  `json:"revision"`
	State              string `json:"state"`
	Title              string `json:"title"`
	Objective          string `json:"objective"`
	AcceptanceCriteria string `json:"acceptance_criteria"`
	NonGoals           string `json:"non_goals"`
}

// readTaskText is GET /v1/identity/peers/{service_id}/task-reads/{project}/{task_id}.
func (s *Server) readTaskText(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.teamPeer(w, r)
	if !ok {
		return
	}
	project, taskID := r.PathValue("project"), r.PathValue("task_id")
	notFound := func(reason string) { s.teamOpRefuseWhy(w, r, peer, "task_not_found", reason) }
	if store.IsReservedProject(project) {
		notFound("reserved_project")
		return
	}
	instance, err := s.reg.ExistingProjectAccessID(project)
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, store.ErrInvalidAccessProject):
		notFound("unknown_project")
		return
	case err != nil:
		s.teamReadFailed(w, err)
		return
	case instance == "":
		notFound("not_granted")
		return
	}
	db, err := s.openAccess(false)
	if err != nil {
		s.identityRefuse(w, "identity_unavailable")
		return
	}
	granted, err := peerGrantedInstances(db, peer.ServiceID)
	if err != nil {
		s.teamReadFailed(w, err)
		return
	}
	if !granted[instance] {
		notFound("not_granted")
		return
	}
	pdb, err := s.reg.OpenExisting(project)
	if errors.Is(err, store.ErrNoSuchProject) {
		notFound("unknown_project")
		return
	}
	if err != nil {
		s.teamReadFailed(w, err)
		return
	}
	t, err := pdb.GetTask(taskID)
	if errors.Is(err, store.ErrTaskNotFound) {
		notFound("unknown_task")
		return
	}
	if err != nil {
		s.teamReadFailed(w, err)
		return
	}
	if err := db.RecordTeamRequest("peer:"+peer.ServiceID, "task.read", fmt.Sprintf("service=%s project=%s task=%s revision=%d", peer.ServiceID, project, t.ID, t.Revision)); err != nil {
		s.teamReadFailed(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, taskTextView{project, t.ID, t.Revision, t.State, t.Title, t.Objective, t.AcceptanceCriteria, t.NonGoals})
}
