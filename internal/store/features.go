package store

// Features (docs/DESIGN-feature-record.md): a feature is a task row of kind
// feature, with its link to a goal stored beside the editable content. It
// shares the task surface (history, comments, grants, retries) and is never
// work: it is not reserved, offered, team-managed or depended on, and it
// never enters the board feed except for the one entry that closes a task
// turned into a feature.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"aimem/internal/uuidv7"
)

const (
	// KindFeature is Task.Kind for a feature; a task's kind is empty.
	KindFeature = "feature"
	// FeatureFeedState is the feed's closing value for a task turned into
	// a feature. It has the shape of a state name, as the feed's consumer
	// requires, and is not one of TaskStates.
	FeatureFeedState = "FEATURE"

	maxFeaturePosition  = 10000
	maxFeatureNoteBytes = 2048
)

// ErrFeatureNotWork is wrapped by every refusal of a feature where work is
// expected: a claim, an offer, or a dependency on one.
var ErrFeatureNotWork = errors.New("a feature is not work")

// FeatureLink is a feature's link to its goal, the epic it belongs to.
type FeatureLink struct {
	Goal        string `json:"goal"`
	Position    int    `json:"position"`
	Mark        string `json:"mark"`             // mandatory or optional
	Ground      string `json:"ground,omitempty"` // check or owner_condition, for a mandatory feature
	GroundNote  string `json:"ground_note,omitempty"`
	Alternative string `json:"alternative,omitempty"`
}

func (l *FeatureLink) validate(epic string) error {
	if epic == "" {
		return errors.New("a feature belongs to a goal: set the record's epic first")
	}
	if l.Goal != epic {
		return errors.New("the feature's goal must be the record's epic")
	}
	if l.Position < 1 || l.Position > maxFeaturePosition {
		return fmt.Errorf("position must be between 1 and %d", maxFeaturePosition)
	}
	switch l.Mark {
	case "mandatory":
		if l.Ground != "check" && l.Ground != "owner_condition" {
			return errors.New("a mandatory feature needs a ground: check or owner_condition")
		}
	case "optional":
		if l.Ground != "" {
			return errors.New("an optional feature has no ground")
		}
	default:
		return errors.New("mark must be mandatory or optional")
	}
	if err := taskText(l.GroundNote, maxFeatureNoteBytes, false); err != nil {
		return fmt.Errorf("ground_note: %w", err)
	}
	if err := taskText(l.Alternative, maxFeatureNoteBytes, false); err != nil {
		return fmt.Errorf("alternative: %w", err)
	}
	return nil
}

func (t Task) isFeature() bool { return t.Kind == KindFeature }

// featureWorkError refuses a feature where work is expected.
func featureWorkError(base error, id string) error {
	return fmt.Errorf("%w: %w: %s is a feature; claim or offer one of its confirmed tasks instead", ErrFeatureNotWork, base, id)
}

// dependentOf names a record of the claim's graph that depends on id, for
// the refusal's message; the graph always holds one, since id was reached
// through a dependency.
func dependentOf(nodes map[string]dependencyNode, id string) string {
	best := ""
	for from, n := range nodes {
		if slices.Contains(n.task.Dependencies, id) && (best == "" || from < best) {
			best = from
		}
	}
	return best
}

// featureDependencyError names the record that depends on a feature.
func featureDependencyError(base error, task, feature string) error {
	return fmt.Errorf("%w: %w: %s depends on %s, which is a feature: confirm the task on the feature instead", ErrFeatureNotWork, base, task, feature)
}

// checkFeatureRules applies, inside a write's transaction, the rules every
// write of a record obeys: a feature keeps its goal as its epic, moves
// into DONE only with evidence, and no write adds a dependency on a
// feature of this project. from and priorDeps (a JSON array) are the
// stored state and dependencies before the write; create means there were
// none. Only an added dependency is refused, so a record that names a
// feature anyway (data no guard wrote) can still be released or finished.
func checkFeatureRules(tx *sql.Tx, t Task, from, priorDeps string, create bool) error {
	if t.isFeature() && t.Feature != nil && t.Epic != t.Feature.Goal {
		return invalid(errors.New("a feature's epic is its goal: it cannot move to another epic"))
	}
	if t.isFeature() && t.State == "DONE" && (create || from != "DONE") && len(t.EvidenceRefs) == 0 {
		return invalid(errors.New("a feature moves to DONE only with evidence that its check passes: add an evidence reference"))
	}
	if len(t.Dependencies) == 0 {
		return nil
	}
	deps, err := json.Marshal(t.Dependencies)
	if err != nil {
		return err
	}
	var feature string
	err = tx.QueryRow(`SELECT id FROM tasks WHERE kind='feature' AND id IN (SELECT value FROM json_each(?))
		AND id NOT IN (SELECT value FROM json_each(?)) ORDER BY id LIMIT 1`, string(deps), priorDeps).Scan(&feature)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return invalid(featureDependencyError(errors.New("refused"), t.ID, feature))
}

// createFeatureRetry shares the task create's operation: one route and one
// key, so a key used for one kind of creation answers the other with
// ErrTaskRetryConflict. The input carries the link, so the plain create's
// digest (its content alone) is unchanged.
func createFeatureRetry(content TaskContent, link FeatureLink) retry {
	if content.State == "" {
		content.State = "BACKLOG"
	}
	return retry{"create", "", struct {
		Content TaskContent `json:"content"`
		Feature FeatureLink `json:"feature"`
	}{content, link}}
}

// CreateFeatureReplays reports whether CreateFeature with these arguments
// would return a stored receipt rather than create a feature.
func (d *DB) CreateFeatureReplays(content TaskContent, link FeatureLink, actor TaskActor, key string) bool {
	return d.replays(actor, key, createFeatureRetry(content, link))
}

// CreateFeature stores a new record as a feature in one write, so it never
// appears in the board feed as work.
func (d *DB) CreateFeature(content TaskContent, link FeatureLink, actor TaskActor, key string) (Task, error) {
	if content.State == "" {
		content.State = "BACKLOG"
	}
	if err := content.validate(); err != nil {
		return Task{}, invalid(err)
	}
	if err := link.validate(content.Epic); err != nil {
		return Task{}, invalid(err)
	}
	r := createFeatureRetry(content, link)
	t, err := taskMutation(d, actor, r.op, r.scope, key, r.input, func(tx *sql.Tx) (Task, error) {
		if err := checkEpicAssignable(tx, content.Epic, ""); err != nil {
			return Task{}, err
		}
		now := nowUTC()
		t := Task{ID: uuidv7.New(), Revision: 1, Kind: KindFeature, Feature: &link, TaskContent: content, CreatedAt: now, UpdatedAt: now}
		return t, saveTask(tx, t, actor, true)
	})
	if err == nil && d.hints != nil {
		d.hints.put(t.ID, d.projectID)
	}
	return t, err
}

func setFeatureRetry(id string, link *FeatureLink, remove bool, expected int64) retry {
	return retry{"set_feature", id, struct {
		Feature  *FeatureLink `json:"feature,omitempty"`
		Remove   bool         `json:"remove,omitempty"`
		Expected int64        `json:"expected"`
	}{link, remove, expected}}
}

// SetFeatureReplays reports whether SetFeature with these arguments would
// return a stored receipt rather than change the record.
func (d *DB) SetFeatureReplays(id string, link *FeatureLink, remove bool, expected int64, actor TaskActor, key string) bool {
	return d.replays(actor, key, setFeatureRetry(id, link, remove, expected))
}

// SetFeature turns a task into a feature, changes a feature's link, or,
// with remove, turns a feature back into a task. A conversion appends the
// feed's closing entry and a removal the entry that reopens the record, in
// the same transaction. A conversion is refused while the task is reserved
// or team-managed, or while a task of this project depends on it; a removal
// is refused while the feature has confirmed tasks.
func (d *DB) SetFeature(id string, link *FeatureLink, remove bool, expected int64, actor TaskActor, key string) (Task, error) {
	if !taskIDRE.MatchString(id) {
		return Task{}, ErrTaskNotFound
	}
	if expected < 1 {
		return Task{}, invalid(errors.New("expected_revision must be a positive revision"))
	}
	if remove == (link != nil) {
		return Task{}, invalid(errors.New("give either the feature's link or remove"))
	}
	r := setFeatureRetry(id, link, remove, expected)
	check := func(tx *sql.Tx) error {
		var managed bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM team_managed_tasks WHERE task_id=? AND managed=1)`, id).Scan(&managed); err != nil {
			return err
		}
		if managed {
			return ErrManagedTask
		}
		return rejectActiveReservation(tx, id)
	}
	return checkedTaskMutation(d, actor, r.op, r.scope, key, r.input, check, func(tx *sql.Tx) (Task, error) {
		t, err := readTask(tx, id)
		if err != nil {
			return Task{}, err
		}
		if t.Revision != expected {
			return Task{}, &TaskConflict{Current: t}
		}
		was := t.isFeature()
		if remove {
			if !was {
				return Task{}, invalid(errors.New("the record is not a feature"))
			}
			if len(t.Dependencies) != 0 {
				return Task{}, invalid(fmt.Errorf("the feature has %d confirmed tasks: take them out first", len(t.Dependencies)))
			}
			t.Kind, t.Feature = "", nil
		} else {
			if err := link.validate(t.Epic); err != nil {
				return Task{}, invalid(err)
			}
			if !was {
				if err := rejectFeatureDependents(tx, id); err != nil {
					return Task{}, err
				}
			}
			t.Kind, t.Feature = KindFeature, link
		}
		t.Revision++
		t.UpdatedAt = nowUTC()
		if err := saveTask(tx, t, actor, false); err != nil {
			return Task{}, err
		}
		switch {
		case remove:
			err = insertStateChange(tx, t.ID, t.Revision, FeatureFeedState, t.State, t.UpdatedAt)
		case !was:
			err = insertStateChange(tx, t.ID, t.Revision, t.State, FeatureFeedState, t.UpdatedAt)
		}
		return t, err
	})
}

// rejectFeatureDependents refuses a conversion that a task of this project
// depends on, and names those tasks. A task of another project is not
// visible here; the readers that act on dependencies refuse it.
func rejectFeatureDependents(tx *sql.Tx, id string) error {
	rows, err := tx.Query(`SELECT id FROM tasks WHERE id <> ? AND EXISTS (SELECT 1 FROM json_each(tasks.body, '$.dependencies') WHERE value = ?) ORDER BY id`, id, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	var dependents []string
	for rows.Next() {
		var dep string
		if err := rows.Scan(&dep); err != nil {
			return err
		}
		dependents = append(dependents, dep)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(dependents) != 0 {
		return invalid(fmt.Errorf("these tasks depend on the record, and no task may depend on a feature: %s", strings.Join(dependents, ", ")))
	}
	return nil
}

func confirmFeatureTaskRetry(featureID, taskID string, remove bool, expected int64) retry {
	return retry{"confirm_feature_task", featureID, struct {
		Task     string `json:"task"`
		Remove   bool   `json:"remove,omitempty"`
		Expected int64  `json:"expected"`
	}{taskID, remove, expected}}
}

// ConfirmFeatureTaskReplays reports whether ConfirmFeatureTask with these
// arguments would return a stored receipt rather than change the feature.
func (d *DB) ConfirmFeatureTaskReplays(featureID, taskID string, remove bool, expected int64, actor TaskActor, key string) bool {
	return d.replays(actor, key, confirmFeatureTaskRetry(featureID, taskID, remove, expected))
}

// ConfirmFeatureTask adds one task to a feature's dependencies, or with
// remove takes it out, under the feature's revision. The store refuses a
// task that is a feature of this project; that the task exists and that the
// caller may read it, in this project or another, is the route's check.
func (d *DB) ConfirmFeatureTask(featureID, taskID string, remove bool, expected int64, actor TaskActor, key string) (Task, error) {
	if !taskIDRE.MatchString(featureID) {
		return Task{}, ErrTaskNotFound
	}
	if !taskIDRE.MatchString(taskID) || taskID == featureID {
		return Task{}, invalid(errors.New("task must be the id of another task"))
	}
	if expected < 1 {
		return Task{}, invalid(errors.New("expected_revision must be a positive revision"))
	}
	r := confirmFeatureTaskRetry(featureID, taskID, remove, expected)
	return taskMutation(d, actor, r.op, r.scope, key, r.input, func(tx *sql.Tx) (Task, error) {
		t, err := readTask(tx, featureID)
		if err != nil {
			return Task{}, err
		}
		if t.Revision != expected {
			return Task{}, &TaskConflict{Current: t}
		}
		if !t.isFeature() {
			return Task{}, invalid(errors.New("the record is not a feature"))
		}
		at := -1
		for i, dep := range t.Dependencies {
			if dep == taskID {
				at = i
			}
		}
		content := t.TaskContent
		switch {
		case remove && at < 0:
			return Task{}, invalid(errors.New("the task is not confirmed on this feature"))
		case remove:
			content.Dependencies = append(append([]string{}, t.Dependencies[:at]...), t.Dependencies[at+1:]...)
		case at >= 0:
			return t, nil // already confirmed: nothing to write
		default:
			content.Dependencies = append(append([]string{}, t.Dependencies...), taskID)
		}
		if err := content.validate(); err != nil {
			return Task{}, invalid(err)
		}
		t.TaskContent = content
		t.Revision++
		t.UpdatedAt = nowUTC()
		return t, saveTask(tx, t, actor, false)
	})
}
