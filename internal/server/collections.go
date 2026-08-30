package server

// Structured-collection endpoints (docs/DESIGN-structured-docs.md). The
// hub is the authority for a project's (or group's) collections; the unit
// of compare-and-swap is the record, so concurrent writers conflict only
// when they touch the SAME record — and a 409 hands back the current
// record, which is small enough to re-apply intent onto directly.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"aimem/internal/store"
)

func (s *Server) listCollections(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	cols, err := db.ListCollections()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if cols == nil {
		cols = []store.ColSummary{}
	}
	s.ok(w, map[string]any{"collections": cols})
}

func (s *Server) listRecords(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	withBodies := r.URL.Query().Get("bodies") == "1"
	recs, err := db.ListRecords(r.PathValue("c"), withBodies)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if recs == nil {
		recs = []store.Record{}
	}
	s.ok(w, map[string]any{"records": recs})
}

func (s *Server) getRecord(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	rev, _ := strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
	rec, err := db.GetRecord(r.PathValue("c"), r.PathValue("id"), rev)
	if err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	s.ok(w, rec)
}

// recordLog lives at .../collections/{c}/log/{id...} rather than a
// /log suffix on the record path: the record id is a slash path and the
// {id...} wildcard must end the pattern.
func (s *Server) recordLog(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	log, err := db.RecordLog(r.PathValue("c"), r.PathValue("id"))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if log == nil {
		log = []store.Record{}
	}
	s.ok(w, map[string]any{"revisions": log})
}

func (s *Server) putRecord(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	var req struct {
		Body      json.RawMessage `json:"body"`
		BaseRev   int64           `json:"base_rev"`
		UpdatedBy string          `json:"updated_by"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, store.MaxRecordBytes*2)).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	rec, err := db.PutRecord(r.PathValue("c"), r.PathValue("id"), req.Body,
		stampWriter(r, req.UpdatedBy), req.BaseRev, false)
	if err != nil {
		s.recordWriteError(w, err)
		return
	}
	s.log.Info("record written", "project", r.PathValue("p"), "collection", rec.Collection,
		"record", rec.ID, "rev", rec.Rev, "by", rec.UpdatedBy, "bytes", rec.Size)
	s.ok(w, map[string]any{"rev": rec.Rev, "updated_at": rec.UpdatedAt})
}

func (s *Server) deleteRecord(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	baseRev, _ := strconv.ParseInt(r.URL.Query().Get("base_rev"), 10, 64)
	by := stampWriter(r, r.URL.Query().Get("by"))
	rec, err := db.PutRecord(r.PathValue("c"), r.PathValue("id"), nil, by, baseRev, true)
	if err != nil {
		s.recordWriteError(w, err)
		return
	}
	s.log.Warn("record retired", "project", r.PathValue("p"), "collection", rec.Collection,
		"record", rec.ID, "rev", rec.Rev, "by", by)
	s.ok(w, map[string]any{"rev": rec.Rev, "deleted": true})
}

// recordWriteError maps a CAS refusal to a 409 carrying the whole current
// record (no truncation dance — records are capped at 32KB), everything
// else to 400.
func (s *Server) recordWriteError(w http.ResponseWriter, err error) {
	var conflict *store.RecordConflict
	if errors.As(err, &conflict) {
		cur := conflict.Current
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{
			"error": err.Error(), "id": cur.ID, "rev": cur.Rev,
			"updated_at": cur.UpdatedAt, "updated_by": cur.UpdatedBy,
			"deleted": cur.Deleted, "body": cur.Body,
		})
		return
	}
	s.fail(w, http.StatusBadRequest, err)
}
