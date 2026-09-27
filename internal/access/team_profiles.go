package access

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"aimem/internal/uuidv7"
)

var (
	// ErrTeamProfileExists refuses a second profile for one (service, team).
	ErrTeamProfileExists = errors.New("a team profile for this service and team already exists")
	// ErrTeamProfileUnknown names a profile that does not exist.
	ErrTeamProfileUnknown = errors.New("unknown team profile")
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

// teamAuditSubject names a profile in the audit: its service, team and ID,
// plus the project instance for a grant.
func teamAuditSubject(p TeamProfile, instance string) string {
	subject := fmt.Sprintf("service=%s team=%s profile=%s", p.ServiceID, p.TeamID, p.ID)
	if instance != "" {
		subject += " instance=" + instance
	}
	return subject
}

// CreateTeamProfile links an aicrew team to this hub. The service must be an
// identity peer registered on this hub, enabled or not, and both keys must be
// identity IDs, the shape an introspection reply carries. The link is
// immutable: there is no delete, and disabling is the off switch. The hub
// admin reaches it through /v1/identity/peers/{service}/teams.
func (s *Store) CreateTeamProfile(actor, serviceID, teamID string) (TeamProfile, error) {
	if !identityIDPattern.MatchString(serviceID) {
		return TeamProfile{}, fmt.Errorf("%w: service ID", ErrInvalidRequest)
	}
	if !identityIDPattern.MatchString(teamID) {
		return TeamProfile{}, fmt.Errorf("%w: team ID must be 1-128 of A-Z a-z 0-9 . _ : -", ErrInvalidRequest)
	}
	// A dot-only ID would be a "." or ".." path segment, which routers clean
	// away; aicrew generates its team IDs, so no real team is dot-only.
	if teamID == "." || teamID == ".." {
		return TeamProfile{}, fmt.Errorf("%w: a team ID cannot be \".\" or \"..\"", ErrInvalidRequest)
	}
	p := TeamProfile{ID: uuidv7.New(), ServiceID: serviceID, TeamID: teamID}
	err := s.change(actor, "team_profile.create", teamAuditSubject(p, ""), func(tx *sql.Tx) error {
		var peers int
		if err := tx.QueryRow("SELECT count(*) FROM identity_peers WHERE service_id=?", serviceID).Scan(&peers); err != nil {
			return err
		}
		if peers != 1 {
			return ErrPeerUnknown
		}
		var existing int
		if err := tx.QueryRow("SELECT count(*) FROM team_access_profiles WHERE service_id=? AND team_id=?", serviceID, teamID).Scan(&existing); err != nil {
			return err
		}
		if existing != 0 {
			return ErrTeamProfileExists
		}
		_, err := tx.Exec("INSERT INTO team_access_profiles(id,service_id,team_id) VALUES(?,?,?)", p.ID, p.ServiceID, p.TeamID)
		return err
	})
	if err != nil {
		return TeamProfile{}, err
	}
	return p, nil
}

// ListTeamProfiles returns a service's profiles in team-ID order.
func (s *Store) ListTeamProfiles(serviceID string) ([]TeamProfile, error) {
	rows, err := s.db.Query("SELECT id,service_id,team_id,disabled FROM team_access_profiles WHERE service_id=? ORDER BY team_id", serviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeamProfile
	for rows.Next() {
		var p TeamProfile
		if err := rows.Scan(&p.ID, &p.ServiceID, &p.TeamID, &p.Disabled); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) teamProfileByID(id string) (TeamProfile, error) {
	var p TeamProfile
	err := s.db.QueryRow("SELECT id,service_id,team_id,disabled FROM team_access_profiles WHERE id=?", id).
		Scan(&p.ID, &p.ServiceID, &p.TeamID, &p.Disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return TeamProfile{}, ErrTeamProfileUnknown
	}
	return p, err
}

func (s *Store) TeamProfileByKey(serviceID, teamID string) (TeamProfile, error) {
	var p TeamProfile
	err := s.db.QueryRow("SELECT id,service_id,team_id,disabled FROM team_access_profiles WHERE service_id=? AND team_id=?", serviceID, teamID).
		Scan(&p.ID, &p.ServiceID, &p.TeamID, &p.Disabled)
	return p, err
}

func (s *Store) SetTeamProfileDisabled(actor, profileID string, disabled bool) error {
	p, err := s.teamProfileByID(profileID)
	if err != nil {
		return err
	}
	return s.change(actor, fmt.Sprintf("team_profile.disabled.%t", disabled), teamAuditSubject(p, ""), func(tx *sql.Tx) error {
		if err := requireRow(tx, "team_access_profiles", profileID); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE team_access_profiles SET disabled=? WHERE id=?", disabled, profileID)
		return err
	})
}

// SetTeamGrant grants or revokes one project instance for a profile. Both
// directions are idempotent and audited, including a no-op.
func (s *Store) SetTeamGrant(actor, project, profileID string, present bool) error {
	if project == "" || strings.ContainsAny(project, " /") {
		return fmt.Errorf("%w: project instance", ErrInvalidRequest)
	}
	p, err := s.teamProfileByID(profileID)
	if err != nil {
		return err
	}
	return s.change(actor, fmt.Sprintf("team_grant.%t", present), teamAuditSubject(p, project), func(tx *sql.Tx) error {
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

// RecordTeamRequest audits one team-mode request outcome under the
// authenticated caller. The subject names what is known of the session and
// the correlation ID; never a handle.
func (s *Store) RecordTeamRequest(actor, action, subject string) error {
	return s.change(actor, action, subject, func(*sql.Tx) error { return nil })
}

// TeamGrantInstances lists every project instance granted to a profile,
// whether or not the profile is enabled, for the operator's listing.
func (s *Store) TeamGrantInstances(profileID string) ([]string, error) {
	rows, err := s.db.Query("SELECT project FROM team_profile_grants WHERE profile_id=? ORDER BY project", profileID)
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

// RecordTeamRevokeWithoutInstance audits a revoke by project name for a
// project that has never had an access instance. Nothing can be granted
// there, so nothing changes, but the admin's request stays in the audit.
func (s *Store) RecordTeamRevokeWithoutInstance(actor, profileID, project string) error {
	p, err := s.teamProfileByID(profileID)
	if err != nil {
		return err
	}
	return s.change(actor, "team_grant.false", teamAuditSubject(p, "")+" project="+project+" instance=none", func(*sql.Tx) error { return nil })
}
