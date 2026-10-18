package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"aimem/internal/process"
	"aimem/internal/store"
)

// Process selection: which Git repository, commit and manifest hold the
// process documents a project's agents follow, and, since the hub-served
// set became the default path (docs/DESIGN-portable-team-context.md,
// increment 1d), the files themselves. Selecting is an admin action on
// the hub host with a compare-and-swap on the previous commit; the
// admin's command imports the files (from Git, or from an archive) and
// sends them with the selection, so the hub runs no Git. The reference is
// on the ordinary surface, because it is what an agent needs to find its
// rules and grants no access to the repository; the set is served only
// under a current project grant, because it replaces a Git permission.
// The hub keeps the sets of the last processSetsKept selections, so an
// earlier one is re-selected without a new import.
const (
	processMetaKey    = "process"
	processHistoryKey = "process_history"
	processHistoryMax = 50
	processSetPrefix  = "process_set:" // + digest: the stored set
	processSetsKey    = "process_sets" // the kept digests, newest first
	processSetsKept   = 5
	processBodyMax    = 8 << 20 // a set of MaxSetBytes, base64 in JSON
)

// storedSet is the bundle as the hub keeps it, with when it arrived.
type storedSet struct {
	process.Bundle
	StoredAt string `json:"stored_at"`
}

// keptSet is one entry of the kept-sets listing.
type keptSet struct {
	Digest   string `json:"digest"`
	Manifest string `json:"manifest"`
	Bytes    int    `json:"bytes"`
	StoredAt string `json:"stored_at"`
}

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

func decodeKept(v string) []string {
	var kept []string
	if v != "" {
		json.Unmarshal([]byte(v), &kept)
	}
	return kept
}

// getProcessRef answers the current selection; ?history=1 adds the
// retained earlier selections (newest first), so historical evidence that
// names a commit can still be resolved after a change, and the sets the
// hub still keeps, so an admin sees which earlier selection needs no
// re-import.
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
		keptRaw, err := db.GetMeta(processSetsKey)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		sets := []keptSet{}
		for _, d := range decodeKept(keptRaw) {
			raw, err := db.GetMeta(processSetPrefix + d)
			if err != nil {
				s.fail(w, http.StatusInternalServerError, err)
				return
			}
			var st storedSet
			if raw == "" || json.Unmarshal([]byte(raw), &st) != nil {
				continue
			}
			sets = append(sets, keptSet{Digest: d, Manifest: st.Manifest, Bytes: st.Bytes(), StoredAt: st.StoredAt})
		}
		out["sets"] = sets
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

// processRefused is a selection the hub will not record: the body says
// why, and nothing was stored.
type processRefused struct{ msg string }

func (e processRefused) Error() string { return e.msg }

// putProcessRef selects or clears the reference. The body's
// expected_commit must equal the current selection's commit ("" when there
// is none). A selection carries the set: files (the manifest and every
// file it names, as the admin's command read them from Git or an archive),
// or the digest of a set this hub still keeps. The comparison, the stored
// set, the selection, the kept list and the history are one atomic storage
// step, so two admins who both read the same selection cannot both write
// (the second is refused with 409 and the current selection), and a
// selection whose content does not validate stores nothing.
func (s *Server) putProcessRef(w http.ResponseWriter, r *http.Request) {
	db := s.processProject(w, r)
	if db == nil {
		return
	}
	var req struct {
		process.Ref
		ExpectedCommit string            `json:"expected_commit"`
		Clear          bool              `json:"clear"`
		Files          map[string][]byte `json:"files"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, processBodyMax))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	ref := req.Ref
	var bundle *process.Bundle
	if !req.Clear {
		ref.Name = ""
		if err := ref.Validate(); err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		switch {
		case len(req.Files) > 0:
			bundle = &process.Bundle{Version: 1, Manifest: ref.Manifest, Files: req.Files}
			if err := bundle.Validate(); err != nil {
				s.fail(w, http.StatusBadRequest, fmt.Errorf("the set does not validate, nothing stored: %w", err))
				return
			}
			if d := bundle.Digest(); ref.Digest != "" && ref.Digest != d {
				s.fail(w, http.StatusBadRequest, fmt.Errorf("the files' digest is %s, not the stated %s", d[:12], ref.Digest[:12]))
				return
			} else {
				ref.Digest = d
			}
		case ref.Digest == "":
			s.fail(w, http.StatusBadRequest, errors.New("a selection carries its set: the files of the manifest and what it names, or the digest of a set this hub keeps (aimem process select on the hub host sends them)"))
			return
		}
		ref.Name = process.SetName(ref.Repo, ref.Manifest)
		ref.SelectedAt = time.Now().UTC().Format(time.RFC3339)
		ref.SelectedBy = taskActor(r).Name
	} else if len(req.Files) > 0 || req.Digest != "" {
		s.fail(w, http.StatusBadRequest, errors.New("clear takes no files or digest"))
		return
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
				return processRefused{"nothing to clear"}
			}
			result = nil
			return set(processMetaKey, "")
		}
		keptRaw, err := get(processSetsKey)
		if err != nil {
			return err
		}
		kept := decodeKept(keptRaw)
		if bundle != nil {
			raw, err := json.Marshal(storedSet{Bundle: *bundle, StoredAt: ref.SelectedAt})
			if err != nil {
				return err
			}
			if err := set(processSetPrefix+ref.Digest, string(raw)); err != nil {
				return err
			}
		} else {
			// A digest alone names a set this hub keeps, for this manifest.
			stored, err := get(processSetPrefix + ref.Digest)
			if err != nil {
				return err
			}
			var st storedSet
			if stored == "" || json.Unmarshal([]byte(stored), &st) != nil {
				return processRefused{fmt.Sprintf("this hub keeps no set %s; re-import it (aimem process select with the files)", ref.Digest[:12])}
			}
			if st.Manifest != ref.Manifest {
				return processRefused{fmt.Sprintf("set %s was stored for manifest %s, not %s", ref.Digest[:12], st.Manifest, ref.Manifest)}
			}
		}
		// The kept list: this digest first, then the others, the oldest
		// beyond the bound dropped (its value cleared) — unless it is this
		// one, which is current.
		next := []string{ref.Digest}
		for _, d := range kept {
			if d != ref.Digest {
				next = append(next, d)
			}
		}
		for len(next) > processSetsKept {
			d := next[len(next)-1]
			next = next[:len(next)-1]
			if err := set(processSetPrefix+d, ""); err != nil {
				return err
			}
		}
		keptOut, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err := set(processSetsKey, string(keptOut)); err != nil {
			return err
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
	var refused processRefused
	switch {
	case errors.As(err, &conflict):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": conflict.Error(), "current": conflict.current})
		return
	case errors.As(err, &refused):
		s.fail(w, http.StatusBadRequest, err)
		return
	case err != nil:
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if result == nil {
		s.log.Info("process reference cleared", "project", r.PathValue("p"), "by", taskActor(r).Name)
	} else {
		s.log.Info("process reference selected", "project", r.PathValue("p"), "commit", result.Commit, "digest", result.Digest, "imported", bundle != nil, "by", result.SelectedBy)
	}
	s.ok(w, map[string]any{"project": r.PathValue("p"), "current": result, "imported": bundle != nil})
}

// getProcessSet serves the stored set of the current selection (or, with
// ?commit= and ?manifest=, of an earlier selection this hub still keeps:
// the version an accepted attempt was taken under) to a caller with a
// current grant on the project. The answer is the whole set with its
// digest, or an error: 404 when nothing is selected, when the selection
// predates stored sets (an older hub selected it; the client then falls
// back to Git) or when the asked version is no longer kept. Nothing in
// the request names a path or a URL.
func (s *Server) getProcessSet(w http.ResponseWriter, r *http.Request) {
	if s.projectReadDenied(w, r, "process set", false) {
		return
	}
	db := s.processProject(w, r)
	if db == nil {
		return
	}
	ref, err := currentProcessRef(db)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	q := r.URL.Query()
	if c, m := q.Get("commit"), q.Get("manifest"); c != "" || m != "" {
		want := process.Ref{Repo: "https://x", Commit: c, Manifest: m}
		if err := want.Validate(); err != nil {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("commit and manifest: %w", err))
			return
		}
		ref = nil
		hist, err := db.GetMeta(processHistoryKey)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		var entries []process.Ref
		if hist != "" {
			json.Unmarshal([]byte(hist), &entries)
		}
		for i := range entries {
			if entries[i].Commit == c && entries[i].Manifest == m {
				ref = &entries[i]
				break
			}
		}
		if ref == nil {
			s.fail(w, http.StatusNotFound, errors.New("no selection of that commit and manifest is recorded for this project"))
			return
		}
	}
	if ref == nil {
		s.fail(w, http.StatusNotFound, errors.New("no process reference selected for this project; an admin selects one (aimem process select … on the hub host)"))
		return
	}
	if ref.Digest == "" {
		s.fail(w, http.StatusNotFound, errors.New("the selection was made before this hub stored sets; an admin re-selects it on the hub host (aimem process select …), which imports the files"))
		return
	}
	raw, err := db.GetMeta(processSetPrefix + ref.Digest)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if raw == "" {
		s.fail(w, http.StatusNotFound, fmt.Errorf("set %s is no longer kept on this hub", ref.Digest[:12]))
		return
	}
	if strings.Trim(r.Header.Get("If-None-Match"), `"`) == ref.Digest {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", `"`+ref.Digest+`"`)
	s.ok(w, map[string]any{
		"project":   r.PathValue("p"),
		"selection": map[string]string{"name": ref.DisplayName(), "commit": ref.Commit, "manifest": ref.Manifest, "digest": ref.Digest, "selected_at": ref.SelectedAt},
		"set":       json.RawMessage(raw),
	})
}

// processesInUse lists the distinct process selections this hub holds,
// each with the projects that follow it, most used first. It is what an
// admin turning tasks on for a project without one is offered.
func (s *Server) processesInUse() ([]process.InUse, error) {
	ids, err := s.reg.Projects()
	if err != nil {
		return nil, err
	}
	var out []process.InUse
	at := map[process.Ref]int{}
	for _, p := range ids {
		if store.IsReservedProject(p) {
			continue
		}
		db, err := s.reg.OpenExisting(p)
		if errors.Is(err, store.ErrNoSuchProject) {
			continue // dropped since the listing
		}
		if err != nil {
			return nil, fmt.Errorf("project %q: %w", p, err)
		}
		ref, err := currentProcessRef(db)
		if err != nil {
			return nil, fmt.Errorf("project %q: %w", p, err)
		}
		if ref == nil {
			continue
		}
		key := ref.Key()
		i, seen := at[key]
		if !seen {
			i = len(out)
			at[key] = i
			out = append(out, process.InUse{Repo: ref.Repo, Ref: ref.Ref, Commit: ref.Commit, Manifest: ref.Manifest})
		}
		out[i].Projects = append(out[i].Projects, p)
	}
	slices.SortStableFunc(out, func(a, b process.InUse) int { return len(b.Projects) - len(a.Projects) })
	return out, nil
}

// listProcesses answers the selections in use on this hub (admin).
func (s *Server) listProcesses(w http.ResponseWriter, r *http.Request) {
	inUse, err := s.processesInUse()
	if err != nil {
		s.log.Error("processes in use", "err", err)
		s.fail(w, http.StatusInternalServerError, errors.New("process selections unavailable"))
		return
	}
	if inUse == nil {
		inUse = []process.InUse{}
	}
	s.ok(w, map[string]any{"processes": inUse})
}
