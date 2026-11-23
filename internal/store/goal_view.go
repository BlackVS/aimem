package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// GoalView is what a goal's features listing reads from the goal's project
// in one read transaction: the goal, its features in id order, the states of
// their confirmed tasks that live in this project, and the number of the
// goal's tasks that no feature of this project confirms. Nothing in it is
// stored; it is computed at read time.
type GoalView struct {
	Goal     Epic
	Features []Task
	// Truncated reports that the goal has more features than the limit.
	Truncated bool
	// Local holds the state of each confirmed task found in this project,
	// by id. A confirmed id missing here is of another project, or gone.
	Local map[string]string
	// Unconfirmed counts the records of kind task under the goal's epic,
	// not archived, that no feature of this project confirms, whatever
	// that feature's goal and archived or not.
	Unconfirmed int
}

// ReadGoalView reads a goal's features listing from one snapshot of the
// project. The transaction is read-only, so the driver begins it deferred
// (no writer lock, whatever the store's _txlock) and, under WAL, every
// statement in it reads the same snapshot: a write that commits meanwhile
// changes none of the answer.
func (d *DB) ReadGoalView(goal string, includeArchived bool, limit int) (GoalView, error) {
	if !taskIDRE.MatchString(goal) {
		return GoalView{}, ErrEpicNotFound
	}
	if err := d.taskScopeOK(); err != nil {
		return GoalView{}, err
	}
	tx, err := d.sql.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return GoalView{}, err
	}
	defer tx.Rollback()
	return readGoalView(tx, goal, includeArchived, limit)
}

// goalViewBetweenReads runs between the features read and the reads that
// follow it; a test sets it to write from another connection there.
var goalViewBetweenReads = func() {}

// readGoalView is ReadGoalView's reads, in the transaction it is given.
func readGoalView(tx *sql.Tx, goal string, includeArchived bool, limit int) (GoalView, error) {
	v := GoalView{Features: []Task{}, Local: map[string]string{}}
	e, err := readEpic(tx, goal)
	if err != nil {
		return GoalView{}, err
	}
	v.Goal = e
	query := taskReadQuery + ` WHERE tasks.kind='feature' AND tasks.epic=?`
	if !includeArchived {
		query += ` AND tasks.archived=0`
	}
	rows, err := tx.Query(query+` ORDER BY tasks.id LIMIT ?`, goal, limit+1)
	if err != nil {
		return GoalView{}, err
	}
	for rows.Next() {
		t, err := scanCurrentTask(rows)
		if err != nil {
			rows.Close()
			return GoalView{}, err
		}
		v.Features = append(v.Features, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return GoalView{}, err
	}
	if len(v.Features) > limit {
		v.Features, v.Truncated = v.Features[:limit], true
	}
	goalViewBetweenReads()
	var ids []string
	for _, f := range v.Features {
		ids = append(ids, f.Dependencies...)
	}
	if len(ids) > 0 {
		list, err := json.Marshal(ids)
		if err != nil {
			return GoalView{}, err
		}
		rows, err := tx.Query(`SELECT id, state FROM tasks WHERE id IN (SELECT value FROM json_each(?))`, string(list))
		if err != nil {
			return GoalView{}, err
		}
		for rows.Next() {
			var id, state string
			if err := rows.Scan(&id, &state); err != nil {
				rows.Close()
				return GoalView{}, err
			}
			v.Local[id] = state
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return GoalView{}, err
		}
	}
	err = tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE kind='task' AND epic=? AND archived=0
		AND id NOT IN (SELECT j.value FROM tasks f, json_each(f.body, '$.dependencies') j WHERE f.kind='feature')`,
		goal).Scan(&v.Unconfirmed)
	if err != nil {
		return GoalView{}, err
	}
	return v, nil
}
