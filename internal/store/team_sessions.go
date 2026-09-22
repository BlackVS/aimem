package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"aimem/internal/uuidv7"
)

var (
	ErrTeamSessionDenied       = errors.New("team session access denied")
	ErrTeamSessionStale        = errors.New("team session is closed or generation is stale")
	ErrTeamCoordinatorOccupied = errors.New("team coordinator slot is occupied")
	ErrTeamSessionQuota        = errors.New("team active session limit reached")
)

const MaxTeamSessions = 100

// Profiles are declarations, never an authorization input or proof of ability.
type TeamModel struct {
	Provider   string `json:"provider"`
	ID         string `json:"id"`
	Version    string `json:"version"`
	Source     string `json:"source"`
	ObservedAt string `json:"observed_at"`
}

type TeamProfile struct {
	Label           string    `json:"label"`
	Platform        string    `json:"platform"`
	PlatformVersion string    `json:"platform_version"`
	Model           TeamModel `json:"model"`
	Capabilities    []string  `json:"capabilities"`
}

type TeamSession struct {
	ID                    string `json:"id"`
	TeamID                string `json:"team_id"`
	UserID                string `json:"user_id"`
	TokenID               string `json:"token_id"`
	Generation            int64  `json:"generation"`
	CoordinatorGeneration int64  `json:"coordinator_generation,omitempty"`
	Role                  string `json:"role"`
	State                 string `json:"state"`
	Availability          string `json:"availability"`
	ProfileRevision       int64  `json:"profile_revision"`
	TeamProfile
	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at"`
}

// Suspect is a read-time liveness observation. It never frees a coordinator
// slot or changes ownership. The caller supplies hub time and a policy timeout.
func (s TeamSession) Suspect(now time.Time, timeout time.Duration) bool {
	last, err := time.Parse(time.RFC3339Nano, s.LastSeenAt)
	return s.State == "active" && (err != nil || timeout <= 0 || !now.Before(last.Add(timeout)))
}

func (p *TeamProfile) validate() error {
	for name, value := range map[string]string{"label": p.Label, "platform": p.Platform, "platform_version": p.PlatformVersion} {
		if err := taskText(value, 128, true); err != nil {
			return invalid(fmt.Errorf("%s: %w", name, err))
		}
	}
	if p.Model.Source == "" {
		p.Model.Source = "unknown"
	}
	switch p.Model.Source {
	case "runtime_reported", "operator_configured", "agent_reported", "unknown":
	default:
		return invalid(errors.New("invalid model source"))
	}
	for _, v := range []*string{&p.Model.Provider, &p.Model.ID, &p.Model.Version} {
		if *v == "" {
			*v = "unknown"
		}
		if err := taskText(*v, 256, true); err != nil {
			return invalid(err)
		}
	}
	if p.Model.ObservedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, p.Model.ObservedAt); err != nil {
			return invalid(errors.New("invalid model observation time"))
		}
	}
	if len(p.Capabilities) > 32 {
		return invalid(errors.New("at most 32 capabilities"))
	}
	for _, v := range p.Capabilities {
		if err := taskText(v, 128, true); err != nil {
			return invalid(err)
		}
	}
	if p.Capabilities == nil {
		p.Capabilities = []string{}
	}
	return nil
}

// requireTeamMember checks project-local authority inside the transaction.
// The service MUST first authenticate a live ordinary WRITE token and current
// project grant. This store does not authenticate tokens in the access database.
func requireTeamMember(tx *sql.Tx, teamID string, actor TaskActor, coordinator bool) (Team, error) {
	if actor.Kind != "user" {
		return Team{}, ErrTeamSessionDenied
	}
	if _, err := actor.key(); err != nil {
		return Team{}, ErrTeamSessionDenied
	}
	var enabled string
	if err := tx.QueryRow(`SELECT COALESCE((SELECT value FROM meta WHERE key=?),'')`, TasksMetaKey).Scan(&enabled); err != nil {
		return Team{}, err
	}
	if enabled != "on" {
		return Team{}, ErrTeamSessionDenied
	}
	t, err := readTeam(tx, teamID)
	if err != nil {
		return Team{}, err
	}
	for _, e := range t.Enrollment {
		if e.UserID == actor.UserID && (!coordinator || e.Coordinator) {
			return t, nil
		}
	}
	return Team{}, ErrTeamSessionDenied
}

func readTeamSession(q rowQuerier, teamID, id string) (TeamSession, error) {
	var s TeamSession
	var body string
	if err := q.QueryRow(`SELECT body FROM team_sessions WHERE team_id=? AND id=?`, teamID, id).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return s, ErrTeamSessionDenied
		}
		return s, err
	}
	err := json.Unmarshal([]byte(body), &s)
	return s, err
}

func boundTeamSession(tx *sql.Tx, teamID, id string, a TaskActor) (Team, TeamSession, error) {
	s, err := readTeamSession(tx, teamID, id)
	if err != nil {
		return Team{}, s, err
	}
	if s.UserID != a.UserID || s.TokenID != a.TokenID {
		return Team{}, s, ErrTeamSessionDenied
	}
	t, err := requireTeamMember(tx, teamID, a, s.Role == "coordinator")
	return t, s, err
}

func nextCoordinatorGeneration(tx *sql.Tx, teamID string) (int64, error) {
	var n int64
	err := tx.QueryRow(`INSERT INTO team_session_control(team_id,coordinator_generation) VALUES(?,1)
ON CONFLICT(team_id) DO UPDATE SET coordinator_generation=coordinator_generation+1 RETURNING coordinator_generation`, teamID).Scan(&n)
	return n, err
}

func saveTeamSession(tx *sql.Tx, t Team, s TeamSession, previous int64, operation string, audit TeamAuditContext) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO team_sessions(id,team_id,role,state,body) VALUES(?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET state=excluded.state,body=excluded.body`, s.ID, s.TeamID, s.Role, s.State, string(body))
	if err != nil {
		return err
	}
	e := TeamEvent{ID: uuidv7.New(), ProtocolVersion: 1, Operation: operation, At: nowUTC(), TeamAuditContext: audit, Team: t, Session: &s, PreviousGeneration: previous}
	body, err = json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO team_events(team_id,body) VALUES(?,?)`, t.ID, string(body))
	return err
}

// JoinTeam takes a stable team ID. Transport resolves readable names first.
// A distinct retry key intentionally creates a distinct session, even for the
// same credential. No task or execution ownership is created here.
func (d *DB) JoinTeam(teamID, role string, profile TeamProfile, audit TeamAuditContext, key string) (TeamSession, error) {
	return d.JoinTeamLimit(teamID, role, profile, audit, key, MaxTeamSessions)
}

// JoinTeamLimit accepts a trusted operator policy, not a client field.
func (d *DB) JoinTeamLimit(teamID, role string, profile TeamProfile, audit TeamAuditContext, key string, limit int) (TeamSession, error) {
	if limit < 1 || limit > 10000 {
		return TeamSession{}, invalid(errors.New("invalid team session limit"))
	}
	if err := profile.validate(); err != nil {
		return TeamSession{}, err
	}
	if role != "worker" && role != "reviewer" && role != "coordinator" {
		return TeamSession{}, invalid(errors.New("invalid team role"))
	}
	var team Team
	check := func(tx *sql.Tx) (err error) {
		team, err = requireTeamMember(tx, teamID, audit.Actor, role == "coordinator")
		return
	}
	input := struct {
		Role    string
		Profile TeamProfile
	}{role, profile}
	return checkedTaskMutation(d, audit.Actor, "team.join", teamID, key, input, check, func(tx *sql.Tx) (TeamSession, error) {
		var count int
		if err := tx.QueryRow(`SELECT count(*) FROM team_sessions WHERE team_id=? AND state='active'`, teamID).Scan(&count); err != nil {
			return TeamSession{}, err
		}
		if count >= limit {
			return TeamSession{}, ErrTeamSessionQuota
		}
		now := nowUTC()
		s := TeamSession{ID: uuidv7.New(), TeamID: teamID, UserID: audit.Actor.UserID, TokenID: audit.Actor.TokenID, Generation: 1, Role: role, State: "active", Availability: "available", ProfileRevision: 1, TeamProfile: profile, CreatedAt: now, LastSeenAt: now}
		if s.Model.ObservedAt == "" {
			s.Model.ObservedAt = now
		}
		if role == "coordinator" {
			var occupied bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM team_sessions WHERE team_id=? AND role='coordinator' AND state='active')`, teamID).Scan(&occupied); err != nil {
				return TeamSession{}, err
			}
			if occupied {
				return TeamSession{}, ErrTeamCoordinatorOccupied
			}
			var err error
			if s.CoordinatorGeneration, err = nextCoordinatorGeneration(tx, teamID); err != nil {
				return TeamSession{}, err
			}
		}
		return s, saveTeamSession(tx, team, s, 0, "team.join", audit)
	})
}

// TeamSessionCommand contains only concurrency handles and operation content.
// No role, identity, timestamps or coordinator generation is accepted from clients.
type TeamSessionCommand struct {
	SessionID               string       `json:"session_id"`
	Generation              int64        `json:"generation"`
	Profile                 *TeamProfile `json:"profile,omitempty"`
	ExpectedProfileRevision int64        `json:"expected_profile_revision,omitempty"`
	Availability            string       `json:"availability,omitempty"`
}

func (d *DB) ChangeTeamSession(teamID, operation string, command TeamSessionCommand, audit TeamAuditContext, key string) (TeamSession, error) {
	if command.Generation < 1 || !taskIDRE.MatchString(command.SessionID) {
		return TeamSession{}, invalid(errors.New("invalid session handle"))
	}
	if operation != "profile" && (command.Profile != nil || command.ExpectedProfileRevision != 0) {
		return TeamSession{}, invalid(errors.New("profile fields require profile operation"))
	}
	if operation != "heartbeat" && command.Availability != "" {
		return TeamSession{}, invalid(errors.New("availability requires heartbeat"))
	}
	switch operation {
	case "profile":
		if command.Profile == nil || command.ExpectedProfileRevision < 1 {
			return TeamSession{}, invalid(errors.New("profile and expected revision required"))
		}
		copy := *command.Profile
		if err := copy.validate(); err != nil {
			return TeamSession{}, err
		}
		command.Profile = &copy
	case "heartbeat":
		if command.Availability != "available" && command.Availability != "unavailable" {
			return TeamSession{}, invalid(errors.New("invalid availability"))
		}
	case "resume", "leave":
	default:
		return TeamSession{}, invalid(errors.New("unknown session operation"))
	}
	var t Team
	var s TeamSession
	check := func(tx *sql.Tx) (err error) {
		t, s, err = boundTeamSession(tx, teamID, command.SessionID, audit.Actor)
		return
	}
	return checkedTaskMutation(d, audit.Actor, "team."+operation, teamID, key, command, check, func(tx *sql.Tx) (TeamSession, error) {
		if s.State != "active" || s.Generation != command.Generation {
			return TeamSession{}, ErrTeamSessionStale
		}
		s.LastSeenAt = nowUTC()
		switch operation {
		case "profile":
			if s.ProfileRevision != command.ExpectedProfileRevision {
				return TeamSession{}, ErrTeamSessionStale
			}
			s.TeamProfile = *command.Profile
			if s.Model.ObservedAt == "" {
				s.Model.ObservedAt = s.LastSeenAt
			}
			s.ProfileRevision++
		case "heartbeat":
			s.Availability = command.Availability
		case "resume":
			s.Generation++
			if s.Role == "coordinator" {
				var err error
				if s.CoordinatorGeneration, err = nextCoordinatorGeneration(tx, teamID); err != nil {
					return TeamSession{}, err
				}
			}
		case "leave":
			s.State, s.Availability = "left", "unavailable"
			s.Generation++
		}
		if err := saveTeamSession(tx, t, s, command.Generation, "team."+operation, audit); err != nil {
			return TeamSession{}, err
		}
		// Only a worker resume carries its reserved attempt forward. Leave keeps
		// the reservation on the closed handle for operator recovery.
		if operation == "resume" && s.Role == "worker" {
			if err := rebindReservedAssignment(tx, t, s, audit); err != nil {
				return TeamSession{}, err
			}
		}
		return s, nil
	})
}

// TeamRoster validates the requesting current session in the same snapshot as
// its bounded read. Closed sessions remain visible for reconciliation/history.
// The service must check live token/grant authority before calling this method.
func (d *DB) TeamRoster(teamID, sessionID string, generation int64, actor TaskActor, after string, limit int) ([]TeamSession, error) {
	if err := d.taskScopeOK(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 101 {
		return nil, invalid(errors.New("invalid roster page limit"))
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, s, err := boundTeamSession(tx, teamID, sessionID, actor)
	if err != nil {
		return nil, err
	}
	if s.State != "active" || generation != s.Generation {
		return nil, ErrTeamSessionStale
	}
	rows, err := tx.Query(`SELECT body FROM team_sessions WHERE team_id=? AND id>? ORDER BY id LIMIT ?`, teamID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamSession{}
	for rows.Next() {
		var body string
		var member TeamSession
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(body), &member); err != nil {
			return nil, err
		}
		out = append(out, member)
	}
	return out, rows.Err()
}
