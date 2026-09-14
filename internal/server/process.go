package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"aimem/internal/process"
	"aimem/internal/store"
)

// Process reference selection: which Git repository, commit and manifest
// hold the process documents a project's agents follow. The hub stores
// the reference only (docs/DESIGN-kanban-docs.md); every machine fetches
// the files with its own Git access. Selecting is an admin action with a
// compare-and-swap on the previous commit — the "expected metadata
// revision" of the design, without a schema change; reading is on the
// ordinary surface, because the reference is what an agent needs to
// find its rules and grants no access to the repository itself.
const (
	processMetaKey    = "process"
	processHistoryKey = "process_history"
	processHistoryMax = 50
)

func (s *Server) processProject(w http.ResponseWriter, r *http.Request) *store.DB {
	p := r.PathValue("p")
	if store.IsReservedProject(p) {
		s.fail(w, http.StatusBadRequest, store.ErrTaskReservedScope)
		return nil
	}
	db, err := s.reg.OpenExisting(p)
	if errors.Is(err, store.ErrNoSuchProject) {
		s.fail(w, http.StatusNotFound, errors.New("unknown project"))
		return nil
	}
	if err != nil {
		s.log.Error("process reference", "project", p, "err", err)
		s.fail(w, http.StatusInternalServerError, errors.New("project unavailable"))
		return nil
	}
	return db
}

func currentProcessRef(db *store.DB) (*process.Ref, error) {
	v, err := db.GetMeta(processMetaKey)
	if err != nil || v == "" {
		return nil, err
	}
	var ref process.Ref
	if err := json.Unmarshal([]byte(v), &ref); err != nil {
		return nil, fmt.Errorf("stored process reference is unreadable: %w", err)
	}
	return &ref, nil
}

// getProcessRef answers the current selection; ?history=1 adds the
// retained earlier selections (newest first), so historical evidence that
// names a commit can still be resolved after a change.
func (s *Server) getProcessRef(w http.ResponseWriter, r *http.Request) {
	db := s.processProject(w, r)
	if db == nil {
		return
	}
	ref, err := currentProcessRef(db)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	out := map[string]any{"project": r.PathValue("p"), "current": ref}
	if r.URL.Query().Get("history") != "" {
		hist, err := db.GetMeta(processHistoryKey)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		var entries []process.Ref
		if hist != "" {
			json.Unmarshal([]byte(hist), &entries)
		}
		if entries == nil {
			entries = []process.Ref{}
		}
		out["history"] = entries
	}
	if ref == nil && r.URL.Query().Get("history") == "" {
		s.fail(w, http.StatusNotFound, errors.New("no process reference selected for this project; an admin selects one (aimem process select … on the hub host)"))
		return
	}
	s.ok(w, out)
}

// processConflict carries the current selection to the loser of a
// compare-and-swap, so the admin decides again on what they now see.
type processConflict struct{ current *process.Ref }

func (processConflict) Error() string { return "process reference changed since you read it" }

// putProcessRef selects or clears the reference. The body's
// expected_commit must equal the current selection's commit ("" when there
// is none). The comparison, the write and the history update are one
// atomic storage step, so two admins who both read the same selection
// cannot both write: the second sees the first's result and is refused
// with 409 and that current selection.
func (s *Server) putProcessRef(w http.ResponseWriter, r *http.Request) {
	db := s.processProject(w, r)
	if db == nil {
		return
	}
	var req struct {
		process.Ref
		ExpectedCommit string `json:"expected_commit"`
		Clear          bool   `json:"clear"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	ref := req.Ref
	if !req.Clear {
		if err := ref.Validate(); err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		ref.SelectedAt = time.Now().UTC().Format(time.RFC3339)
		ref.SelectedBy = taskActor(r).Name
	}
	var result *process.Ref
	err := db.MetaTx(func(get func(string) (string, error), set func(string, string) error) error {
		v, err := get(processMetaKey)
		if err != nil {
			return err
		}
		var cur *process.Ref
		if v != "" {
			cur = &process.Ref{}
			if err := json.Unmarshal([]byte(v), cur); err != nil {
				return fmt.Errorf("stored process reference is unreadable: %w", err)
			}
		}
		curCommit := ""
		if cur != nil {
			curCommit = cur.Commit
		}
		if req.ExpectedCommit != curCommit {
			return processConflict{current: cur}
		}
		if req.Clear {
			if cur == nil {
				return errors.New("nothing to clear")
			}
			result = nil
			return set(processMetaKey, "")
		}
		raw, _ := json.Marshal(ref)
		if err := set(processMetaKey, string(raw)); err != nil {
			return err
		}
		// The selection history, newest first, bounded — in the same
		// transaction, so a lost entry is impossible.
		var hist []process.Ref
		if h, err := get(processHistoryKey); err == nil && h != "" {
			json.Unmarshal([]byte(h), &hist)
		}
		hist = append([]process.Ref{ref}, hist...)
		if len(hist) > processHistoryMax {
			hist = hist[:processHistoryMax]
		}
		hraw, err := json.Marshal(hist)
		if err != nil {
			return err
		}
		result = &ref
		return set(processHistoryKey, string(hraw))
	})
	var conflict processConflict
	switch {
	case errors.As(err, &conflict):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": conflict.Error(), "current": conflict.current})
		return
	case err != nil && err.Error() == "nothing to clear":
		s.fail(w, http.StatusBadRequest, err)
		return
	case err != nil:
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if result == nil {
		s.log.Info("process reference cleared", "project", r.PathValue("p"), "by", taskActor(r).Name)
	} else {
		s.log.Info("process reference selected", "project", r.PathValue("p"), "commit", result.Commit, "by", result.SelectedBy)
	}
	s.ok(w, map[string]any{"project": r.PathValue("p"), "current": result})
}
