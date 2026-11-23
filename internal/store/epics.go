package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"aimem/internal/uuidv7"
)

// Epics group tasks above the task level: a release or a milestone maps
// to the tasks under its epics (docs/DESIGN-kanban-docs.md, "Task
// grouping"). An epic is task-shaped — project scope, an id never
// reused, an expected-revision write, a retirement lifecycle, the
// enablement gate — so it gets task-shaped storage: its own table and
// history (schema 12), written through the same receipt-backed mutation
// as tasks. Retirement replaces deletion: existing task links and
// history keep resolving, and a retired epic takes no new assignment.
// A goal that was reached is closed the same way, in a state of its own:
// REACHED, refused while the epic holds a feature that is neither DONE nor
// CANCELLED, so nothing unfinished stays in a reached goal.

// The epic states: OPEN, and the two closed ones.
const (
	EpicOpen    = "OPEN"
	EpicRetired = "RETIRED"
	EpicReached = "REACHED"
)

// EpicStates are the states an epic has.
var EpicStates = []string{EpicOpen, EpicRetired, EpicReached}

// MaxEpicTextBytes bounds an epic's objective.
const MaxEpicTextBytes = 8 * 1024

var (
	// ErrEpicNotFound: no epic with that id in this project.
	ErrEpicNotFound = errors.New("epic not found")
)

// EpicContent is everything a writer may set. An update replaces it whole,
// except two fields a client may not know: an empty state keeps the
// epic's state, and the check is kept unless the update gives one.
type EpicContent struct {
	Title     string `json:"title"`
	Objective string `json:"objective"`
	State     string `json:"state"`  // OPEN (default on create), RETIRED or REACHED
	Target    string `json:"target"` // the release or milestone it maps to
	// Check is the goal's check: what has to pass for the goal to count as
	// reached, kept apart from the objective's prose.
	Check string `json:"check"`
}

// Epic is the current row.
type Epic struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
	EpicContent
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// EpicChange is one history row.
type EpicChange struct {
	Epic  Epic      `json:"epic"`
	Actor TaskActor `json:"actor"`
}

// EpicConflict is returned when the expected revision is stale; it carries
// the current epic so the caller can decide again on what is there now.
type EpicConflict struct{ Current Epic }

func (c *EpicConflict) Error() string {
	return fmt.Sprintf("epic is at revision %d, not the expected one", c.Current.Revision)
}

// validate checks the content. A create turns an empty state into OPEN; an
// update leaves it empty, and the update keeps the epic's state.
func (c *EpicContent) validate(create bool) error {
	if err := taskText(c.Title, MaxTaskTitleBytes, true); err != nil {
		return fmt.Errorf("title: %w", err)
	}
	if c.State == "" && create {
		c.State = EpicOpen
	}
	if c.State != "" && !slices.Contains(EpicStates, c.State) {
		return fmt.Errorf("invalid epic state (want one of %s)", strings.Join(EpicStates, ", "))
	}
	if create && c.State == EpicReached {
		return errors.New("a new epic is OPEN or RETIRED: mark a goal REACHED with update_epic once its features are done")
	}
	if err := taskText(c.Objective, MaxEpicTextBytes, false); err != nil {
		return fmt.Errorf("objective: %w", err)
	}
	if err := taskText(c.Target, MaxTaskTitleBytes, false); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if err := taskText(c.Check, MaxEpicTextBytes, false); err != nil {
		return fmt.Errorf("check: %w", err)
	}
	return nil
}

// CreateEpic stores a new epic at revision 1 with its first history row.
func (d *DB) CreateEpic(content EpicContent, actor TaskActor, key string) (Epic, error) {
	if err := content.validate(true); err != nil {
		return Epic{}, invalid(err)
	}
	return taskMutation(d, actor, "epic-create", "", key, content, func(tx *sql.Tx) (Epic, error) {
		now := nowUTC()
		e := Epic{ID: uuidv7.New(), Revision: 1, EpicContent: content, CreatedAt: now, UpdatedAt: now}
		return e, saveEpic(tx, e, actor, true)
	})
}

// UpdateEpic replaces the content under expected-revision CAS. Setting the
// state to RETIRED is the retirement; it is an ordinary update, explicit,
// never a roll-up of task states. Setting it to REACHED closes a reached
// goal, from OPEN or RETIRED, and is refused while the epic holds a
// feature that is neither DONE nor CANCELLED. An empty state keeps the
// epic's state, and a nil check keeps its check: a client that does not
// know a field never clears it. content.Check is not read; check is.
func (d *DB) UpdateEpic(id string, content EpicContent, check *string, expected int64, actor TaskActor, key string) (Epic, error) {
	if !taskIDRE.MatchString(id) {
		return Epic{}, ErrEpicNotFound
	}
	if expected < 1 {
		return Epic{}, invalid(errors.New("expected_revision must be a positive revision"))
	}
	content.Check = ""
	if check != nil {
		content.Check = *check
	}
	if err := content.validate(false); err != nil {
		return Epic{}, invalid(err)
	}
	// The receipt's canonical form drops empty values, so a check given
	// empty would hash as a check not given: CheckGiven tells them apart.
	input := struct {
		Content    EpicContent `json:"content"`
		CheckGiven bool        `json:"check_given"`
		Expected   int64       `json:"expected"`
	}{content, check != nil, expected}
	return taskMutation(d, actor, "epic-update", id, key, input, func(tx *sql.Tx) (Epic, error) {
		e, err := readEpic(tx, id)
		if err != nil {
			return Epic{}, err
		}
		if e.Revision != expected {
			return Epic{}, &EpicConflict{Current: e}
		}
		next := content
		if next.State == "" {
			next.State = e.State
		}
		if check == nil {
			next.Check = e.Check
		}
		if next.State == EpicReached && e.State != EpicReached {
			if err := checkGoalFinished(tx, id); err != nil {
				return Epic{}, err
			}
		}
		e.EpicContent = next
		e.Revision++
		e.UpdatedAt = nowUTC()
		return e, saveEpic(tx, e, actor, false)
	})
}

// maxUnfinishedNamed bounds the features a refused REACHED names.
const maxUnfinishedNamed = 5

// checkGoalFinished refuses, inside the epic write's transaction, a goal
// that still holds a feature neither DONE nor CANCELLED, archived ones
// included: an archived feature still belongs to its goal.
func checkGoalFinished(tx *sql.Tx, id string) error {
	rows, err := tx.Query(`SELECT id, state, archived FROM tasks WHERE kind='feature' AND epic=?
		AND state NOT IN ('DONE','CANCELLED') ORDER BY id LIMIT ?`, id, maxUnfinishedNamed+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	var named []string
	for rows.Next() {
		var fid, state string
		var archived int
		if err := rows.Scan(&fid, &state, &archived); err != nil {
			return err
		}
		what := fid + " (" + state
		if archived != 0 {
			what += ", archived"
		}
		named = append(named, what+")")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(named) == 0 {
		return nil
	}
	if len(named) > maxUnfinishedNamed {
		named = append(named[:maxUnfinishedNamed], "and more")
	}
	return invalid(fmt.Errorf("the goal still holds unfinished features: %s: move each to an OPEN goal with set_feature, or finish or cancel it, before the goal is REACHED (an archived feature still belongs to its goal)", strings.Join(named, ", ")))
}

// GetEpic reads the current epic.
func (d *DB) GetEpic(id string) (Epic, error) {
	if !taskIDRE.MatchString(id) {
		return Epic{}, ErrEpicNotFound
	}
	if err := d.taskScopeOK(); err != nil {
		return Epic{}, err
	}
	return readEpic(d.sql, id)
}

// MaxEpicsListed bounds ListEpics: epics are few by construction (a
// release or milestone each), and a project with more than this many has
// outgrown the flat list.
const MaxEpicsListed = 500

// ListEpics returns the project's epics in id order, the closed ones
// (RETIRED and REACHED) only on request.
func (d *DB) ListEpics(includeRetired bool) ([]Epic, error) {
	if err := d.taskScopeOK(); err != nil {
		return nil, err
	}
	query := `SELECT body FROM epics`
	if !includeRetired {
		query += ` WHERE state = 'OPEN'`
	}
	query += ` ORDER BY id LIMIT ?`
	rows, err := d.sql.Query(query, MaxEpicsListed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Epic{}
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return nil, err
		}
		var e Epic
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func saveEpic(tx *sql.Tx, e Epic, actor TaskActor, create bool) error {
	body, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if create {
		_, err = tx.Exec(`INSERT INTO epics(id, state, body) VALUES(?,?,?)`, e.ID, e.State, string(body))
	} else {
		_, err = tx.Exec(`UPDATE epics SET state=?, body=? WHERE id=?`, e.State, string(body), e.ID)
	}
	if err != nil {
		return err
	}
	change, err := json.Marshal(EpicChange{Epic: e, Actor: actor})
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO epic_history(epic_id, revision, body) VALUES(?,?,?)`, e.ID, e.Revision, string(change))
	return err
}

func readEpic(q rowQuerier, id string) (Epic, error) {
	var body string
	if err := q.QueryRow(`SELECT body FROM epics WHERE id=?`, id).Scan(&body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Epic{}, ErrEpicNotFound
		}
		return Epic{}, err
	}
	var e Epic
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		return Epic{}, err
	}
	return e, nil
}

// checkEpicAssignable decides, inside the task's own transaction, whether
// a task may carry epic: it must exist in this project, and a closed epic
// (RETIRED or REACHED) takes no new assignment — an assignment the task
// already had survives the closing, so history keeps resolving.
func checkEpicAssignable(tx *sql.Tx, epic, previous string) error {
	if epic == "" {
		return nil
	}
	e, err := readEpic(tx, epic)
	if errors.Is(err, ErrEpicNotFound) {
		return invalid(errors.New("epic does not exist in this project"))
	}
	if err != nil {
		return err
	}
	if epic != previous {
		switch e.State {
		case EpicRetired:
			return invalid(errors.New("epic is retired and takes no new assignment"))
		case EpicReached:
			return invalid(errors.New("epic is a reached goal and takes no new assignment: give an OPEN epic"))
		}
	}
	return nil
}
