package store

import (
	"database/sql"
	"errors"

	"aimem/internal/uuidv7"
)

// TeamManagedChange is the immutable audit record of one managed-task
// lifecycle operation: which task moved, between which revisions and states,
// why, and for finalization which accepted attempt and delivery evidence.
type TeamManagedChange struct {
	TaskID           string    `json:"task_id"`
	AttemptID        string    `json:"attempt_id,omitempty"`
	PreviousRevision int64     `json:"previous_revision"`
	Revision         int64     `json:"revision"`
	PreviousState    string    `json:"previous_state"`
	State            string    `json:"state"`
	Reason           string    `json:"reason"`
	Evidence         []TaskRef `json:"evidence,omitempty"`
	CreatedAt        string    `json:"created_at"`
}

// TeamManagedEditCommand replaces managed task content under revision CAS.
// The state is set by coordination operations, never by an edit.
type TeamManagedEditCommand struct {
	TeamSessionHandle
	CoordinatorGeneration int64       `json:"coordinator_generation"`
	ExpectedRevision      int64       `json:"expected_revision"`
	Content               TaskContent `json:"content"`
	Reason                string      `json:"reason"`
}

// TeamFinalizeCommand closes a REVIEW task whose named attempt was accepted,
// recording the merge/delivery evidence the hub cannot verify itself.
type TeamFinalizeCommand struct {
	TeamSessionHandle
	CoordinatorGeneration int64     `json:"coordinator_generation"`
	ExpectedRevision      int64     `json:"expected_revision"`
	AttemptID             string    `json:"attempt_id"`
	Reason                string    `json:"reason"`
	Evidence              []TaskRef `json:"evidence"`
}

type TeamUnmanageCommand struct {
	ExpectedRevision int64  `json:"expected_revision"`
	Reason           string `json:"reason"`
}

// managedTask reads the task at the expected revision and proves the calling
// team manages it with no reserved attempt. It runs inside the transaction
// that writes the task.
func managedTask(tx *sql.Tx, teamID, taskID string, revision int64) (Task, error) {
	t, err := readTask(tx, taskID)
	if err != nil {
		return t, err
	}
	if t.Revision != revision {
		return t, &TaskConflict{Current: t}
	}
	if t.Coordination == nil || t.Coordination.TeamID != teamID || t.Coordination.AttemptID != "" {
		return t, ErrTeamAssignmentConflict
	}
	return t, nil
}

func managedEvent(tx *sql.Tx, tm Team, s *TeamSession, operation string, change TeamManagedChange, a TeamAuditContext) error {
	change.CreatedAt = nowUTC()
	return putTeamEvent(tx, tm.ID, TeamEvent{ID: uuidv7.New(), ProtocolVersion: 1, Operation: operation, At: change.CreatedAt, TeamAuditContext: a, Team: tm, Session: s, PreviousRevision: change.PreviousRevision, Managed: &change})
}

// EditManagedTask lets the current coordinator replace a managed task's content
// between attempts. Changing scope during execution requires a stop and a new
// offer; the task state and archive flag are owned by coordination operations.
// The service MUST authenticate the caller's live ordinary write token before
// every call, including receipt replay.
func (d *DB) EditManagedTask(teamID, taskID string, c TeamManagedEditCommand, a TeamAuditContext, key string) (Task, error) {
	if !taskIDRE.MatchString(taskID) {
		return Task{}, ErrTaskNotFound
	}
	if c.CoordinatorGeneration < 1 || c.ExpectedRevision < 1 {
		return Task{}, invalid(errors.New("positive coordinator_generation and expected_revision required"))
	}
	if err := taskText(c.Reason, 4096, true); err != nil {
		return Task{}, invalid(err)
	}
	if err := c.Content.validate(); err != nil {
		return Task{}, invalid(err)
	}
	if c.Content.Archived {
		return Task{}, invalid(errors.New("managed tasks are archived after unmanage, not by edit"))
	}
	check := func(tx *sql.Tx) error { _, _, err := boundTeamSession(tx, teamID, c.SessionID, a.Actor); return err }
	return checkedTaskMutation(d, a.Actor, "team.task.edit", taskID, key, c, check, func(tx *sql.Tx) (Task, error) {
		tm, coordinator, err := currentCoordinator(tx, teamID, c.TeamSessionHandle, c.CoordinatorGeneration, a.Actor)
		if err != nil {
			return Task{}, err
		}
		t, err := managedTask(tx, teamID, taskID, c.ExpectedRevision)
		if err != nil {
			return Task{}, err
		}
		if t.Archived {
			return Task{}, ErrTaskArchived
		}
		if c.Content.State != t.State {
			return Task{}, invalid(errors.New("managed task state is set by coordination operations; edit must keep " + t.State))
		}
		if err := checkEpicAssignable(tx, c.Content.Epic, t.Epic); err != nil {
			return Task{}, err
		}
		change := TeamManagedChange{TaskID: t.ID, PreviousRevision: t.Revision, PreviousState: t.State, Reason: c.Reason}
		t.TaskContent = c.Content
		t.Revision++
		t.UpdatedAt = nowUTC()
		change.Revision, change.State = t.Revision, t.State
		if err := saveTask(tx, t, a.Actor, false); err != nil {
			return Task{}, err
		}
		return t, managedEvent(tx, tm, &coordinator, "team.task.edit", change, a)
	})
}

// FinalizeManagedTask records DONE for a REVIEW task whose named attempt the
// coordinator accepted, appending the merge/delivery evidence to the task.
// Acceptance alone never finalizes; the hub does not verify the evidence.
func (d *DB) FinalizeManagedTask(teamID, taskID string, c TeamFinalizeCommand, a TeamAuditContext, key string) (Task, error) {
	if !taskIDRE.MatchString(taskID) {
		return Task{}, ErrTaskNotFound
	}
	if c.CoordinatorGeneration < 1 || c.ExpectedRevision < 1 || !taskIDRE.MatchString(c.AttemptID) {
		return Task{}, invalid(errors.New("positive coordinator_generation, expected_revision and attempt_id required"))
	}
	if err := taskText(c.Reason, 4096, true); err != nil {
		return Task{}, invalid(err)
	}
	if len(c.Evidence) == 0 {
		return Task{}, invalid(errors.New("finalize requires merge/delivery evidence"))
	}
	if err := validateRefs("evidence", c.Evidence); err != nil {
		return Task{}, invalid(err)
	}
	check := func(tx *sql.Tx) error { _, _, err := boundTeamSession(tx, teamID, c.SessionID, a.Actor); return err }
	return checkedTaskMutation(d, a.Actor, "team.task.finalize", taskID, key, c, check, func(tx *sql.Tx) (Task, error) {
		tm, coordinator, err := currentCoordinator(tx, teamID, c.TeamSessionHandle, c.CoordinatorGeneration, a.Actor)
		if err != nil {
			return Task{}, err
		}
		t, err := managedTask(tx, teamID, taskID, c.ExpectedRevision)
		if err != nil {
			return Task{}, err
		}
		if t.State != "REVIEW" || t.Archived {
			return Task{}, ErrTeamAssignmentConflict
		}
		attempt, err := readTeamAssignment(tx, teamID, c.AttemptID)
		if err != nil {
			return Task{}, err
		}
		if attempt.TaskID != taskID || attempt.State != "ACCEPTED" {
			return Task{}, ErrTeamAssignmentConflict
		}
		change := TeamManagedChange{TaskID: t.ID, AttemptID: attempt.ID, PreviousRevision: t.Revision, PreviousState: t.State, Reason: c.Reason, Evidence: c.Evidence}
		t.EvidenceRefs = append(t.EvidenceRefs, c.Evidence...)
		t.State = "DONE"
		// Appending evidence must not push a near-limit task past its bounds.
		if err := t.TaskContent.validate(); err != nil {
			return Task{}, invalid(err)
		}
		t.Revision++
		t.UpdatedAt = nowUTC()
		change.Revision, change.State = t.Revision, t.State
		if err := saveTask(tx, t, a.Actor, false); err != nil {
			return Task{}, err
		}
		return t, managedEvent(tx, tm, &coordinator, "team.task.finalize", change, a)
	})
}

// UnmanageTask releases a task from team management so ordinary writes apply
// again. It needs a trusted admin actor, no reserved attempt and a reason; the
// service MUST authenticate live admin authority on every call, including
// receipt replay. Historical attempts stay readable, and a later offer manages
// the task afresh.
func (d *DB) UnmanageTask(taskID string, c TeamUnmanageCommand, a TeamAuditContext, key string) (Task, error) {
	if a.Actor.Kind != "admin" {
		return Task{}, ErrTeamSessionDenied
	}
	if !taskIDRE.MatchString(taskID) {
		return Task{}, ErrTaskNotFound
	}
	if c.ExpectedRevision < 1 {
		return Task{}, invalid(errors.New("positive expected_revision required"))
	}
	if err := taskText(c.Reason, 4096, true); err != nil {
		return Task{}, invalid(err)
	}
	check := func(tx *sql.Tx) error {
		var enabled string
		if err := tx.QueryRow(`SELECT COALESCE((SELECT value FROM meta WHERE key=?),'')`, TasksMetaKey).Scan(&enabled); err != nil {
			return err
		}
		if enabled != "on" {
			return ErrTeamSessionDenied
		}
		return nil
	}
	return checkedTaskMutation(d, a.Actor, "team.task.unmanage", taskID, key, c, check, func(tx *sql.Tx) (Task, error) {
		t, err := readTask(tx, taskID)
		if err != nil {
			return Task{}, err
		}
		if t.Revision != c.ExpectedRevision {
			return Task{}, &TaskConflict{Current: t}
		}
		if t.Coordination == nil || t.Coordination.AttemptID != "" {
			return Task{}, ErrTeamAssignmentConflict
		}
		tm, err := readTeam(tx, t.Coordination.TeamID)
		if err != nil {
			return Task{}, err
		}
		if _, err := tx.Exec(`UPDATE team_managed_tasks SET managed=0 WHERE task_id=?`, taskID); err != nil {
			return Task{}, err
		}
		change := TeamManagedChange{TaskID: t.ID, PreviousRevision: t.Revision, PreviousState: t.State, State: t.State, Reason: c.Reason}
		// The revision advances so a concurrent coordinator command at the old
		// revision conflicts instead of acting on a task it no longer manages.
		t.Revision++
		t.UpdatedAt = nowUTC()
		t.Coordination = nil
		change.Revision = t.Revision
		if err := saveTask(tx, t, a.Actor, false); err != nil {
			return Task{}, err
		}
		return t, managedEvent(tx, tm, nil, "team.task.unmanage", change, a)
	})
}
