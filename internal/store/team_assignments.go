package store

import (
	"database/sql"
	"encoding/json"
	"errors"

	"aimem/internal/uuidv7"
)

var (
	ErrManagedTask            = errors.New("managed_task: use team coordination operations; generic task update/archive is unavailable")
	ErrTeamAssignmentConflict = errors.New("assignment state, task readiness or worker capacity conflict")
	ErrTeamAssignmentNotFound = errors.New("team assignment not found")
)

// TaskCoordination is a derived read-only projection, never task content.
type TaskCoordination struct {
	TeamID    string `json:"team_id"`
	AttemptID string `json:"attempt_id,omitempty"`
	State     string `json:"state,omitempty"`
}

type TeamOffer struct {
	TeamSessionHandle
	CoordinatorGeneration int64             `json:"coordinator_generation"`
	TaskID                string            `json:"task_id"`
	ExpectedRevision      int64             `json:"expected_revision"`
	Worker                TeamSessionHandle `json:"worker"`
	SuitabilityRationale  string            `json:"suitability_rationale"`
	CostRationale         string            `json:"cost_rationale"`
}

// The requirements and profile are immutable snapshots of the accepted offer.
// Profile declarations and the coordinator's rationale do not grant authority.
type TeamAssignment struct {
	ID                    string                  `json:"id"`
	TeamID                string                  `json:"team_id"`
	TaskID                string                  `json:"task_id"`
	State                 string                  `json:"state"`
	Coordinator           TeamSessionHandle       `json:"coordinator"`
	CoordinatorGeneration int64                   `json:"coordinator_generation"`
	Worker                TeamSessionHandle       `json:"worker"`
	ProfileRevision       int64                   `json:"profile_revision"`
	Profile               TeamProfile             `json:"profile"`
	TaskRevision          int64                   `json:"task_revision"`
	Requirements          TaskContent             `json:"requirements"`
	SuitabilityRationale  string                  `json:"suitability_rationale"`
	CostRationale         string                  `json:"cost_rationale"`
	Reason                string                  `json:"reason,omitempty"`
	CreatedAt             string                  `json:"created_at"`
	UpdatedAt             string                  `json:"updated_at"`
	Result                *TeamResult             `json:"result,omitempty"`
	Review                *TeamResultReview       `json:"review,omitempty"`
	Recovery              *TeamAssignmentRecovery `json:"recovery,omitempty"`
	Rebinds               []TeamAssignmentRebind  `json:"rebinds,omitempty"`
	RebindCount           int64                   `json:"rebind_count,omitempty"`
}

// TeamAssignmentRebind records one worker session resume that moved the
// attempt's worker handle to the new session generation. The worker identity,
// offer and result snapshots never change. The assignment keeps the most recent
// MaxTeamRebinds records; every rebind also has its own audit event.
type TeamAssignmentRebind struct {
	PreviousGeneration int64  `json:"previous_generation"`
	Generation         int64  `json:"generation"`
	CreatedAt          string `json:"created_at"`
}

const MaxTeamRebinds = 32

type TeamAssignmentCommand struct {
	TeamSessionHandle
	CoordinatorGeneration int64  `json:"coordinator_generation,omitempty"`
	Reason                string `json:"reason,omitempty"`
}

// WorkerAuthority MUST check the worker's live token, user, project scope and
// write grant in the access service. It runs inside the project transaction;
// it must not re-enter this DB. The caller also authenticates its own live token
// on every request, including receipt replay. Neither is inferred from a profile.
type WorkerAuthority func(TeamSession) error

func currentCoordinator(tx *sql.Tx, teamID string, h TeamSessionHandle, generation int64, a TaskActor) (Team, TeamSession, error) {
	t, s, err := currentMessageSession(tx, teamID, h, a)
	if err != nil {
		return t, s, err
	}
	if s.Role != "coordinator" || generation < 1 || s.CoordinatorGeneration != generation {
		return t, s, ErrTeamSessionStale
	}
	var current int64
	if err := tx.QueryRow(`SELECT coordinator_generation FROM team_session_control WHERE team_id=?`, teamID).Scan(&current); err != nil {
		return t, s, err
	}
	if current != generation {
		return t, s, ErrTeamSessionStale
	}
	return t, s, nil
}

func assignmentReady(tx *sql.Tx, id string, revision int64) (Task, error) {
	t, err := readTask(tx, id)
	if err != nil {
		return t, err
	}
	if err := rejectActiveReservation(tx, id); err != nil {
		return t, err
	}
	if t.Revision != revision {
		return t, &TaskConflict{Current: t}
	}
	if t.State != "READY" || t.Archived {
		return t, ErrTeamAssignmentConflict
	}
	for _, id := range t.Dependencies {
		dep, err := readTask(tx, id)
		if err != nil {
			return t, err
		}
		if dep.State != "DONE" {
			return t, ErrTeamAssignmentConflict
		}
	}
	return t, nil
}

func assignmentWorker(tx *sql.Tx, teamID string, h TeamSessionHandle, authorize WorkerAuthority) (TeamSession, error) {
	s, err := readTeamSession(tx, teamID, h.SessionID)
	if err != nil {
		return s, err
	}
	if s.Role != "worker" || s.State != "active" || s.Generation != h.Generation || h.Generation < 1 || s.Availability != "available" {
		return s, ErrTeamAssignmentConflict
	}
	if _, err := requireTeamMember(tx, teamID, TaskActor{Kind: "user", Name: "assignment worker", UserID: s.UserID, TokenID: s.TokenID}, false); err != nil {
		return s, err
	}
	if authorize == nil {
		return s, ErrTeamSessionDenied
	}
	return s, authorize(s)
}

func (d *DB) OfferTeamAssignment(teamID string, c TeamOffer, a TeamAuditContext, key string, authorize WorkerAuthority) (TeamAssignment, error) {
	if !taskIDRE.MatchString(c.TaskID) || c.ExpectedRevision < 1 {
		return TeamAssignment{}, invalid(errors.New("task ID and positive expected_revision required"))
	}
	for _, v := range []string{c.SuitabilityRationale, c.CostRationale} {
		if err := taskText(v, 4096, true); err != nil {
			return TeamAssignment{}, invalid(err)
		}
	}
	check := func(tx *sql.Tx) error {
		if _, _, err := boundTeamSession(tx, teamID, c.SessionID, a.Actor); err != nil {
			return err
		}
		return rejectActiveReservation(tx, c.TaskID)
	}
	return checkedTaskMutation(d, a.Actor, "team.assignment.offer", teamID, key, c, check, func(tx *sql.Tx) (TeamAssignment, error) {
		tm, coordinator, err := currentCoordinator(tx, teamID, c.TeamSessionHandle, c.CoordinatorGeneration, a.Actor)
		if err != nil {
			return TeamAssignment{}, err
		}
		t, err := assignmentReady(tx, c.TaskID, c.ExpectedRevision)
		if err != nil {
			return TeamAssignment{}, err
		}
		if t.Coordination != nil && (t.Coordination.TeamID != teamID || t.Coordination.AttemptID != "") {
			return TeamAssignment{}, ErrTeamAssignmentConflict
		}
		worker, err := assignmentWorker(tx, teamID, c.Worker, authorize)
		if err != nil {
			return TeamAssignment{}, err
		}
		var busy bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM team_assignments WHERE worker_id=? AND reserved=1)`, worker.ID).Scan(&busy); err != nil {
			return TeamAssignment{}, err
		}
		if busy {
			return TeamAssignment{}, ErrTeamAssignmentConflict
		}
		now := nowUTC()
		out := TeamAssignment{ID: uuidv7.New(), TeamID: teamID, TaskID: t.ID, State: "OFFERED", Coordinator: c.TeamSessionHandle, CoordinatorGeneration: c.CoordinatorGeneration, Worker: c.Worker, ProfileRevision: worker.ProfileRevision, Profile: worker.TeamProfile, TaskRevision: t.Revision, Requirements: t.TaskContent, SuitabilityRationale: c.SuitabilityRationale, CostRationale: c.CostRationale, CreatedAt: now, UpdatedAt: now}
		// A released task (managed=0) is taken over by the offering team; the
		// projection check above already refused a task another team still manages.
		if _, err := tx.Exec(`INSERT INTO team_managed_tasks(task_id,team_id,managed) VALUES(?,?,1) ON CONFLICT(task_id) DO UPDATE SET team_id=excluded.team_id,managed=1`, t.ID, teamID); err != nil {
			return out, err
		}
		return out, saveTeamAssignment(tx, tm, coordinator, out, "team.assignment.offer", a)
	})
}

func readTeamAssignment(q rowQuerier, teamID, id string) (TeamAssignment, error) {
	var out TeamAssignment
	var body string
	if err := q.QueryRow(`SELECT body FROM team_assignments WHERE team_id=? AND id=?`, teamID, id).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, ErrTeamAssignmentNotFound
		}
		return out, err
	}
	err := json.Unmarshal([]byte(body), &out)
	return out, err
}

// ChangeTeamAssignment closes only unaccepted offers. RUNNING reservations
// survive missing heartbeat, resume and leave; recovery is a later operation.
func (d *DB) ChangeTeamAssignment(teamID, id, op string, c TeamAssignmentCommand, a TeamAuditContext, key string, authorize WorkerAuthority) (TeamAssignment, error) {
	switch op {
	case "accept":
		if c.Reason != "" || c.CoordinatorGeneration != 0 {
			return TeamAssignment{}, invalid(errors.New("accept requires only the worker session handle"))
		}
	case "decline", "withdraw":
		if op == "decline" && c.CoordinatorGeneration != 0 {
			return TeamAssignment{}, invalid(errors.New("decline requires only the worker handle and reason"))
		}
		if err := taskText(c.Reason, 4096, true); err != nil {
			return TeamAssignment{}, invalid(err)
		}
	default:
		return TeamAssignment{}, invalid(errors.New("unsupported assignment operation"))
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
		if out.State != "OFFERED" {
			return out, ErrTeamAssignmentConflict
		}
		if op == "withdraw" {
			if _, _, err := currentCoordinator(tx, teamID, c.TeamSessionHandle, c.CoordinatorGeneration, a.Actor); err != nil {
				return out, err
			}
		} else if out.Worker != c.TeamSessionHandle || s.Role != "worker" {
			return out, ErrTeamSessionDenied
		}
		switch op {
		case "accept":
			if _, err := assignmentWorker(tx, teamID, out.Worker, authorize); err != nil {
				return out, err
			}
			t, err := assignmentReady(tx, out.TaskID, out.TaskRevision)
			if err != nil {
				return out, err
			}
			t.State, t.UpdatedAt = "IN_PROGRESS", nowUTC()
			t.Revision++
			if err := saveTask(tx, t, a.Actor, false); err != nil {
				return out, err
			}
			out.State = "RUNNING"
		case "decline":
			out.State = "DECLINED"
		case "withdraw":
			out.State = "WITHDRAWN"
		}
		out.Reason, out.UpdatedAt = c.Reason, nowUTC()
		return out, saveTeamAssignment(tx, tm, s, out, "team.assignment."+op, a)
	})
}

// saveTeamAssignment writes the attempt row, a hub-authored lifecycle message
// for the counterpart sessions (the attempt's worker and the current
// coordinator, minus the session that acted) and one audit event naming the
// message, all in the caller's transaction. Session s is the acting session
// for user commands and the affected worker for operator recovery.
func saveTeamAssignment(tx *sql.Tx, t Team, s TeamSession, out TeamAssignment, op string, a TeamAuditContext) error {
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	reserved := out.State == "OFFERED" || out.State == "RUNNING" || out.State == "SUBMITTED" || out.State == "BLOCKED" || out.State == "STOP_REQUESTED" || out.State == "STOPPED"
	_, err = tx.Exec(`INSERT INTO team_assignments(id,team_id,task_id,worker_id,state,reserved,body) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET state=excluded.state,reserved=excluded.reserved,body=excluded.body`, out.ID, out.TeamID, out.TaskID, out.Worker.SessionID, out.State, boolInt(reserved), string(b))
	if err != nil {
		return err
	}
	task, err := readTask(tx, out.TaskID)
	if err != nil {
		return err
	}
	coordinator, err := activeCoordinatorID(tx, t.ID)
	if err != nil {
		return err
	}
	exclude := ""
	l := TeamLifecycle{Operation: op, TaskID: out.TaskID, AttemptID: out.ID, State: out.State, TaskState: task.State, ActorKind: a.Actor.Kind}
	if a.Actor.Kind == "user" {
		exclude = s.ID
		l.Session = &TeamSessionHandle{SessionID: s.ID, Generation: s.Generation}
	}
	recipients, err := lifecycleRecipients(tx, t, exclude, out.Worker.SessionID, coordinator)
	if err != nil {
		return err
	}
	text := op + ": attempt " + out.ID + " is " + out.State + "; task " + out.TaskID + " is " + task.State
	if out.Reason != "" {
		text += "; reason: " + out.Reason
	}
	msg, err := lifecycleMessage(tx, t.ID, TeamRecipient{Kind: "participants"}, recipients, l, text, []TaskRef{{Kind: "task", Ref: out.TaskID}})
	if err != nil {
		return err
	}
	e := TeamEvent{ID: uuidv7.New(), ProtocolVersion: 1, Operation: op, At: nowUTC(), TeamAuditContext: a, Team: t, Session: &s, Assignment: &out, MessageIDs: []string{msg}}
	b, err = json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO team_events(team_id,body) VALUES(?,?)`, t.ID, string(b))
	return err
}

// GetTeamAssignment validates the session in the same snapshot as the read.
// Live caller token/project authority is the service's responsibility.
func (d *DB) GetTeamAssignment(teamID, id string, h TeamSessionHandle, actor TaskActor) (TeamAssignment, error) {
	if err := d.taskScopeOK(); err != nil {
		return TeamAssignment{}, err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return TeamAssignment{}, err
	}
	defer tx.Rollback()
	if _, _, err := currentMessageSession(tx, teamID, h, actor); err != nil {
		return TeamAssignment{}, err
	}
	return readTeamAssignment(tx, teamID, id)
}

// ReservedTeamAssignment returns the attempt currently reserved for the calling
// session, validated in the same snapshot: the outstanding-work view a resumed
// worker reads before reconciling local state. It grants nothing, releases
// nothing and reports not-found when the session holds no reservation.
// Live caller token/project authority is the service's responsibility.
func (d *DB) ReservedTeamAssignment(teamID string, h TeamSessionHandle, actor TaskActor) (TeamAssignment, error) {
	if err := d.taskScopeOK(); err != nil {
		return TeamAssignment{}, err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return TeamAssignment{}, err
	}
	defer tx.Rollback()
	if _, _, err := currentMessageSession(tx, teamID, h, actor); err != nil {
		return TeamAssignment{}, err
	}
	var id string
	if err := tx.QueryRow(`SELECT id FROM team_assignments WHERE team_id=? AND worker_id=? AND reserved=1`, teamID, h.SessionID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TeamAssignment{}, ErrTeamAssignmentNotFound
		}
		return TeamAssignment{}, err
	}
	return readTeamAssignment(tx, teamID, id)
}

// rebindReservedAssignment moves a resumed worker's reserved attempt to the new
// session generation inside the resume transaction, so the returning client
// commands its own work with the handle resume returned. An OFFERED attempt is
// left bound to the generation that received it: the coordinator withdraws and
// offers again. Rebinding changes no task content, result or process state and
// does not release the reservation; it cannot stop a surviving local command.
func rebindReservedAssignment(tx *sql.Tx, t Team, s TeamSession, audit TeamAuditContext) error {
	var id string
	err := tx.QueryRow(`SELECT id FROM team_assignments WHERE team_id=? AND worker_id=? AND reserved=1 AND state<>'OFFERED'`, t.ID, s.ID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	out, err := readTeamAssignment(tx, t.ID, id)
	if err != nil {
		return err
	}
	now := nowUTC()
	out.Rebinds = append(out.Rebinds, TeamAssignmentRebind{PreviousGeneration: out.Worker.Generation, Generation: s.Generation, CreatedAt: now})
	if len(out.Rebinds) > MaxTeamRebinds {
		out.Rebinds = out.Rebinds[len(out.Rebinds)-MaxTeamRebinds:]
	}
	out.RebindCount++
	out.Worker.Generation, out.UpdatedAt = s.Generation, now
	return saveTeamAssignment(tx, t, s, out, "team.assignment.rebind", audit)
}
