package server

// Anti-entropy sync over the hub API (DESIGN-hub-sync): thin streaming
// wrappers over the same store primitives the ssh legs shell out to —
// no new merge logic, no new formats. GET streams JSONL out, POST
// accepts JSONL in; both sides carry a `projects` filter because a hub
// holds many machines' projects (the ssh peer case never needed one).
// GET handlers open EXISTING projects only — a read must never
// resurrect a dropped id (the v0.1.26 lesson); imports create, that is
// their job.

import (
	"bufio"
	"encoding/json"
	"net/http"
	"strings"

	"aimem/internal/store"
)

// syncProjects resolves the request's projects filter: named ids that
// exist locally, or every project when the filter is absent (matches
// the unfiltered ssh pull legs on single-hub deployments).
func (s *Server) syncProjects(r *http.Request) ([]string, error) {
	all, err := s.reg.Projects()
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(r.URL.Query().Get("projects"))
	if raw == "" {
		return all, nil
	}
	have := map[string]bool{}
	for _, id := range all {
		have[id] = true
	}
	var out []string
	for _, id := range strings.Split(raw, ",") {
		if id = strings.TrimSpace(id); id != "" && have[id] {
			out = append(out, id)
		}
	}
	return out, nil
}

func (s *Server) syncEventsOut(w http.ResponseWriter, r *http.Request) {
	ids, err := s.syncProjects(r)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	since := r.URL.Query().Get("since")
	w.Header().Set("Content-Type", "application/x-ndjson")
	for _, id := range ids {
		db, err := s.reg.OpenExisting(id)
		if err != nil {
			continue
		}
		if err := db.DumpSince(w, since); err != nil {
			s.log.Warn("sync events out", "project", id, "err", err)
			return // mid-stream: the client's scanner stops at the break
		}
	}
}

func (s *Server) syncEventsIn(w http.ResponseWriter, r *http.Request) {
	sc := bufio.NewScanner(r.Body)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	var submitted, failed int
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req appendRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			failed++
			continue
		}
		if _, _, err := s.appendOne(&req); err != nil {
			failed++
			continue
		}
		submitted++
	}
	if err := sc.Err(); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.log.Info("sync events in", "submitted", submitted, "failed", failed)
	s.ok(w, map[string]any{"submitted": submitted, "failed": failed})
}

func (s *Server) syncMemoriesOut(w http.ResponseWriter, r *http.Request) {
	ids, err := s.syncProjects(r)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	for _, id := range ids {
		db, err := s.reg.OpenExisting(id)
		if err != nil {
			continue
		}
		if err := db.DumpMemories(enc); err != nil {
			s.log.Warn("sync memories out", "project", id, "err", err)
			return
		}
		// Curation run history travels alongside memories (id-idempotent)
		// so every machine's dashboard sees hub-side maintenance cost —
		// the same shape `export-memories` emits.
		runs, err := db.CurateRuns()
		if err != nil {
			continue
		}
		for i := range runs {
			if err := enc.Encode(map[string]any{"project_id": id, "curate_run": runs[i]}); err != nil {
				return
			}
		}
	}
}

func (s *Server) syncMemoriesIn(w http.ResponseWriter, r *http.Request) {
	sc := bufio.NewScanner(r.Body)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	var imported, failed int
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			ProjectID string          `json:"project_id"`
			Memory    *store.Memory   `json:"memory"`
			CurateRun *store.CurateRun `json:"curate_run"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil || rec.ProjectID == "" {
			failed++
			continue
		}
		db, err := s.reg.Open(rec.ProjectID)
		if err != nil {
			failed++
			continue
		}
		switch {
		case rec.CurateRun != nil:
			if rec.CurateRun.ID == "" || rec.CurateRun.TS == "" || db.AddCurateRun(rec.CurateRun) != nil {
				failed++
				continue
			}
		case rec.Memory != nil:
			if db.ImportMemory(rec.Memory) != nil {
				failed++
				continue
			}
		default:
			failed++
			continue
		}
		imported++
	}
	if err := sc.Err(); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.log.Info("sync memories in", "imported", imported, "failed", failed)
	s.ok(w, map[string]any{"imported": imported, "failed": failed})
}

func (s *Server) syncConfigOut(w http.ResponseWriter, r *http.Request) {
	ids, err := s.syncProjects(r)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	if err := store.ExportGroupConfig(s.reg, ids, json.NewEncoder(w)); err != nil {
		s.log.Warn("sync config out", "err", err)
	}
}

func (s *Server) syncConfigIn(w http.ResponseWriter, r *http.Request) {
	sc := bufio.NewScanner(r.Body)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	applied := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec store.ConfigRecord
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		msg, ok := store.ImportGroupConfigRecord(s.reg, rec)
		if msg != "" {
			s.log.Info("sync config in", "msg", msg)
		}
		if ok {
			applied++
		}
	}
	if err := sc.Err(); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.ok(w, map[string]any{"applied": applied})
}
