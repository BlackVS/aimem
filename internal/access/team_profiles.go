package access

import (
	"database/sql"
	"fmt"
	"time"

	"aimem/internal/uuidv7"
)

// HubID is minted once in access schema 3. URLs and project names never
// identify the issuing hub.
func (s *Store) HubID() (string, error) {
	var id string
	err := s.db.QueryRow("SELECT id FROM hub_identity WHERE singleton=1").Scan(&id)
	return id, err
}

type TeamProfile struct {
	ID        string
	ServiceID string
	TeamID    string
	Disabled  bool
}

// Profile administration has no route or command yet: an operator surface
// that establishes the peer service and team keys is a separate, reviewed
// increment. Only the team-mode verifier (internal/server/teamcontext.go)
// and tests call these methods.
func (s *Store) CreateTeamProfile(actor, serviceID, teamID string) (TeamProfile, error) {
	if err := validName(serviceID); err != nil {
		return TeamProfile{}, fmt.Errorf("service ID: %w", err)
	}
	if err := validName(teamID); err != nil {
		return TeamProfile{}, fmt.Errorf("team ID: %w", err)
	}
	p := TeamProfile{ID: uuidv7.New(), ServiceID: serviceID, TeamID: teamID}
	err := s.change(actor, "team_profile.create", p.ID, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO team_access_profiles(id,service_id,team_id) VALUES(?,?,?)", p.ID, p.ServiceID, p.TeamID)
		return err
	})
	if err != nil {
		return TeamProfile{}, err
	}
	return p, nil
}

func (s *Store) TeamProfileByKey(serviceID, teamID string) (TeamProfile, error) {
	var p TeamProfile
	err := s.db.QueryRow("SELECT id,service_id,team_id,disabled FROM team_access_profiles WHERE service_id=? AND team_id=?", serviceID, teamID).
		Scan(&p.ID, &p.ServiceID, &p.TeamID, &p.Disabled)
	return p, err
}

func (s *Store) SetTeamProfileDisabled(actor, profileID string, disabled bool) error {
	return s.change(actor, fmt.Sprintf("team_profile.disabled.%t", disabled), profileID, func(tx *sql.Tx) error {
		if err := requireRow(tx, "team_access_profiles", profileID); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE team_access_profiles SET disabled=? WHERE id=?", disabled, profileID)
		return err
	})
}

func (s *Store) SetTeamGrant(actor, project, profileID string, present bool) error {
	if project == "" {
		return fmt.Errorf("project instance is required")
	}
	return s.change(actor, fmt.Sprintf("team_grant.%t", present), project+"/"+profileID, func(tx *sql.Tx) error {
		if err := requireRow(tx, "team_access_profiles", profileID); err != nil {
			return err
		}
		query := "DELETE FROM team_profile_grants WHERE project=? AND profile_id=?"
		if present {
			query = "INSERT OR IGNORE INTO team_profile_grants(project,profile_id) VALUES(?,?)"
		}
		_, err := tx.Exec(query, project, profileID)
		return err
	})
}

// TeamGrantAllows evaluates only a profile grant and a live individual
// user-scoped token, never a personal or group grant. Its profile argument is
// not proof of membership or role: the caller must have verified the team
// context online (the team-mode verifier) before asking.
func (s *Store) TeamGrantAllows(user, token, profileID, project string) (bool, error) {
	if user == "" || token == "" || profileID == "" || project == "" {
		return false, nil
	}
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM tokens t
JOIN users u ON u.id=t.user_id
JOIN team_access_profiles p ON p.id=? AND p.disabled=0
JOIN team_profile_grants g ON g.profile_id=p.id AND g.project=?
WHERE t.id=? AND u.id=? AND u.disabled=0 AND t.revoked=0 AND t.expires_at>?
AND t.scope='user' AND t.project=''`, profileID, project, token, user, time.Now().Unix()).Scan(&n)
	return n == 1, err
}

// TeamGrantProjects lists the project instances an enabled profile is
// currently granted, for the team-mode context report.
func (s *Store) TeamGrantProjects(profileID string) ([]string, error) {
	rows, err := s.db.Query(`SELECT g.project FROM team_profile_grants g
JOIN team_access_profiles p ON p.id=g.profile_id AND p.disabled=0
WHERE g.profile_id=? ORDER BY g.project`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordTeamRequest audits one verified team-mode request. The subject names
// the service, team, session, generation and correlation ID; never a handle.
func (s *Store) RecordTeamRequest(userID, action, subject string) error {
	return s.change("user:"+userID, action, subject, func(*sql.Tx) error { return nil })
}
