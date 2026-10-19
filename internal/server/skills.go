package server

// The hub-served skills set (docs/DESIGN-portable-team-context.md,
// increment 1e). An admin on the hub host imports a skills release, or the
// instance's own skills, from an archive verified by its SHA256SUMS; the
// hub validates the set, stores it hub-wide under the state root with its
// digest and keeps the last five imports per source. A member reads the
// index (the imported sets and the effective skills, instance over
// release by name) and one set's files under a current grant on a
// project, with the process set's rule, verifies the digest and installs
// the skills its process requires. The import is an admin's action; the
// API lists and reads, never imports, and nothing here names where the
// archive came from.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aimem/internal/skills"
)

// skillsBodyMax bounds an import: a set of MaxSetBytes, base64 in JSON.
const skillsBodyMax = 8 << 20

func (s *Server) skillsStore() *skills.Store {
	s.skillsOnce.Do(func() { s.skillsSt = skills.NewStore(s.reg.Root()) })
	return s.skillsSt
}

// skillsHealth is the health route's view of the store: none imported,
// what is imported per source, or a fault when the index cannot be read.
func (s *Server) skillsHealth() map[string]any {
	ix, err := s.skillsStore().Load()
	if err != nil {
		return map[string]any{"state": "fault", "error": err.Error()}
	}
	out := map[string]any{"state": "none"}
	sources := map[string]string{}
	for src, e := range ix.Current() {
		sources[src] = e.Version
	}
	if len(sources) > 0 {
		out["state"], out["sources"] = "ok", sources
	}
	return out
}

// skillsIndexAnswer is the index as both the admin and a member read it.
func skillsIndexAnswer(project string, ix *skills.Index) map[string]any {
	sources := map[string]any{}
	kept := map[string]any{}
	for _, src := range skills.Sources {
		var cur *skills.Entry
		var k []skills.Entry
		if st := ix.Sources[src]; st != nil {
			cur, k = st.Current, st.Kept
		}
		sources[src] = cur
		kept[src] = k
	}
	out := map[string]any{"sources": sources, "kept": kept, "effective": ix.Effective()}
	if project != "" {
		out["project"] = project
	}
	return out
}

// getSkills is the admin's index read, on the hub host.
func (s *Server) getSkills(w http.ResponseWriter, _ *http.Request) {
	ix, err := s.skillsStore().Load()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.ok(w, skillsIndexAnswer("", ix))
}

// skillsIndexFor loads the index for a handler. A member is told that the
// hub's store is in a fault, not the path of the hub's state root; the
// admin's routes and the log carry the cause.
func (s *Server) skillsIndexFor(w http.ResponseWriter, project string) (*skills.Index, bool) {
	ix, err := s.skillsStore().Load()
	if err == nil {
		return ix, true
	}
	if project == "" {
		s.fail(w, http.StatusInternalServerError, err)
		return nil, false
	}
	s.log.Error("skills index unreadable", "err", err)
	s.fail(w, http.StatusInternalServerError, errors.New("the hub's skills index cannot be read; the admin checks aimem health on the hub host"))
	return nil, false
}

// getProjectSkills serves the index to a caller with a current grant on
// the project: the imported sets per source and the effective skills.
func (s *Server) getProjectSkills(w http.ResponseWriter, r *http.Request) {
	if s.projectReadDenied(w, r, "skills", false) {
		return
	}
	ix, ok := s.skillsIndexFor(w, r.PathValue("p"))
	if !ok {
		return
	}
	s.ok(w, skillsIndexAnswer(r.PathValue("p"), ix))
}

// putSkillsSet imports a set for the source in the path (admin, on the
// hub host). The body carries version and files (skill path to content).
// The set is validated whole and refused whole; the stored files, the
// index and the pruning of sets no source keeps are one step.
func (s *Server) putSkillsSet(w http.ResponseWriter, r *http.Request) {
	source := r.PathValue("source")
	if !skills.ValidSource(source) {
		s.fail(w, http.StatusBadRequest, errors.New("the source is release or instance"))
		return
	}
	var req struct {
		Version string            `json:"version"`
		Files   map[string][]byte `json:"files"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, skillsBodyMax))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	set := &skills.Set{Source: source, Version: req.Version, Files: req.Files}
	if err := set.Validate(); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("the set does not validate, nothing stored: %w", err))
		return
	}
	entry, written, err := s.skillsStore().Import(set, taskActor(r).Name, time.Now())
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("skills set imported", "source", source, "version", entry.Version, "digest", entry.Digest, "skills", len(entry.Skills), "written", written, "by", entry.ImportedBy)
	s.ok(w, map[string]any{"source": source, "current": entry, "imported": written})
}

// skillsSetAnswer serves one stored set: the current import of a source,
// or a kept one by digest. ETag is the digest.
func (s *Server) skillsSetAnswer(w http.ResponseWriter, r *http.Request, project string) {
	ix, ok := s.skillsIndexFor(w, project)
	if !ok {
		return
	}
	q := r.URL.Query()
	source, digest := q.Get("source"), q.Get("digest")
	if source == "" {
		source = r.PathValue("source")
	}
	var entry *skills.Entry
	switch {
	case digest != "":
		if !skills.ValidDigest(digest) {
			s.fail(w, http.StatusBadRequest, errors.New("digest must be 64 lowercase hex characters"))
			return
		}
		for src, st := range ix.Sources {
			if st == nil || (source != "" && src != source) {
				continue
			}
			for i := range st.Kept {
				if st.Kept[i].Digest == digest {
					entry, source = &st.Kept[i], src
				}
			}
		}
		if entry == nil {
			s.fail(w, http.StatusNotFound, fmt.Errorf("this hub keeps no skills set %s", digest[:12]))
			return
		}
	case !skills.ValidSource(source):
		s.fail(w, http.StatusBadRequest, errors.New("source=release or source=instance names the set (or digest= a kept one)"))
		return
	default:
		if st := ix.Sources[source]; st != nil {
			entry = st.Current
		}
		if entry == nil {
			s.fail(w, http.StatusNotFound, fmt.Errorf("no %s skills set is imported on this hub; an admin imports one on the hub host (aimem skills import --from-archive FILE --source %s)", source, source))
			return
		}
	}
	if strings.Trim(r.Header.Get("If-None-Match"), `"`) == entry.Digest {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	files, err := s.skillsStore().Read(entry.Digest)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("ETag", `"`+entry.Digest+`"`)
	out := map[string]any{"source": source, "version": entry.Version, "digest": entry.Digest, "imported_at": entry.ImportedAt, "skills": entry.Skills, "files": files}
	if project != "" {
		out["project"] = project
	}
	s.ok(w, out)
}

// getSkillsSet is the admin's read of one set (export on the hub host).
func (s *Server) getSkillsSet(w http.ResponseWriter, r *http.Request) {
	s.skillsSetAnswer(w, r, "")
}

// getProjectSkillsSet serves one set to a caller with a current grant on
// the project: ?source=release|instance for the current import, or
// ?digest= for a kept one (the version a manifest pins).
func (s *Server) getProjectSkillsSet(w http.ResponseWriter, r *http.Request) {
	if s.projectReadDenied(w, r, "skills set", false) {
		return
	}
	s.skillsSetAnswer(w, r, r.PathValue("p"))
}
