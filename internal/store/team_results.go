package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"

	"aimem/internal/uuidv7"
)

// Full Git object IDs pin evidence; the store does not fetch repositories or
// attest that a commit exists, contains the change, or passed its stated checks.
var teamCommitRE = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type TeamResultContent struct {
	BaseCommit   string    `json:"base_commit"`
	Commit       string    `json:"commit"`
	Summary      string    `json:"summary"`
	Validation   string    `json:"validation"`
	EvidenceRefs []TaskRef `json:"evidence_refs"`
}

// One immutable result belongs to one attempt. Corrections require a new
// offer, not an edit of the prior evidence. The profile records declarations
// at submission; the assignment separately retains its original offer profile.
type TeamResult struct {
	ID              string            `json:"id"`
	AttemptID       string            `json:"attempt_id"`
	Worker          TeamSessionHandle `json:"worker"`
	TaskRevision    int64             `json:"task_revision"`
	ProfileRevision int64             `json:"profile_revision"`
	Profile         TeamProfile       `json:"profile"`
	CreatedAt       string            `json:"created_at"`
	TeamResultContent
}

type TeamResultSubmission struct {
	TeamSessionHandle
	ExpectedRevision int64 `json:"expected_revision"`
	TeamResultContent
}

type TeamResultDecision struct {
	TeamSessionHandle
	CoordinatorGeneration int64  `json:"coordinator_generation"`
	ExpectedRevision      int64  `json:"expected_revision"`
	ResultID              string `json:"result_id"`
	Decision              string `json:"decision"` // accept or rework
	Reason                string `json:"reason"`
}

type TeamResultReview struct {
	ResultID              string            `json:"result_id"`
	Decision              string            `json:"decision"`
	Reason                string            `json:"reason"`
	Coordinator           TeamSessionHandle `json:"coordinator"`
	CoordinatorGeneration int64             `json:"coordinator_generation"`
	CreatedAt             string            `json:"created_at"`
}

func (c TeamResultContent) validate() error {
	if !teamCommitRE.MatchString(c.BaseCommit) || !teamCommitRE.MatchString(c.Commit) {
		return invalid(errors.New("base_commit and commit require full lowercase Git object IDs"))
	}
	for _, s := range []string{c.Summary, c.Validation} {
		if err := taskText(s, MaxTaskBytes, true); err != nil {
			return invalid(err)
		}
	}
	if len(c.EvidenceRefs) == 0 {
		return invalid(errors.New("result requires evidence_refs"))
	}
	if err := validateRefs("evidence_refs", c.EvidenceRefs); err != nil {
		return invalid(err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if len(b) > MaxTaskBytes {
		return invalid(errors.New("result content exceeds 32 KiB; link long evidence instead"))
	}
	return nil
}

// resultTask fences the task side of a transition as well as the attempt side.
// It must run in the same transaction that saves the task and assignment.
func resultTask(tx *sql.Tx, out TeamAssignment, revision int64, state string) (Task, error) {
	t, err := readTask(tx, out.TaskID)
	if err != nil {
		return t, err
	}
	if t.Revision != revision {
		return t, &TaskConflict{Current: t}
	}
	if t.Archived || t.State != state || t.Coordination == nil || t.Coordination.TeamID != out.TeamID || t.Coordination.AttemptID != out.ID {
		return t, ErrTeamAssignmentConflict
	}
	return t, nil
}

// SubmitTeamResult accepts only the current assigned worker. The service MUST
// authenticate the caller's live ordinary write token/grant before every call,
// including receipt replay. Profiles and handles never authenticate a caller.
func (d *DB) SubmitTeamResult(teamID, id string, c TeamResultSubmission, a TeamAuditContext, key string) (TeamAssignment, error) {
	if c.ExpectedRevision < 1 {
		return TeamAssignment{}, invalid(errors.New("positive expected_revision required"))
	}
	if err := c.TeamResultContent.validate(); err != nil {
		return TeamAssignment{}, err
	}
	check := func(tx *sql.Tx) error { _, _, err := boundTeamSession(tx, teamID, c.SessionID, a.Actor); return err }
	return checkedTaskMutation(d, a.Actor, "team.assignment.submit", teamID+"/"+id, key, c, check, func(tx *sql.Tx) (TeamAssignment, error) {
		tm, worker, err := currentMessageSession(tx, teamID, c.TeamSessionHandle, a.Actor)
		if err != nil {
			return TeamAssignment{}, err
		}
		out, err := readTeamAssignment(tx, teamID, id)
		if err != nil {
			return out, err
		}
		if worker.Role != "worker" || out.Worker != c.TeamSessionHandle {
			return out, ErrTeamSessionDenied
		}
		if out.State != "RUNNING" || out.Result != nil {
			return out, ErrTeamAssignmentConflict
		}
		t, err := resultTask(tx, out, c.ExpectedRevision, "IN_PROGRESS")
		if err != nil {
			return out, err
		}
		now := nowUTC()
		out.Result = &TeamResult{ID: uuidv7.New(), AttemptID: out.ID, Worker: c.TeamSessionHandle, TaskRevision: t.Revision, ProfileRevision: worker.ProfileRevision, Profile: worker.TeamProfile, CreatedAt: now, TeamResultContent: c.TeamResultContent}
		out.State, out.UpdatedAt = "SUBMITTED", now
		t.State, t.UpdatedAt = "REVIEW", now
		t.Revision++
		if err := saveTask(tx, t, a.Actor, false); err != nil {
			return out, err
		}
		return out, saveTeamAssignment(tx, tm, worker, out, "team.assignment.submit", a)
	})
}

// ReviewTeamResult closes a submitted attempt, never marks a task DONE. A
// coordinator acceptance leaves REVIEW for the separate human delivery gates.
// Live caller authorization is required by the service, as for submission.
func (d *DB) ReviewTeamResult(teamID, id string, c TeamResultDecision, a TeamAuditContext, key string) (TeamAssignment, error) {
	if c.ExpectedRevision < 1 || !taskIDRE.MatchString(c.ResultID) || (c.Decision != "accept" && c.Decision != "rework") {
		return TeamAssignment{}, invalid(errors.New("positive expected_revision, result_id and decision accept or rework required"))
	}
	if err := taskText(c.Reason, 4096, true); err != nil {
		return TeamAssignment{}, invalid(err)
	}
	check := func(tx *sql.Tx) error { _, _, err := boundTeamSession(tx, teamID, c.SessionID, a.Actor); return err }
	return checkedTaskMutation(d, a.Actor, "team.assignment.review", teamID+"/"+id, key, c, check, func(tx *sql.Tx) (TeamAssignment, error) {
		tm, coordinator, err := currentCoordinator(tx, teamID, c.TeamSessionHandle, c.CoordinatorGeneration, a.Actor)
		if err != nil {
			return TeamAssignment{}, err
		}
		out, err := readTeamAssignment(tx, teamID, id)
		if err != nil {
			return out, err
		}
		if out.State != "SUBMITTED" || out.Result == nil || out.Result.ID != c.ResultID || out.Review != nil {
			return out, ErrTeamAssignmentConflict
		}
		t, err := resultTask(tx, out, c.ExpectedRevision, "REVIEW")
		if err != nil {
			return out, err
		}
		now := nowUTC()
		out.Review = &TeamResultReview{ResultID: c.ResultID, Decision: c.Decision, Reason: c.Reason, Coordinator: c.TeamSessionHandle, CoordinatorGeneration: c.CoordinatorGeneration, CreatedAt: now}
		out.State = "ACCEPTED"
		if c.Decision == "rework" {
			out.State, t.State = "RETURNED", "READY"
		}
		out.UpdatedAt, t.UpdatedAt = now, now
		t.Revision++
		if err := saveTask(tx, t, a.Actor, false); err != nil {
			return out, err
		}
		return out, saveTeamAssignment(tx, tm, coordinator, out, "team.assignment.review", a)
	})
}
