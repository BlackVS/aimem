package store

import (
	"database/sql"
	"encoding/json"
	"errors"

	"aimem/internal/uuidv7"
)

// TeamTokenRebindEvidence is the operator's recorded assessment that the old
// credential no longer drives the session. The hub cannot observe that; it
// validates the shape and preserves the declaration with the transfer.
type TeamTokenRebindEvidence struct {
	OldCredentialStopped bool      `json:"old_credential_stopped"`
	Reason               string    `json:"reason"`
	RuntimeCheck         string    `json:"runtime_check"`
	EvidenceRefs         []TaskRef `json:"evidence_refs"`
}

// TeamTokenRebindCommand names the session as observed and the replacement
// token of the same user. The transport verifies that token against the
// access database through the authorize callback; storage never sees secrets.
type TeamTokenRebindCommand struct {
	SessionID          string                  `json:"session_id"`
	ExpectedGeneration int64                   `json:"expected_generation"`
	TokenID            string                  `json:"token_id"`
	Reconciliation     TeamTokenRebindEvidence `json:"reconciliation"`
}

// TeamTokenRebind is the immutable audit record of one credential transfer.
type TeamTokenRebind struct {
	PreviousTokenID string                  `json:"previous_token_id"`
	TokenID         string                  `json:"token_id"`
	CreatedAt       string                  `json:"created_at"`
	Reconciliation  TeamTokenRebindEvidence `json:"reconciliation"`
}

func (e TeamTokenRebindEvidence) validate() error {
	if !e.OldCredentialStopped {
		return invalid(errors.New("token replacement requires an explicit old_credential_stopped affirmation after reconciliation"))
	}
	for _, s := range []string{e.Reason, e.RuntimeCheck} {
		if err := taskText(s, 4096, true); err != nil {
			return invalid(err)
		}
	}
	if len(e.EvidenceRefs) == 0 {
		return invalid(errors.New("token replacement requires reconciliation evidence_refs"))
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

// RebindTeamSessionToken moves an active session to a replacement token of
// the same user in one transaction: the binding changes, the session
// generation advances (and the coordinator generation for a coordinator), a
// reserved non-offer attempt follows the new generation exactly as on resume,
// and one audit event records the transfer. The old credential can use no
// handle afterwards, and receipts it recorded are keyed to it and never
// replay for the new credential. The service MUST authenticate live admin
// authority on every call, including receipt replay, and the required
// authorize callback MUST check the replacement token in the access database
// (live, same user, write scope for the project) without re-entering this DB.
func (d *DB) RebindTeamSessionToken(teamID string, c TeamTokenRebindCommand, a TeamAuditContext, key string, authorize WorkerAuthority) (TeamSession, error) {
	if a.Actor.Kind != "admin" {
		return TeamSession{}, ErrTeamSessionDenied
	}
	if !taskIDRE.MatchString(c.SessionID) || c.ExpectedGeneration < 1 {
		return TeamSession{}, invalid(errors.New("session_id and positive expected_generation required"))
	}
	if err := taskText(c.TokenID, 128, true); err != nil {
		return TeamSession{}, invalid(errors.New("token_id required"))
	}
	if err := c.Reconciliation.validate(); err != nil {
		return TeamSession{}, err
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
	return checkedTaskMutation(d, a.Actor, "team.session.rebind_token", teamID+"/"+c.SessionID, key, c, check, func(tx *sql.Tx) (TeamSession, error) {
		s, err := readTeamSession(tx, teamID, c.SessionID)
		if err != nil {
			return TeamSession{}, err
		}
		if s.State != "active" || s.Generation != c.ExpectedGeneration {
			return TeamSession{}, ErrTeamSessionStale
		}
		if s.TokenID == c.TokenID {
			return TeamSession{}, invalid(errors.New("replacement token must differ from the bound token"))
		}
		// The user must still be enrolled (and designated for a coordinator), and
		// the transport must accept the replacement credential for this user.
		rebound := s
		rebound.TokenID = c.TokenID
		if _, err := requireTeamMember(tx, teamID, TaskActor{Kind: "user", Name: "rebound session", UserID: rebound.UserID, TokenID: rebound.TokenID}, s.Role == "coordinator"); err != nil {
			return TeamSession{}, err
		}
		if authorize == nil {
			return TeamSession{}, ErrTeamSessionDenied
		}
		if err := authorize(rebound); err != nil {
			return TeamSession{}, err
		}
		previous := s.Generation
		record := &TeamTokenRebind{PreviousTokenID: s.TokenID, TokenID: c.TokenID, CreatedAt: nowUTC(), Reconciliation: c.Reconciliation}
		s.TokenID = c.TokenID
		s.Generation++
		if s.Role == "coordinator" {
			if s.CoordinatorGeneration, err = nextCoordinatorGeneration(tx, teamID); err != nil {
				return TeamSession{}, err
			}
		}
		if err := putTeamSession(tx, s); err != nil {
			return TeamSession{}, err
		}
		if err := putTeamEvent(tx, teamID, TeamEvent{ID: uuidv7.New(), ProtocolVersion: 1, Operation: "team.session.rebind_token", At: record.CreatedAt, TeamAuditContext: a, Team: tm, Session: &s, PreviousGeneration: previous, TokenRebind: record}); err != nil {
			return TeamSession{}, err
		}
		if s.Role == "worker" {
			if err := rebindReservedAssignment(tx, tm, s, a); err != nil {
				return TeamSession{}, err
			}
		}
		return s, nil
	})
}
