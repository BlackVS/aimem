package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"

	"aimem/internal/uuidv7"
)

// TeamHandoffReconciliation is an admin's recorded assessment that a lost
// coordinator no longer issues commands. The hub cannot observe that; it only
// validates and preserves the declaration next to the transfer.
type TeamHandoffReconciliation struct {
	CoordinatorStopped bool      `json:"coordinator_stopped"`
	LivenessCheck      string    `json:"liveness_check"`
	EvidenceRefs       []TaskRef `json:"evidence_refs"`
}

// TeamHandoffCommand names the outgoing coordinator handle and slot generation
// as observed, the successor session and a reason. Reconciliation is required
// from an admin closing a lost coordinator and refused from the coordinator.
type TeamHandoffCommand struct {
	TeamSessionHandle
	CoordinatorGeneration int64                      `json:"coordinator_generation"`
	Target                TeamSessionHandle          `json:"target"`
	Reason                string                     `json:"reason"`
	Reconciliation        *TeamHandoffReconciliation `json:"reconciliation,omitempty"`
}

// TeamCoordinatorHandoff is the immutable audit record of one transfer.
type TeamCoordinatorHandoff struct {
	From                          TeamSessionHandle          `json:"from"`
	To                            TeamSessionHandle          `json:"to"`
	PreviousCoordinatorGeneration int64                      `json:"previous_coordinator_generation"`
	CoordinatorGeneration         int64                      `json:"coordinator_generation"`
	Reason                        string                     `json:"reason"`
	CreatedAt                     string                     `json:"created_at"`
	Reconciliation                *TeamHandoffReconciliation `json:"reconciliation,omitempty"`
}

func (r TeamHandoffReconciliation) validate() error {
	if !r.CoordinatorStopped {
		return invalid(errors.New("admin handoff requires an explicit coordinator_stopped affirmation after reconciliation"))
	}
	if err := taskText(r.LivenessCheck, 4096, true); err != nil {
		return invalid(err)
	}
	if len(r.EvidenceRefs) == 0 {
		return invalid(errors.New("admin handoff requires reconciliation evidence_refs"))
	}
	if err := validateRefs("evidence_refs", r.EvidenceRefs); err != nil {
		return invalid(err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > MaxTaskBytes {
		return invalid(errors.New("reconciliation exceeds 32 KiB; link long evidence instead"))
	}
	return nil
}

// HandoffTeamCoordinator transfers the single coordinator slot in one
// transaction: the outgoing session closes with an advanced generation, the
// successor becomes coordinator under the next slot generation, and one audit
// event records both handles. Worker attempts are untouched; their issuing
// generation stays audit data and new coordinator commands use the new one.
// The current coordinator calls it with its own bound handle. An admin calls
// it for a coordinator that was lost, with recorded reconciliation; the
// service MUST authenticate live admin authority on every call, including
// receipt replay. Nothing here stops a lost coordinator's local process.
func (d *DB) HandoffTeamCoordinator(teamID string, c TeamHandoffCommand, a TeamAuditContext, key string) (TeamSession, error) {
	admin := a.Actor.Kind == "admin"
	if c.CoordinatorGeneration < 1 || c.Generation < 1 || c.Target.Generation < 1 || !taskIDRE.MatchString(c.SessionID) || !taskIDRE.MatchString(c.Target.SessionID) {
		return TeamSession{}, invalid(errors.New("outgoing coordinator handle, coordinator_generation and target handle required"))
	}
	if c.SessionID == c.Target.SessionID {
		return TeamSession{}, invalid(errors.New("target must be a different session"))
	}
	if err := taskText(c.Reason, 4096, true); err != nil {
		return TeamSession{}, invalid(err)
	}
	if admin != (c.Reconciliation != nil) {
		return TeamSession{}, invalid(errors.New("reconciliation is required from an admin and refused from the coordinator"))
	}
	if admin {
		if err := c.Reconciliation.validate(); err != nil {
			return TeamSession{}, err
		}
	}
	var tm Team
	check := func(tx *sql.Tx) (err error) {
		if !admin {
			tm, _, err = boundTeamSession(tx, teamID, c.SessionID, a.Actor)
			return err
		}
		var enabled string
		if err := tx.QueryRow(`SELECT COALESCE((SELECT value FROM meta WHERE key=?),'')`, TasksMetaKey).Scan(&enabled); err != nil {
			return err
		}
		if enabled != "on" {
			return ErrTeamSessionDenied
		}
		tm, err = readTeam(tx, teamID)
		return err
	}
	return checkedTaskMutation(d, a.Actor, "team.coordinator.handoff", teamID, key, c, check, func(tx *sql.Tx) (TeamSession, error) {
		from, err := outgoingCoordinator(tx, tm, c, a.Actor, admin)
		if err != nil {
			return TeamSession{}, err
		}
		to, err := readTeamSession(tx, teamID, c.Target.SessionID)
		if err != nil {
			return TeamSession{}, err
		}
		if to.State != "active" || to.Generation != c.Target.Generation {
			return TeamSession{}, ErrTeamSessionStale
		}
		// Designation is checked for the successor's own user; the transfer grants
		// nothing a join as coordinator would not have granted that user.
		if _, err := requireTeamMember(tx, teamID, TaskActor{Kind: "user", Name: "handoff target", UserID: to.UserID, TokenID: to.TokenID}, true); err != nil {
			return TeamSession{}, err
		}
		var reserved bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM team_assignments WHERE worker_id=? AND reserved=1)`, to.ID).Scan(&reserved); err != nil {
			return TeamSession{}, err
		}
		if reserved {
			return TeamSession{}, ErrTeamAssignmentConflict
		}
		// Neither session acted through this command, so liveness is untouched:
		// a transfer must not make a suspect successor look alive. The outgoing
		// row closes before the successor takes the role, or the unique
		// active-coordinator index rejects the second write.
		from.State, from.Availability, from.Generation = "left", "unavailable", from.Generation+1
		if err := putTeamSession(tx, from); err != nil {
			return TeamSession{}, err
		}
		next, err := nextCoordinatorGeneration(tx, teamID)
		if err != nil {
			return TeamSession{}, err
		}
		to.Role, to.CoordinatorGeneration = "coordinator", next
		if err := putTeamSession(tx, to); err != nil {
			return TeamSession{}, err
		}
		now := nowUTC()
		h := &TeamCoordinatorHandoff{From: c.TeamSessionHandle, To: c.Target, PreviousCoordinatorGeneration: c.CoordinatorGeneration, CoordinatorGeneration: next, Reason: c.Reason, CreatedAt: now, Reconciliation: c.Reconciliation}
		// Every remaining member learns the new coordinator generation from its
		// inbox; the outgoing session is already closed and receives nothing.
		recipients, err := messageRecipients(tx, tm, TeamRecipient{Kind: "team"})
		if err != nil {
			return TeamSession{}, err
		}
		l := TeamLifecycle{Operation: "team.coordinator.handoff", State: "coordinator", ActorKind: a.Actor.Kind, Session: &c.Target, CoordinatorGeneration: next}
		msg, err := lifecycleMessage(tx, teamID, TeamRecipient{Kind: "team"}, recipients, l, "team.coordinator.handoff: session "+to.ID+" is coordinator at generation "+strconv.FormatInt(next, 10)+"; reason: "+c.Reason, nil)
		if err != nil {
			return TeamSession{}, err
		}
		return to, putTeamEvent(tx, teamID, TeamEvent{ID: uuidv7.New(), ProtocolVersion: 1, Operation: "team.coordinator.handoff", At: now, TeamAuditContext: a, Team: tm, Session: &to, Handoff: h, MessageIDs: []string{msg}})
	})
}

// outgoingCoordinator fences the transfer on the slot as observed: the current
// coordinator proves its own bound live handle, while an admin names the lost
// session's exact handle and the current slot generation without owning it.
func outgoingCoordinator(tx *sql.Tx, tm Team, c TeamHandoffCommand, actor TaskActor, admin bool) (TeamSession, error) {
	if !admin {
		_, from, err := currentCoordinator(tx, tm.ID, c.TeamSessionHandle, c.CoordinatorGeneration, actor)
		return from, err
	}
	from, err := readTeamSession(tx, tm.ID, c.SessionID)
	if err != nil {
		return from, err
	}
	var current int64
	if err := tx.QueryRow(`SELECT coordinator_generation FROM team_session_control WHERE team_id=?`, tm.ID).Scan(&current); err != nil {
		return from, err
	}
	if from.Role != "coordinator" || from.State != "active" || from.Generation != c.Generation || from.CoordinatorGeneration != c.CoordinatorGeneration || current != c.CoordinatorGeneration {
		return from, ErrTeamSessionStale
	}
	return from, nil
}
