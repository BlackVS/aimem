package store

import (
	"errors"
	"fmt"
	"strings"
)

// A task or comment id may be named by a unique prefix of at least
// TaskIDPrefixMin characters: the first two groups, as people write it
// (01a17718-4ca9). The ids are written lowercase and a prefix is taken as
// written, never case-folded. A prefix resolves to the one full id it
// starts; the rest of the code only ever sees full ids.

// TaskIDPrefixMin is the shortest prefix that names a task or a comment.
const TaskIDPrefixMin = 13

// taskIDLayout is the shape of a full id, position by position: x a
// lowercase hex digit, - a dash, 7 the version, v the variant.
const taskIDLayout = "xxxxxxxx-xxxx-7xxx-vxxx-xxxxxxxxxxxx"

var (
	// ErrTaskIDShort refuses a prefix too short to name one id.
	ErrTaskIDShort = fmt.Errorf("%w: a task or comment id is the full id or a unique prefix of at least %d characters, such as 01a17718-4ca9", ErrTaskInvalid, TaskIDPrefixMin)
	// ErrTaskIDAmbiguous refuses a prefix that starts more than one id.
	ErrTaskIDAmbiguous = errors.New("ambiguous id prefix")
)

// TaskIDMatch is one id a prefix starts and the project it lives in.
type TaskIDMatch struct {
	Project string
	ID      string
}

// AmbiguousIDError lists every id a prefix starts.
type AmbiguousIDError struct {
	Prefix  string
	Matches []TaskIDMatch
}

func (e *AmbiguousIDError) Error() string {
	ids := make([]string, len(e.Matches))
	for i, m := range e.Matches {
		ids[i] = m.ID
	}
	return fmt.Sprintf("ambiguous id prefix %s: it matches %s", e.Prefix, strings.Join(ids, ", "))
}

func (e *AmbiguousIDError) Unwrap() error { return ErrTaskIDAmbiguous }

// IDPrefixShape reports whether s, shorter than a full id, has the shape
// of the start of one.
func IDPrefixShape(s string) bool {
	if s == "" || len(s) >= len(taskIDLayout) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		hex := c >= '0' && c <= '9' || c >= 'a' && c <= 'f'
		switch taskIDLayout[i] {
		case '-':
			if c != '-' {
				return false
			}
		case '7':
			if c != '7' {
				return false
			}
		case 'v':
			if c != '8' && c != '9' && c != 'a' && c != 'b' {
				return false
			}
		default:
			if !hex {
				return false
			}
		}
	}
	return true
}

// prefixBound is the first string after every id that starts with
// prefix: ids hold only hex digits and dashes, all below 'g'.
func prefixBound(prefix string) string { return prefix + "g" }

// taskIDsWithPrefix lists at most limit task ids of this project that
// start with prefix.
func (db *DB) taskIDsWithPrefix(prefix string, limit int) ([]string, error) {
	return db.idsWithPrefix(`SELECT id FROM tasks WHERE id >= ? AND id < ? ORDER BY id LIMIT ?`, prefix, limit)
}

func (db *DB) idsWithPrefix(query, prefix string, limit int, args ...any) ([]string, error) {
	rows, err := db.sql.Query(query, append(args, prefix, prefixBound(prefix), limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// FullTaskID reports whether s is a full task or comment id.
func FullTaskID(s string) bool { return taskIDRE.MatchString(s) }

// ResolveTaskCommentID resolves a comment id of the task: a full id as
// it is, a prefix to the one comment of the task it starts.
func (db *DB) ResolveTaskCommentID(taskID, arg string) (string, error) {
	return resolveIDIn(arg, func(prefix string) ([]string, error) {
		return db.idsWithPrefix(`SELECT id FROM task_comments WHERE task_id = ? AND id >= ? AND id < ? ORDER BY id LIMIT ?`, prefix, taskIDMatchMax, taskID)
	})
}

// ResolveProjectTaskID is ResolveTaskID inside one project, for routes
// that name it.
func (db *DB) ResolveProjectTaskID(arg string) (string, error) {
	return resolveIDIn(arg, func(prefix string) ([]string, error) {
		return db.taskIDsWithPrefix(prefix, taskIDMatchMax)
	})
}

// resolveIDIn resolves arg against the ids list finds for a prefix.
func resolveIDIn(arg string, list func(prefix string) ([]string, error)) (string, error) {
	if FullTaskID(arg) || !IDPrefixShape(arg) {
		return arg, nil
	}
	if len(arg) < TaskIDPrefixMin {
		return "", ErrTaskIDShort
	}
	ids, err := list(arg)
	if err != nil {
		return "", err
	}
	switch len(ids) {
	case 0:
		return "", ErrTaskNotFound
	case 1:
		return ids[0], nil
	}
	e := &AmbiguousIDError{Prefix: arg}
	for _, id := range ids {
		e.Matches = append(e.Matches, TaskIDMatch{ID: id})
	}
	return "", e
}

// taskIDMatchMax bounds the matches an ambiguity reports.
const taskIDMatchMax = 10

// ResolveTaskID resolves a task id argument across the hub, the scope a
// full id has: a full id (or anything that is not the start of one) as it
// is, a prefix to the one task it starts in any ordinary project. A
// prefix found nowhere is remembered as a miss for taskMissTTL, as a full
// id is, so a caller cannot make every lookup scan every project.
func (r *Registry) ResolveTaskID(arg string) (string, error) {
	if FullTaskID(arg) || !IDPrefixShape(arg) {
		return arg, nil
	}
	if len(arg) < TaskIDPrefixMin {
		return "", ErrTaskIDShort
	}
	if r.hints.missed(arg) {
		return "", ErrTaskNotFound
	}
	gen := r.lifecycleGen.Load()
	matches, err := r.taskIDMatches(arg)
	if err != nil {
		return "", err
	}
	switch len(matches) {
	case 0:
		if r.lifecycleGen.Load() == gen {
			r.hints.noteMiss(arg)
		}
		return "", ErrTaskNotFound
	case 1:
		r.hints.put(matches[0].ID, matches[0].Project)
		return matches[0].ID, nil
	}
	return "", &AmbiguousIDError{Prefix: arg, Matches: matches}
}

// taskIDMatches scans every ordinary project for ids that start with
// prefix, at most taskIDMatchMax in all. A project that cannot be read
// makes the answer incomplete, so it is an error, never a smaller list.
func (r *Registry) taskIDMatches(prefix string) ([]TaskIDMatch, error) {
	r.taskScans.Add(1)
	projects, err := r.Projects()
	if err != nil {
		return nil, err
	}
	var out []TaskIDMatch
	for _, p := range projects {
		if IsReservedProject(p) {
			continue
		}
		db, err := r.OpenExisting(p)
		if err != nil {
			return nil, fmt.Errorf("task lookup incomplete: project %q could not be opened: %w", p, err)
		}
		ids, err := db.taskIDsWithPrefix(prefix, taskIDMatchMax-len(out))
		if err != nil {
			return nil, fmt.Errorf("task lookup incomplete: project %q could not be read: %w", p, err)
		}
		for _, id := range ids {
			out = append(out, TaskIDMatch{Project: p, ID: id})
		}
		if len(out) >= taskIDMatchMax {
			break
		}
	}
	return out, nil
}
