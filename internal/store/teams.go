package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"aimem/internal/uuidv7"
)

var (
	ErrTeamNotFound    = errors.New("team not found")
	ErrTeamNameTaken   = errors.New("team name already exists")
	ErrProjectHasTeams = errors.New("project holds team coordination state; drop and merge are unavailable")
)

type TeamEnrollment struct {
	UserID      string `json:"user_id"`
	Coordinator bool   `json:"coordinator"`
}

type TeamContent struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Enrollment  []TeamEnrollment `json:"enrollment"`
}

type Team struct {
	ID              string `json:"id"`
	Revision        int64  `json:"revision"`
	ProjectInstance string `json:"project_instance"`
	TeamContent
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type TeamConflict struct{ Current Team }

func (e *TeamConflict) Error() string { return "team changed since the expected revision" }

// TeamAuditContext is supplied by the service, never decoded from a request.
type TeamAuditContext struct {
	Actor         TaskActor `json:"actor"`
	RequestID     string    `json:"request_id"`
	ServerVersion string    `json:"server_version"`
	ProcessCommit string    `json:"process_commit,omitempty"`
}

type TeamEvent struct {
	Sequence        int64  `json:"sequence"`
	ID              string `json:"id"`
	ProtocolVersion int    `json:"protocol_version"`
	Operation       string `json:"operation"`
	At              string `json:"at"`
	TeamAuditContext
	PreviousRevision   int64        `json:"previous_revision"`
	Team               Team         `json:"team"`
	Session            *TeamSession `json:"session,omitempty"`
	PreviousGeneration int64        `json:"previous_generation,omitempty"`
}

func (c *TeamContent) validate() error {
	if err := taskText(c.Name, 128, true); err != nil {
		return invalid(fmt.Errorf("name: %w", err))
	}
	if strings.TrimSpace(c.Name) != c.Name {
		return invalid(errors.New("name has surrounding whitespace"))
	}
	if strings.ContainsAny(c.Name, "\r\n\t") {
		return invalid(errors.New("name must be one line without tabs"))
	}
	if err := taskText(c.Description, 4096, false); err != nil {
		return invalid(fmt.Errorf("description: %w", err))
	}
	if len(c.Enrollment) > 100 {
		return invalid(errors.New("at most 100 enrolled users"))
	}
	seen := map[string]bool{}
	for _, e := range c.Enrollment {
		if !taskIDRE.MatchString(e.UserID) || seen[e.UserID] {
			return invalid(errors.New("enrollment requires unique user IDs"))
		}
		seen[e.UserID] = true
	}
	if c.Enrollment == nil {
		c.Enrollment = []TeamEnrollment{}
	}
	return nil
}

// ConfigureTeam holds the coordination lifecycle lock across project resolution
// and commit. A team can never first appear halfway through a project merge.
// Empty id creates; nonempty id replaces the complete configuration under CAS.
func (r *Registry) ConfigureTeam(project, id string, expected int64, content TeamContent, audit TeamAuditContext, key string) (Team, error) {
	if audit.Actor.Kind != "admin" {
		return Team{}, invalid(errors.New("team setup requires an admin actor"))
	}
	if err := content.validate(); err != nil {
		return Team{}, err
	}
	if (id == "" && expected != 0) || (id != "" && (!taskIDRE.MatchString(id) || expected < 1)) {
		return Team{}, invalid(errors.New("invalid team ID or expected revision"))
	}
	r.teamMu.RLock()
	defer r.teamMu.RUnlock()
	db, err := r.OpenExisting(project)
	if err != nil {
		return Team{}, err
	}
	if err := db.taskScopeOK(); err != nil {
		return Team{}, err
	}
	if enabled, err := db.TasksEnabled(); err != nil {
		return Team{}, err
	} else if !enabled {
		return Team{}, invalid(errors.New("tasks are not enabled"))
	}
	instance, err := r.ProjectAccessID(project)
	if err != nil {
		return Team{}, err
	}
	input := struct {
		ID       string
		Expected int64
		Content  TeamContent
	}{id, expected, content}
	return taskMutation(db, audit.Actor, "team-configure", id, key, input, func(tx *sql.Tx) (Team, error) {
		var enabled string
		if err := tx.QueryRow(`SELECT COALESCE((SELECT value FROM meta WHERE key=?),'')`, TasksMetaKey).Scan(&enabled); err != nil {
			return Team{}, err
		}
		if enabled != "on" {
			return Team{}, invalid(errors.New("tasks are not enabled"))
		}
		var other string
		err := tx.QueryRow(`SELECT id FROM teams WHERE name=? AND id<>?`, content.Name, id).Scan(&other)
		if err == nil {
			return Team{}, ErrTeamNameTaken
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Team{}, err
		}
		now := nowUTC()
		t := Team{ID: id, Revision: expected + 1, ProjectInstance: instance, TeamContent: content, CreatedAt: now, UpdatedAt: now}
		if id == "" {
			t.ID = uuidv7.New()
		} else {
			old, err := readTeam(tx, id)
			if err != nil {
				return Team{}, err
			}
			if old.Revision != expected {
				return Team{}, &TeamConflict{Current: old}
			}
			if old.ProjectInstance != instance {
				return Team{}, errors.New("team project instance mismatch")
			}
			t.CreatedAt = old.CreatedAt
		}
		body, err := json.Marshal(t)
		if err != nil {
			return Team{}, err
		}
		if id == "" {
			_, err = tx.Exec(`INSERT INTO teams(id,name,body) VALUES(?,?,?)`, t.ID, t.Name, string(body))
		} else {
			_, err = tx.Exec(`UPDATE teams SET name=?,body=? WHERE id=?`, t.Name, string(body), id)
		}
		if err != nil {
			return Team{}, err
		}
		event := TeamEvent{ID: uuidv7.New(), ProtocolVersion: 1, Operation: "team.configure", At: now, TeamAuditContext: audit, PreviousRevision: expected, Team: t}
		body, err = json.Marshal(event)
		if err != nil {
			return Team{}, err
		}
		_, err = tx.Exec(`INSERT INTO team_events(team_id,body) VALUES(?,?)`, t.ID, string(body))
		return t, err
	})
}

func readTeam(q rowQuerier, id string) (Team, error) {
	var body string
	if err := q.QueryRow(`SELECT body FROM teams WHERE id=?`, id).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Team{}, ErrTeamNotFound
		}
		return Team{}, err
	}
	var t Team
	err := json.Unmarshal([]byte(body), &t)
	return t, err
}

func (d *DB) GetTeam(id string) (Team, error) {
	if err := d.taskScopeOK(); err != nil {
		return Team{}, err
	}
	return readTeam(d.sql, id)
}

// ResolveTeam accepts a stable ID first, or the exact project-local name.
func (d *DB) ResolveTeam(name string) (Team, error) {
	t, err := d.GetTeam(name)
	if !errors.Is(err, ErrTeamNotFound) {
		return t, err
	}
	var id string
	if err = d.sql.QueryRow(`SELECT id FROM teams WHERE name=?`, name).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Team{}, ErrTeamNotFound
		}
		return Team{}, err
	}
	return d.GetTeam(id)
}

func (d *DB) ListTeams(after string, limit int) ([]Team, error) {
	if err := d.taskScopeOK(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 101 {
		return nil, invalid(errors.New("invalid team page limit"))
	}
	rows, err := d.sql.Query(`SELECT body FROM teams WHERE id>? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Team{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var t Team
		if err := json.Unmarshal([]byte(body), &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (d *DB) TeamEvents(id string, after int64, limit int) ([]TeamEvent, error) {
	if _, err := d.GetTeam(id); err != nil {
		return nil, err
	}
	if after < 0 || limit < 1 || limit > 101 {
		return nil, invalid(errors.New("invalid audit page"))
	}
	rows, err := d.sql.Query(`SELECT sequence,body FROM team_events WHERE team_id=? AND sequence>? ORDER BY sequence LIMIT ?`, id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TeamEvent{}
	for rows.Next() {
		var seq int64
		var body string
		if err := rows.Scan(&seq, &body); err != nil {
			return nil, err
		}
		var e TeamEvent
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, err
		}
		e.Sequence = seq
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) HasTeams() (bool, error) {
	var exists bool
	err := d.sql.QueryRow(`SELECT EXISTS(SELECT 1 FROM teams)`).Scan(&exists)
	return exists, err
}
