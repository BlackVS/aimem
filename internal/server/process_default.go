package server

// The hub's default process: the selection `aimem tasks on` applies to a
// project that has none. It is instance configuration, set by the admin on
// the hub host (aimem process default set), never a value of the release.
// The admin's command resolves the tag and imports the set as `process
// select` does, and stores the result here: repository, tag, the commit
// the tag held, manifest and the set's digest. Applying it is a selection
// like any other, made by `tasks on` through the process route, so the
// default itself is never read by a session.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"aimem/internal/process"
)

// processDefaultFile holds the default in the state root, beside
// providers.json; absent means no default.
const processDefaultFile = "process-default.json"

// processDefaultMu serializes the default's writes.
var processDefaultMu sync.Mutex

func (s *Server) processDefaultPath() string {
	return filepath.Join(s.reg.Root(), processDefaultFile)
}

// readProcessDefault returns the stored default, or nil when none is set.
func (s *Server) readProcessDefault() (*process.Ref, error) {
	raw, err := os.ReadFile(s.processDefaultPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ref process.Ref
	if err := json.Unmarshal(raw, &ref); err != nil {
		return nil, fmt.Errorf("%s is unreadable: %w", processDefaultFile, err)
	}
	return &ref, nil
}

// getProcessDefault answers {default}, null when none is set.
func (s *Server) getProcessDefault(w http.ResponseWriter, _ *http.Request) {
	ref, err := s.readProcessDefault()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.ok(w, map[string]any{"default": ref})
}

// putProcessDefault stores {repo, ref, commit, manifest, digest}. The
// default names a tag (ref) and the commit it held when the admin set it,
// and the digest of the set imported from that commit, so applying it
// selects exactly what the admin saw.
func (s *Server) putProcessDefault(w http.ResponseWriter, r *http.Request) {
	var ref process.Ref
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&ref); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	ref.Name, ref.SelectedAt, ref.SelectedBy = "", "", ""
	if err := ref.Validate(); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if ref.Ref == "" || ref.Digest == "" {
		s.fail(w, http.StatusBadRequest, errors.New("a default names its tag (ref) and the digest of its set (aimem process default set on the hub host sends them)"))
		return
	}
	ref.Name = process.SetName(ref.Repo, ref.Manifest)
	ref.SelectedAt = time.Now().UTC().Format(time.RFC3339)
	ref.SelectedBy = taskActor(r).Name
	raw, _ := json.MarshalIndent(ref, "", "  ")
	processDefaultMu.Lock()
	defer processDefaultMu.Unlock()
	tmp := s.processDefaultPath() + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.Rename(tmp, s.processDefaultPath()); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("default process set", "repo", ref.Repo, "ref", ref.Ref, "commit", ref.Commit, "by", ref.SelectedBy)
	s.ok(w, map[string]any{"default": ref})
}

// deleteProcessDefault removes the default; with none set it says so.
func (s *Server) deleteProcessDefault(w http.ResponseWriter, r *http.Request) {
	processDefaultMu.Lock()
	defer processDefaultMu.Unlock()
	err := os.Remove(s.processDefaultPath())
	if errors.Is(err, os.ErrNotExist) {
		s.fail(w, http.StatusNotFound, errors.New("no default process is set"))
		return
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("default process cleared", "by", taskActor(r).Name)
	s.ok(w, map[string]any{"default": nil})
}
