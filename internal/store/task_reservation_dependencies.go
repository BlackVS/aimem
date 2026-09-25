package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"aimem/internal/uuidv7"
)

var ErrDependencyUnresolved = errors.New("dependency evidence is unresolved")

const dependencyClaimTimeout = 5 * time.Second
const maxDependencyWalk = 512

func beginClaimTx(ctx context.Context, db *DB) (*sql.DB, *sql.Tx, error) {
	// A dedicated connection keeps the short busy timeout from changing the
	// registry handle used by ordinary task writers.
	handle, err := sql.Open("sqlite", "file:"+db.path+
		"?mode=rw&_txlock=immediate&_pragma=busy_timeout(50)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, nil, err
	}
	handle.SetMaxOpenConns(1)
	for {
		tx, err := handle.BeginTx(ctx, nil)
		if err == nil {
			return handle, tx, nil
		}
		if ctx.Err() != nil {
			handle.Close()
			return nil, nil, ctx.Err()
		}
		var sqlErr *sqlite.Error
		if !errors.As(err, &sqlErr) || sqlErr.Code()&0xff != sqlite3.SQLITE_BUSY {
			handle.Close()
			return nil, nil, err
		}
		select {
		case <-ctx.Done():
			handle.Close()
			return nil, nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (r *Registry) lockClaimLifecycle(ctx context.Context) error {
	for !r.teamMu.TryRLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		r.teamMu.RUnlock()
		return err
	}
	return nil
}

// DependencyReadVerifier must establish the caller's selected context and
// current read grant for this stable project access ID. It must not re-enter
// a project DB held by the claim. C5 supplies the concrete implementation.
type DependencyReadVerifier func(context.Context, string, string) error

type dependencyNode struct {
	task     Task
	project  string
	accessID string
	db       *DB
}

// locateDependencyGraph gathers the potential lock set before any transaction
// is opened. The graph is checked again under locks, so this read grants no
// eligibility by itself. Walking the graph detects a cycle back to the owner.
func (r *Registry) locateDependencyGraph(ctx context.Context, owner Task,
	verify DependencyReadVerifier) (map[string]dependencyNode, error) {
	nodes := make(map[string]dependencyNode)
	verified := make(map[string]bool)
	pending := []string{owner.ID}
	for len(pending) != 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(nodes) >= maxDependencyWalk {
			return nil, ErrDependencyUnresolved
		}
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if _, ok := nodes[id]; ok {
			continue
		}
		project, db, err := r.LocateTask(id)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDependencyUnresolved, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		accessID, err := r.ExistingProjectAccessID(project)
		if err != nil || accessID == "" {
			return nil, ErrDependencyUnresolved
		}
		if !verified[project] {
			if err := verify(ctx, project, accessID); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrDependencyUnresolved, err)
			}
			verified[project] = true
		}
		task, err := db.GetTask(id)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrDependencyUnresolved, err)
		}
		nodes[id] = dependencyNode{task: task, project: project, accessID: accessID, db: db}
		pending = append(pending, task.Dependencies...)
	}
	return nodes, nil
}

func dependencyProjects(nodes map[string]dependencyNode) []string {
	seen := make(map[string]bool)
	for _, node := range nodes {
		seen[node.project] = true
	}
	projects := make([]string, 0, len(seen))
	for project := range seen {
		projects = append(projects, project)
	}
	sort.Strings(projects)
	return projects
}

func taskDependenciesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func tasksEnabledTx(tx *sql.Tx) (bool, error) {
	var value string
	err := tx.QueryRow(`SELECT value FROM meta WHERE key=?`, TasksMetaKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return value == "on", err
}

func validateDependencyGraph(ownerID string, nodes map[string]dependencyNode, txs map[string]*sql.Tx,
	verify DependencyReadVerifier, ctx context.Context) (Task, []DependencyEvidence, error) {
	current := make(map[string]Task, len(nodes))
	checked := make(map[string]bool)
	for id, node := range nodes {
		if err := ctx.Err(); err != nil {
			return Task{}, nil, err
		}
		tx := txs[node.project]
		if !checked[node.project] {
			on, err := tasksEnabledTx(tx)
			if err != nil || !on {
				return Task{}, nil, ErrDependencyUnresolved
			}
			if err := verify(ctx, node.project, node.accessID); err != nil {
				return Task{}, nil, fmt.Errorf("%w: %v", ErrDependencyUnresolved, err)
			}
			checked[node.project] = true
		}
		task, err := readTask(tx, id)
		if err != nil {
			return Task{}, nil, ErrDependencyUnresolved
		}
		if task.Revision != node.task.Revision ||
			!taskDependenciesEqual(task.Dependencies, node.task.Dependencies) {
			if id == ownerID {
				return Task{}, nil, &TaskConflict{Current: task}
			}
			return Task{}, nil, ErrDependencyUnresolved
		}
		current[id] = task
	}
	colors := make(map[string]uint8, len(nodes))
	var walk func(string) error
	walk = func(id string) error {
		if colors[id] == 1 {
			return ErrDependencyUnresolved
		}
		if colors[id] == 2 {
			return nil
		}
		colors[id] = 1
		for _, dep := range current[id].Dependencies {
			if _, ok := current[dep]; !ok {
				return ErrDependencyUnresolved
			}
			if err := walk(dep); err != nil {
				return err
			}
		}
		colors[id] = 2
		return nil
	}
	if err := walk(ownerID); err != nil {
		return Task{}, nil, err
	}
	owner := current[ownerID]
	evidence := make([]DependencyEvidence, 0, len(owner.Dependencies))
	seen := make(map[string]bool)
	for _, id := range owner.Dependencies {
		if id == ownerID || seen[id] {
			if id == ownerID {
				return Task{}, nil, ErrDependencyUnresolved
			}
			continue
		}
		seen[id] = true
		task := current[id]
		if task.State != "DONE" {
			return Task{}, nil, ErrDependencyUnresolved
		}
		node := nodes[id]
		evidence = append(evidence, DependencyEvidence{TaskID: id, Project: node.project,
			AccessID: node.accessID, Revision: task.Revision})
	}
	return owner, evidence, nil
}

// ClaimTaskReservation is an internal, fail-closed dependency claim boundary.
// It writes only the owner DB; dependency write-intent transactions remain
// open until the owner claim and receipt commit. It authorizes no actor role.
func (r *Registry) ClaimTaskReservation(ctx context.Context, in TaskReservationInput, actor TaskActor,
	key string, verify DependencyReadVerifier) (TaskReservationOutcome, error) {
	if err := validateReservationInput(ReservationClaim, &in); err != nil {
		return TaskReservationOutcome{}, err
	}
	if verify == nil {
		return TaskReservationOutcome{}, ErrDependencyUnresolved
	}
	ctx, cancel := context.WithTimeout(ctx, dependencyClaimTimeout)
	defer cancel()
	if err := r.lockClaimLifecycle(ctx); err != nil {
		return TaskReservationOutcome{}, err
	}
	defer r.teamMu.RUnlock()
	project, db, err := r.LocateTask(in.TaskID)
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	if err := ctx.Err(); err != nil {
		return TaskReservationOutcome{}, err
	}
	ownerAccessID, err := r.ExistingProjectAccessID(project)
	if err != nil || ownerAccessID == "" {
		return TaskReservationOutcome{}, ErrDependencyUnresolved
	}
	if err := verify(ctx, project, ownerAccessID); err != nil {
		return TaskReservationOutcome{}, fmt.Errorf("%w: %v", ErrDependencyUnresolved, err)
	}
	// A committed retry is an outcome lookup, not a fresh eligibility
	// decision. It must remain recoverable if a dependency later reopens.
	if prior, found, err := db.GetTaskReservationReceipt(ReservationClaim, in, actor, key); err != nil {
		return TaskReservationOutcome{}, err
	} else if found {
		return prior, nil
	}
	owner, err := db.GetTask(in.TaskID)
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	nodes, err := r.locateDependencyGraph(ctx, owner, verify)
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	if nodes[in.TaskID].project != project {
		return TaskReservationOutcome{}, ErrDependencyUnresolved
	}
	projects := dependencyProjects(nodes)
	txs := make(map[string]*sql.Tx, len(projects))
	handles := make(map[string]*sql.DB, len(projects))
	defer func() {
		for i := len(projects) - 1; i >= 0; i-- {
			if tx := txs[projects[i]]; tx != nil {
				tx.Rollback()
			}
			if handle := handles[projects[i]]; handle != nil {
				handle.Close()
			}
		}
	}()
	for _, p := range projects {
		nodeDB := nodes[in.TaskID].db
		for _, node := range nodes {
			if node.project == p {
				nodeDB = node.db
				break
			}
		}
		handle, tx, err := beginClaimTx(ctx, nodeDB)
		if err != nil {
			return TaskReservationOutcome{}, err
		}
		handles[p] = handle
		txs[p] = tx
	}
	ownerTx := txs[project]
	var evidence []DependencyEvidence
	authorize := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, p := range projects {
			var accessID string
			for _, node := range nodes {
				if node.project == p {
					accessID = node.accessID
					break
				}
			}
			currentID, err := r.ExistingProjectAccessID(p)
			if err != nil || currentID != accessID {
				return ErrDependencyUnresolved
			}
			if err := verify(ctx, p, accessID); err != nil {
				return fmt.Errorf("%w: %v", ErrDependencyUnresolved, err)
			}
		}
		return ctx.Err()
	}
	out, err := reservationMutationTx(db, ownerTx, actor, ReservationClaim, in, key,
		func(tx *sql.Tx) (TaskReservationOutcome, error) {
			current, deps, err := validateDependencyGraph(in.TaskID, nodes, txs, verify, ctx)
			if err != nil {
				return TaskReservationOutcome{}, err
			}
			if current.Revision != in.ExpectedRevision {
				return TaskReservationOutcome{}, &TaskConflict{Current: current}
			}
			evidence = deps
			hold, err := readTaskReservation(tx, in.TaskID, current.Revision)
			if err != nil {
				return TaskReservationOutcome{}, err
			}
			var managed bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM team_managed_tasks WHERE task_id=? AND managed=1)`,
				in.TaskID).Scan(&managed); err != nil {
				return TaskReservationOutcome{}, err
			}
			if managed || hold.ID != "" || current.State != "READY" || current.Archived {
				return TaskReservationOutcome{}, ErrReservationConflict
			}
			before := hold
			hold.ID, hold.Holder = uuidv7.New(), in.Holder
			if err := advanceReservation(tx, &hold); err != nil {
				return TaskReservationOutcome{}, err
			}
			if err := recordReservationEvent(tx, ReservationClaim, before, hold, actor, "", evidence); err != nil {
				return TaskReservationOutcome{}, err
			}
			return TaskReservationOutcome{Task: current, Reservation: hold}, nil
		}, authorize)
	if err != nil {
		return TaskReservationOutcome{}, err
	}
	if err := ownerTx.Commit(); err != nil {
		return TaskReservationOutcome{}, err
	}
	return out, nil
}

// ReconcileReservationDependencies reports whether a held claim's original
// dependency evidence has changed or become unreadable. It never releases the
// hold. The caller must also authorize disclosure of the reservation itself.
func (r *Registry) ReconcileReservationDependencies(ctx context.Context, taskID string,
	verify DependencyReadVerifier) (bool, error) {
	if verify == nil {
		return true, ErrDependencyUnresolved
	}
	ctx, cancel := context.WithTimeout(ctx, dependencyClaimTimeout)
	defer cancel()
	if err := r.lockClaimLifecycle(ctx); err != nil {
		return true, err
	}
	defer r.teamMu.RUnlock()
	project, db, err := r.LocateTask(taskID)
	if err != nil {
		return true, err
	}
	accessID, err := r.ExistingProjectAccessID(project)
	if err != nil || accessID == "" {
		return true, ErrDependencyUnresolved
	}
	if err := verify(ctx, project, accessID); err != nil {
		return true, fmt.Errorf("%w: %v", ErrDependencyUnresolved, err)
	}
	hold, err := db.GetTaskReservation(taskID)
	if err != nil {
		return true, err
	}
	if hold.ID == "" {
		return true, ErrReservationStale
	}
	var body string
	if err := db.sql.QueryRowContext(ctx, `SELECT body FROM task_reservation_events
		WHERE task_id=? AND operation='claim' ORDER BY sequence DESC LIMIT 1`, taskID).Scan(&body); err != nil {
		return true, err
	}
	var event struct {
		After        TaskReservation      `json:"after"`
		Dependencies []DependencyEvidence `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(body), &event); err != nil {
		return true, err
	}
	if event.After.ID != hold.ID {
		return true, ErrDependencyUnresolved
	}
	currentOwner, err := db.GetTask(taskID)
	if err != nil || event.Dependencies == nil {
		// A store-only or pre-C3 claim has no verified dependency proof.
		return true, ErrDependencyUnresolved
	}
	proved := make(map[string]bool, len(event.Dependencies))
	for _, dep := range event.Dependencies {
		proved[dep.TaskID] = true
	}
	currentIDs := make(map[string]bool, len(currentOwner.Dependencies))
	for _, id := range currentOwner.Dependencies {
		currentIDs[id] = true
		if !proved[id] {
			return true, ErrDependencyUnresolved
		}
	}
	if len(currentIDs) != len(proved) {
		return true, ErrDependencyUnresolved
	}
	for _, proof := range event.Dependencies {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		located, depDB, err := r.LocateTask(proof.TaskID)
		if err != nil {
			return true, ErrDependencyUnresolved
		}
		currentAccess, err := r.ExistingProjectAccessID(located)
		if err != nil || currentAccess != proof.AccessID {
			return true, ErrDependencyUnresolved
		}
		if err := verify(ctx, located, currentAccess); err != nil {
			return true, fmt.Errorf("%w: %v", ErrDependencyUnresolved, err)
		}
		on, err := depDB.TasksEnabled()
		if err != nil || !on {
			return true, ErrDependencyUnresolved
		}
		dep, err := depDB.GetTask(proof.TaskID)
		if err != nil || dep.State != "DONE" || dep.Revision != proof.Revision {
			return true, ErrDependencyUnresolved
		}
	}
	currentHold, err := db.GetTaskReservation(taskID)
	if err != nil || currentHold.ID != hold.ID {
		return true, ErrDependencyUnresolved
	}
	return false, nil
}
