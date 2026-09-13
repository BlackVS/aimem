package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"aimem/internal/schema"
	"aimem/internal/uuidv7"
)

var accessIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

var ErrInvalidAccessProject = errors.New("access assignments require an existing ordinary project")

// ProjectAccessID is a host-local identity for authorization grants. It moves
// with the existing project directory on rename and disappears on deletion.
// Unlike mutable/synced meta it cannot be overwritten by a writer API call.
// A recreated project with the same name never inherits the old grants.
func (r *Registry) ProjectAccessID(project string) (string, error) {
	return r.projectAccessID(project, true)
}

// ExistingProjectAccessID reads an identity without creating it. An existing
// project with no identity returns an empty string and no error.
func (r *Registry) ExistingProjectAccessID(project string) (string, error) {
	return r.projectAccessID(project, false)
}

func (r *Registry) projectAccessID(project string, create bool) (string, error) {
	if !schema.ValidProjectID(project) || project == UserScopeProject || strings.HasPrefix(project, "group-") {
		return "", ErrInvalidAccessProject
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	dir := filepath.Join(r.root, "projects", project)
	if fi, err := os.Lstat(dir); err != nil {
		return "", err
	} else if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("invalid project directory")
	}
	if _, err := os.Stat(filepath.Join(dir, "journal.db")); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "access-id")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		if !create {
			return "", nil
		}
		// Publish only a complete, synced value. Link is no-replace, unlike
		// Rename on Unix: concurrent registries must never replace an ID that
		// another writer has already bound grants to.
		f, err := os.CreateTemp(dir, ".access-id-*")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		_, writeErr := f.WriteString(uuidv7.New() + "\n")
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr != nil {
			return "", writeErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		if err := os.Link(f.Name(), path); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if fi, err := os.Lstat(path); err != nil {
		return "", err
	} else if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("project access identity is not a regular file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(body))
	if !accessIDPattern.MatchString(id) {
		return "", fmt.Errorf("invalid project access identity; refusing to replace it")
	}
	return id, nil
}
