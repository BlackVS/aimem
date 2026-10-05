package store

// Triage (docs/DESIGN-AIFORGE-PILOT-1.md §4): a partial update that moves a
// task between BACKLOG and READY and sets its next action, leaving every
// other field as it is. The pilot's coordinator could not record a READY
// assessment, and a full update_task cleared fields it did not name. Tasks
// have no priority or size fields yet (task 01a0c44a-5ce8); triage gains
// them when they exist.

import (
	"database/sql"
	"errors"
	"fmt"
)

// TaskTriage names the fields a triage write changes; nil leaves a field.
type TaskTriage struct {
	State      *string `json:"state,omitempty"`
	NextAction *string `json:"next_action,omitempty"`
}

// triageStates are the states triage moves a task between.
var triageStates = map[string]bool{"BACKLOG": true, "READY": true}

func (p TaskTriage) validate() error {
	if p.State == nil && p.NextAction == nil {
		return errors.New("name at least one of state or next_action")
	}
	if p.State != nil && !triageStates[*p.State] {
		return fmt.Errorf("triage moves a task between BACKLOG and READY only, not to %q", *p.State)
	}
	return nil
}

// TriageTask applies a partial update under expected-revision CAS. Like a
// full update it refuses a managed task and a task under an active
// reservation (ErrTaskReserved): triage happens before a claim or after a
// release, never under a hold.
func (d *DB) TriageTask(id string, patch TaskTriage, expected int64, actor TaskActor, key string) (Task, error) {
	if !taskIDRE.MatchString(id) {
		return Task{}, ErrTaskNotFound
	}
	if expected < 1 {
		return Task{}, invalid(errors.New("expected_revision must be a positive revision"))
	}
	if err := patch.validate(); err != nil {
		return Task{}, invalid(err)
	}
	// The receipt digest drops empty values, so the input also names the
	// fields the patch sets: an omitted next_action and an empty one are
	// different requests, and a reused key with either is refused as
	// changed input rather than replayed.
	var named []string
	if patch.NextAction != nil {
		named = append(named, "next_action")
	}
	if patch.State != nil {
		named = append(named, "state")
	}
	input := struct {
		Triage   TaskTriage `json:"triage"`
		Expected int64      `json:"expected"`
		Named    []string   `json:"named"`
	}{patch, expected, named}
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
	return checkedTaskMutation(d, actor, "triage", id, key, input, check, func(tx *sql.Tx) (Task, error) {
		t, err := readTask(tx, id)
		if err != nil {
			return Task{}, err
		}
		if t.Revision != expected {
			return Task{}, &TaskConflict{Current: t}
		}
		if t.Archived {
			return Task{}, ErrTaskArchived
		}
		content := t.TaskContent
		if patch.State != nil {
			if !triageStates[t.State] {
				return Task{}, invalid(fmt.Errorf("triage moves a task between BACKLOG and READY only; this task is %s", t.State))
			}
			content.State = *patch.State
		}
		if patch.NextAction != nil {
			content.NextAction = *patch.NextAction
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
