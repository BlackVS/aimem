package store

import (
	"database/sql"
	"errors"
)

type TeamWorkCommand struct {
	TeamSessionHandle
	CoordinatorGeneration int64  `json:"coordinator_generation,omitempty"`
	ExpectedRevision      int64  `json:"expected_revision"`
	Reason                string `json:"reason"`
}

// workTransition is deliberately limited to cooperative work control. STOPPED
// still reserves the attempt until the coordinator explicitly closes it.
func workTransition(op, state string) (attemptState, taskState, priorTaskState string, err error) {
	priorTaskState = "IN_PROGRESS"
	if state == "BLOCKED" || state == "STOP_REQUESTED" || state == "STOPPED" {
		priorTaskState = "BLOCKED"
	}
	switch {
	case op == "block" && state == "RUNNING":
		return "BLOCKED", "BLOCKED", priorTaskState, nil
	case op == "resume-work" && state == "BLOCKED":
		return "RUNNING", "IN_PROGRESS", priorTaskState, nil
	case op == "cancel" && (state == "RUNNING" || state == "BLOCKED"):
		return "STOP_REQUESTED", "BLOCKED", priorTaskState, nil
	case op == "stopped" && state == "STOP_REQUESTED":
		return "STOPPED", "BLOCKED", priorTaskState, nil
	case op == "close-stop" && state == "STOPPED":
		return "CANCELLED", "READY", priorTaskState, nil
	default:
		return "", "", "", ErrTeamAssignmentConflict
	}
}

// ChangeTeamWork never controls a process or infers a stop from liveness. The
// service MUST authenticate the live ordinary caller's token and write grant
// on every call, including receipt replay. A worker acknowledgement is a recorded
// declaration; only the current coordinator can release acknowledged work.
func (d *DB) ChangeTeamWork(teamID, id, op string, c TeamWorkCommand, a TeamAuditContext, key string) (TeamAssignment, error) {
	coordinator := op == "cancel" || op == "close-stop"
	switch op {
	case "block", "resume-work", "cancel", "stopped", "close-stop":
	default:
		return TeamAssignment{}, invalid(errors.New("unsupported work operation"))
	}
	if c.ExpectedRevision < 1 || (!coordinator && c.CoordinatorGeneration != 0) || (coordinator && c.CoordinatorGeneration < 1) {
		return TeamAssignment{}, invalid(errors.New("positive expected_revision required; coordinator_generation is required only for coordinator commands"))
	}
	if err := taskText(c.Reason, 4096, true); err != nil {
		return TeamAssignment{}, invalid(err)
	}
	check := func(tx *sql.Tx) error { _, _, err := boundTeamSession(tx, teamID, c.SessionID, a.Actor); return err }
	return checkedTaskMutation(d, a.Actor, "team.assignment."+op, teamID+"/"+id, key, c, check, func(tx *sql.Tx) (TeamAssignment, error) {
		tm, s, err := currentMessageSession(tx, teamID, c.TeamSessionHandle, a.Actor)
		if err != nil {
			return TeamAssignment{}, err
		}
		out, err := readTeamAssignment(tx, teamID, id)
		if err != nil {
			return out, err
		}
		if coordinator {
			if _, _, err := currentCoordinator(tx, teamID, c.TeamSessionHandle, c.CoordinatorGeneration, a.Actor); err != nil {
				return out, err
			}
		} else if s.Role != "worker" || out.Worker != c.TeamSessionHandle {
			return out, ErrTeamSessionDenied
		}
		next, taskState, priorTaskState, err := workTransition(op, out.State)
		if err != nil {
			return out, err
		}
		t, err := assignmentTask(tx, out, c.ExpectedRevision, priorTaskState)
		if err != nil {
			return out, err
		}
		switch op {
		case "block", "cancel":
			t.Blocker = c.Reason
		case "resume-work", "close-stop":
			t.Blocker = ""
		}
		t.State = taskState
		// Adding a blocker must not turn an otherwise valid near-limit task into
		// an oversized stored task. Validate the resulting content before writing.
		if err := t.TaskContent.validate(); err != nil {
			return out, invalid(err)
		}
		now := nowUTC()
		t.Revision++
		t.UpdatedAt = now
		out.State, out.Reason, out.UpdatedAt = next, c.Reason, now
		if err := saveTask(tx, t, a.Actor, false); err != nil {
			return out, err
		}
		return out, saveTeamAssignment(tx, tm, s, out, "team.assignment."+op, a)
	})
}
