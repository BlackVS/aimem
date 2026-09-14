package store

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// taskHints remembers where a task was last seen and which ids were
// recently looked up and not found. It is a hint, never the authority:
// LocateTask checks a positive hint against the project before trusting
// it and falls back to the scan, and a negative entry expires on its own
// and is displaced by a create. Without it every lookup — by any valid
// credential, for any id, hit or miss — opened and queried every project.
//
// The negative entries assume this process is the only task writer, which
// holds: tasks are created only through the service, never by the CLI
// beside it or by sync.
type taskHints struct {
	mu   sync.Mutex
	loc  map[string]string    // task id -> project it was last found in
	miss map[string]time.Time // task id -> when a full scan found nothing
	now  func() time.Time
}

const (
	taskHintMax = 65536            // positive entries kept before the map is reset
	taskMissMax = 4096             // negative entries kept before the map is reset
	taskMissTTL = 30 * time.Second // how long a miss is believed without a scan
)

func newTaskHints() *taskHints {
	return &taskHints{loc: map[string]string{}, miss: map[string]time.Time{}, now: time.Now}
}

// put records where id was found; any remembered miss for it is void.
func (h *taskHints) put(id, project string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.loc) >= taskHintMax {
		h.loc = map[string]string{}
	}
	h.loc[id] = project
	delete(h.miss, id)
}

func (h *taskHints) get(id string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := h.loc[id]
	return p, ok
}

func (h *taskHints) forget(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.loc, id)
}

// missed reports whether a scan found nothing for id within taskMissTTL.
func (h *taskHints) missed(id string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	at, ok := h.miss[id]
	if !ok {
		return false
	}
	if h.now().Sub(at) > taskMissTTL {
		delete(h.miss, id)
		return false
	}
	return true
}

func (h *taskHints) noteMiss(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.miss) >= taskMissMax {
		h.miss = map[string]time.Time{}
	}
	h.miss[id] = h.now()
}

// LocateTask resolves a task ID to its owning project. The partition is
// the authority: a hint from a create or an earlier lookup is checked
// against the project it names (a rename shows on the next lookup), and
// otherwise every existing ordinary project is scanned. A project that
// cannot be opened is skipped with its error retained: the task is
// reported not found only when every project was readable, and only such
// a conclusive miss is remembered.
func (r *Registry) LocateTask(id string) (string, *DB, error) {
	if !taskIDRE.MatchString(id) {
		return "", nil, ErrTaskNotFound
	}
	if p, ok := r.hints.get(id); ok {
		if db, err := r.OpenExisting(p); err == nil {
			if _, err := db.GetTask(id); err == nil {
				return p, db, nil
			}
		}
		r.hints.forget(id) // renamed, merged or dropped since: the scan decides
	}
	if r.hints.missed(id) {
		return "", nil, ErrTaskNotFound
	}
	p, db, err := r.scanForTask(id)
	switch {
	case err == nil:
		r.hints.put(id, p)
	case errors.Is(err, ErrTaskNotFound):
		r.hints.noteMiss(id)
	}
	return p, db, err
}

func (r *Registry) scanForTask(id string) (string, *DB, error) {
	r.taskScans.Add(1)
	projects, err := r.Projects()
	if err != nil {
		return "", nil, err
	}
	var unreadable error
	for _, p := range projects {
		if IsReservedProject(p) {
			continue
		}
		db, err := r.OpenExisting(p)
		if err != nil {
			unreadable = fmt.Errorf("project %q could not be opened: %w", p, err)
			continue
		}
		if _, err := db.GetTask(id); err == nil {
			return p, db, nil
		} else if !errors.Is(err, ErrTaskNotFound) {
			unreadable = fmt.Errorf("project %q could not be read: %w", p, err)
		}
	}
	if unreadable != nil {
		return "", nil, fmt.Errorf("task lookup incomplete: %w", unreadable)
	}
	return "", nil, ErrTaskNotFound
}
