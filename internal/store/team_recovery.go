package store

import (
	"database/sql"
	"encoding/json"
	"errors"
)

// Reconciliation is an operator's recorded assessment, not a hub observation
// that a process stopped. Unknown external effects must be resolved first.
type TeamRecoveryEvidence struct {
	ExecutionStopped bool      `json:"execution_stopped"`
	Reason           string    `json:"reason"`
	RuntimeCheck     string    `json:"runtime_check"`
	WorktreeCheck    string    `json:"worktree_check"`
	EvidenceRefs     []TaskRef `json:"evidence_refs"`
}

type TeamRecoveryCommand struct {
	ExpectedRevision              int64                `json:"expected_revision"`
	ExpectedWorker                TeamSessionHandle    `json:"expected_worker"`
	ExpectedSessionGeneration     int64                `json:"expected_session_generation"`
	ExpectedCoordinatorGeneration int64                `json:"expected_coordinator_generation"`
	Reconciliation                TeamRecoveryEvidence `json:"reconciliation"`
}

// The assignment retains its original worker/offer snapshots. Recovery records
// the separately observed live-session and coordinator generations at closure.
type TeamAssignmentRecovery struct {
	SessionGeneration     int64                `json:"session_generation"`
	CoordinatorGeneration int64                `json:"coordinator_generation"`
	TaskRevision          int64                `json:"task_revision"`
	CreatedAt             string               `json:"created_at"`
	Reconciliation        TeamRecoveryEvidence `json:"reconciliation"`
}

func (e TeamRecoveryEvidence) validate() error {
	if !e.ExecutionStopped {
		return invalid(errors.New("recovery requires an explicit execution_stopped affirmation after reconciliation"))
	}
	for _, s := range []string{e.Reason, e.RuntimeCheck, e.WorktreeCheck} {
		if err := taskText(s, 4096, true); err != nil {
			return invalid(err)
		}
	}
	if len(e.EvidenceRefs) == 0 {
		return invalid(errors.New("recovery requires reconciliation evidence_refs"))
	}
	if err := validateRefs("evidence_refs", e.EvidenceRefs); err != nil {
		return invalid(err)
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(b) > MaxTaskBytes {
		return invalid(errors.New("reconciliation exceeds 32 KiB; link long evidence instead"))
	}
	return nil
}

// RecoverTeamAssignment closes abandoned execution, never rebinds a token or
// resumes work. The service MUST authenticate admin/local-operator authority on
// every call, including receipt replay. An ordinary coordinator is not an admin.
func (d *DB) RecoverTeamAssignment(teamID, id string, c TeamRecoveryCommand, a TeamAuditContext, key string) (TeamAssignment, error) {
	if a.Actor.Kind != "admin" {
		return TeamAssignment{}, ErrTeamSessionDenied
	}
	if c.ExpectedRevision < 1 || !taskIDRE.MatchString(c.ExpectedWorker.SessionID) || c.ExpectedWorker.Generation < 1 || c.ExpectedSessionGeneration < 1 || c.ExpectedCoordinatorGeneration < 1 {
		return TeamAssignment{}, invalid(errors.New("positive task/session/coordinator generations and an assigned worker handle required"))
	}
	if err := c.Reconciliation.validate(); err != nil {
		return TeamAssignment{}, err
	}
	var tm Team
	check := func(tx *sql.Tx) error {
		var enabled string
		if err := tx.QueryRow(`SELECT COALESCE((SELECT value FROM meta WHERE key=?),'')`, TasksMetaKey).Scan(&enabled); err != nil {
			return err
		}
		if enabled != "on" {
			return ErrTeamSessionDenied
		}
		var err error
		tm, err = readTeam(tx, teamID)
		return err
	}
	return checkedTaskMutation(d, a.Actor, "team.assignment.recover", teamID+"/"+id, key, c, check, func(tx *sql.Tx) (TeamAssignment, error) {
		out, err := readTeamAssignment(tx, teamID, id)
		if err != nil {
			return out, err
		}
		priorTaskState := "BLOCKED"
		switch out.State {
		case "RUNNING":
			priorTaskState = "IN_PROGRESS"
		case "BLOCKED", "STOP_REQUESTED", "STOPPED":
		default:
			return out, ErrTeamAssignmentConflict
		}
		if out.Worker != c.ExpectedWorker || out.Result != nil || out.Recovery != nil {
			return out, ErrTeamAssignmentConflict
		}
		worker, err := readTeamSession(tx, teamID, out.Worker.SessionID)
		if err != nil {
			return out, err
		}
		var coordinatorGeneration int64
		if err := tx.QueryRow(`SELECT coordinator_generation FROM team_session_control WHERE team_id=?`, teamID).Scan(&coordinatorGeneration); err != nil {
			return out, err
		}
		if worker.Generation != c.ExpectedSessionGeneration || coordinatorGeneration != c.ExpectedCoordinatorGeneration {
			return out, ErrTeamSessionStale
		}
		t, err := assignmentTask(tx, out, c.ExpectedRevision, priorTaskState)
		if err != nil {
			return out, err
		}
		now := nowUTC()
		out.Recovery = &TeamAssignmentRecovery{SessionGeneration: worker.Generation, CoordinatorGeneration: coordinatorGeneration, TaskRevision: t.Revision, CreatedAt: now, Reconciliation: c.Reconciliation}
		out.State, out.Reason, out.UpdatedAt = "RECOVERED", c.Reconciliation.Reason, now
		t.State, t.Blocker = "READY", ""
		if err := t.TaskContent.validate(); err != nil {
			return out, invalid(err)
		}
		t.Revision++
		t.UpdatedAt = now
		if err := saveTask(tx, t, a.Actor, false); err != nil {
			return out, err
		}
		// The audit actor is the operator; Session describes the affected worker,
		// which may have left or lost enrollment. It does not grant that worker access.
		return out, saveTeamAssignment(tx, tm, worker, out, "team.assignment.recover", a)
	})
}
