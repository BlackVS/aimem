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

type teamProfile struct {
	ID        string
	ServiceID string
	TeamID    string
	Disabled  bool
}

// Profile administration stays package-private until a reviewed operator
// protocol can establish the peer service and team keys.
func (s *Store) createTeamProfile(actor, serviceID, teamID string) (teamProfile, error) {
	if err := validName(serviceID); err != nil {
		return teamProfile{}, fmt.Errorf("service ID: %w", err)
	}
	if err := validName(teamID); err != nil {
		return teamProfile{}, fmt.Errorf("team ID: %w", err)
	}
	p := teamProfile{ID: uuidv7.New(), ServiceID: serviceID, TeamID: teamID}
	err := s.change(actor, "team_profile.create", p.ID, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO team_access_profiles(id,service_id,team_id) VALUES(?,?,?)", p.ID, p.ServiceID, p.TeamID)
		return err
	})
	if err != nil {
		return teamProfile{}, err
	}
	return p, nil
}

func (s *Store) teamProfileByKey(serviceID, teamID string) (teamProfile, error) {
	var p teamProfile
	err := s.db.QueryRow("SELECT id,service_id,team_id,disabled FROM team_access_profiles WHERE service_id=? AND team_id=?", serviceID, teamID).
		Scan(&p.ID, &p.ServiceID, &p.TeamID, &p.Disabled)
	return p, err
}

func (s *Store) setTeamProfileDisabled(actor, profileID string, disabled bool) error {
	return s.change(actor, fmt.Sprintf("team_profile.disabled.%t", disabled), profileID, func(tx *sql.Tx) error {
		if err := requireRow(tx, "team_access_profiles", profileID); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE team_access_profiles SET disabled=? WHERE id=?", disabled, profileID)
		return err
	})
}

func (s *Store) setTeamGrant(actor, project, profileID string, present bool) error {
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

// canWriteTeamToken evaluates only a profile grant and a live individual
// user-scoped token. Its profile argument is not proof of membership or role;
// no production handler may call it until a later verified context boundary
// supplies those facts.
func (s *Store) canWriteTeamToken(user, token, profileID, project string) (bool, error) {
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
