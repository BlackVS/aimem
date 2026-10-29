package server

// The hub's default process: the selection `aimem tasks on` applies to a
// project that has none. It is instance configuration, set by the admin on
// the hub host (aimem process default set), never a value of the release.
// The admin's command resolves the tag and imports the set as `process
// select` does, and stores the result here: repository, tag, the commit
// the tag held, manifest and the set's digest, and the set's files, which
// the hub keeps so `tasks on` applies the default without reading Git (a
// default stored by an older command has no files, and is applied from
// Git as before). Applying it is a selection like any other, made by
// `tasks on` through the process route, so the default itself is never
// read by a session.

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
// providers.json; absent means no default. processDefaultSetFile holds
// its set, when the command that stored it sent one.
const (
	processDefaultFile    = "process-default.json"
	processDefaultSetFile = "process-default-set.json"
)

// processDefaultMu serializes the default's writes.
var processDefaultMu sync.Mutex

func (s *Server) processDefaultPath() string {
	return filepath.Join(s.reg.Root(), processDefaultFile)
}

func (s *Server) processDefaultSetPath() string {
	return filepath.Join(s.reg.Root(), processDefaultSetFile)
}

// readProcessDefaultSet returns the kept set of the default ref, or nil
// when none is kept or the kept one is not ref's (a write that stopped
// between the two files).
func (s *Server) readProcessDefaultSet(ref *process.Ref) (*storedSet, error) {
	raw, err := os.ReadFile(s.processDefaultSetPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st storedSet
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("%s is unreadable: %w", processDefaultSetFile, err)
	}
	if ref == nil || st.Manifest != ref.Manifest || st.Digest() != ref.Digest {
		return nil, nil
	}
	return &st, nil
}

// writeStateFile replaces path with data through a temporary file.
func writeStateFile(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
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

// getProcessDefault answers {default, set_kept}: the default, null when
// none is set, and whether the hub keeps its set.
func (s *Server) getProcessDefault(w http.ResponseWriter, _ *http.Request) {
	ref, err := s.readProcessDefault()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	st, err := s.readProcessDefaultSet(ref)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.ok(w, map[string]any{"default": ref, "set_kept": st != nil})
}

// getProcessDefaultSet answers {digest, set}: the files of the default's
// set, which `tasks on` sends as the selection's set. 404 when no default
// is set or the hub keeps no set for it.
func (s *Server) getProcessDefaultSet(w http.ResponseWriter, _ *http.Request) {
	ref, err := s.readProcessDefault()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	st, err := s.readProcessDefaultSet(ref)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if st == nil {
		s.fail(w, http.StatusNotFound, errors.New("the hub keeps no set for the default process"))
		return
	}
	s.ok(w, map[string]any{"digest": ref.Digest, "set": st.Bundle})
}

// putProcessDefault stores {repo, ref, commit, manifest, digest, files}.
// The default names a tag (ref) and the commit it held when the admin set
// it, and the digest of the set imported from that commit, so applying it
// selects exactly what the admin saw. files is that set, checked as a
// selection's set is and bounded like a selection body; a body without
// files (an older command) stores the default alone.
func (s *Server) putProcessDefault(w http.ResponseWriter, r *http.Request) {
	var req struct {
		process.Ref
		Files map[string][]byte `json:"files"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, processBodyMax))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	ref := req.Ref
	ref.Name, ref.SelectedAt, ref.SelectedBy = "", "", ""
	if err := ref.Validate(); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if ref.Ref == "" || ref.Digest == "" {
		s.fail(w, http.StatusBadRequest, errors.New("a default names its tag (ref) and the digest of its set (aimem process default set on the hub host sends them)"))
		return
	}
	var set []byte
	if req.Files != nil {
		bundle := process.Bundle{Version: 1, Manifest: ref.Manifest, Files: req.Files}
		if err := bundle.Validate(); err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		if d := bundle.Digest(); d != ref.Digest {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("the files' digest is %s, not the stated %s", d[:12], ref.Digest[:12]))
			return
		}
		set, _ = json.Marshal(storedSet{Bundle: bundle, StoredAt: time.Now().UTC().Format(time.RFC3339)})
	}
	ref.Name = process.SetName(ref.Repo, ref.Manifest)
	ref.SelectedAt = time.Now().UTC().Format(time.RFC3339)
	ref.SelectedBy = taskActor(r).Name
	raw, _ := json.MarshalIndent(ref, "", "  ")
	processDefaultMu.Lock()
	defer processDefaultMu.Unlock()
	// The set first: a kept set is served only while the default names its
	// digest, so one left by a write that stopped halfway is never applied.
	var err error
	if set != nil {
		err = writeStateFile(s.processDefaultSetPath(), set)
	} else if err = os.Remove(s.processDefaultSetPath()); errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err == nil {
		err = writeStateFile(s.processDefaultPath(), append(raw, '\n'))
	}
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("default process set", "repo", ref.Repo, "ref", ref.Ref, "commit", ref.Commit, "set_kept", set != nil, "by", ref.SelectedBy)
	s.ok(w, map[string]any{"default": ref, "set_kept": set != nil})
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
	if err := os.Remove(s.processDefaultSetPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("default process cleared", "by", taskActor(r).Name)
	s.ok(w, map[string]any{"default": nil})
}
