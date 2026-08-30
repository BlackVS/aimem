package server

// Shared-document endpoints (docs/DESIGN-shared-docs.md). The hub is the
// authority for a project's documents; writes are compare-and-swap and a
// conflict hands back the current document so the caller can merge. All
// routes are bearer-gated like the rest of the API.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"aimem/internal/diff3"
	"aimem/internal/store"
)

// conflictBodyCap bounds the body echoed inside a 409: enough to merge a
// handoff-sized doc in place, while a huge one is fetched with a GET.
const conflictBodyCap = 64 * 1024

// stampWriter makes updated_by attributable when the writer used a
// NAMED token (DESIGN-hub-sync): the authenticated name prefixes the
// client-supplied label, so "who overwrote my handoff" is answered by
// authentication, not honor. The env token and the local unix socket
// keep the legacy label untouched.
func stampWriter(r *http.Request, by string) string {
	if id, ok := IdentityFrom(r.Context()); ok && id.Name != "env" {
		return id.Name + "/" + by
	}
	return by
}

func (s *Server) listDocs(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	docs, err := db.ListDocs()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if docs == nil {
		docs = []store.Doc{}
	}
	s.ok(w, map[string]any{"docs": docs})
}

func (s *Server) getDoc(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	rev, _ := strconv.ParseInt(r.URL.Query().Get("rev"), 10, 64)
	doc, err := db.GetDoc(r.PathValue("name"), rev)
	if err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	s.ok(w, doc)
}

func (s *Server) docLog(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	log, err := db.DocLog(r.PathValue("name"))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if log == nil {
		log = []store.Doc{}
	}
	s.ok(w, map[string]any{"revisions": log})
}

func (s *Server) putDoc(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	var body struct {
		Body      string `json:"body"`
		BaseRev   int64  `json:"base_rev"`
		UpdatedBy string `json:"updated_by"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, store.MaxDocBytes*2)).Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	doc, err := db.PutDoc(r.PathValue("name"), body.Body, stampWriter(r, body.UpdatedBy), body.BaseRev, false)
	if err != nil {
		s.docWriteError(w, err)
		return
	}
	s.log.Info("doc published", "project", r.PathValue("p"), "doc", doc.Name,
		"rev", doc.Rev, "by", doc.UpdatedBy, "bytes", doc.Size)
	s.ok(w, map[string]any{"rev": doc.Rev, "updated_at": doc.UpdatedAt})
}

func (s *Server) deleteDoc(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	baseRev, _ := strconv.ParseInt(r.URL.Query().Get("base_rev"), 10, 64)
	by := stampWriter(r, r.URL.Query().Get("by"))
	doc, err := db.PutDoc(r.PathValue("name"), "", by, baseRev, true)
	if err != nil {
		s.docWriteError(w, err)
		return
	}
	s.log.Warn("doc retired", "project", r.PathValue("p"), "doc", doc.Name, "rev", doc.Rev, "by", by)
	s.ok(w, map[string]any{"rev": doc.Rev, "deleted": true})
}

// mergeDoc is a CALCULATOR, not a writer (DESIGN-doc-collab): given a
// draft and the revision it was based on, return the three-way merge
// against the current document — so a console 409 becomes one click
// instead of "copy your text somewhere". The hub still never merges on
// write; saving the result is a separate, ordinary CAS PUT.
func (s *Server) mergeDoc(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	var req struct {
		Body    string `json:"body"`
		BaseRev int64  `json:"base_rev"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, store.MaxDocBytes*2)).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	name := r.PathValue("name")
	cur, err := db.GetDoc(name, 0)
	if err != nil {
		s.fail(w, http.StatusNotFound, err)
		return
	}
	base, baseFound := "", false
	if req.BaseRev > 0 && req.BaseRev != cur.Rev {
		if bd, err := db.GetDoc(name, req.BaseRev); err == nil && !bd.Deleted {
			base, baseFound = bd.Body, true
		}
	}
	merged, conflicts, err := diff3.MergeText(base, req.Body, cur.Body,
		"mine", fmt.Sprintf("hub rev %d by %s", cur.Rev, cur.UpdatedBy))
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.ok(w, map[string]any{
		"merged": merged, "conflicts": conflicts,
		"against_rev": cur.Rev, "base_found": baseFound,
	})
}

// docWriteError maps a CAS refusal to 409 carrying the current document
// (body capped), everything else to 400. The 409 payload is the merge
// input: rev to rebase onto, writer to blame, body to diff against.
func (s *Server) docWriteError(w http.ResponseWriter, err error) {
	var conflict *store.DocConflict
	if errors.As(err, &conflict) {
		cur := conflict.Current
		truncated := false
		if len(cur.Body) > conflictBodyCap {
			cut := cur.Body[:conflictBodyCap]
			// Never split a UTF-8 rune: a cut mid-sequence would decode as
			// U+FFFD in the JSON the caller is asked to merge from.
			for len(cut) > 0 && cut[len(cut)-1] >= 0x80 && cut[len(cut)-1] < 0xC0 {
				cut = cut[:len(cut)-1]
			}
			cur.Body = cut
			truncated = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{
			"error": err.Error(), "rev": cur.Rev, "updated_at": cur.UpdatedAt,
			"updated_by": cur.UpdatedBy, "deleted": cur.Deleted,
			"body": cur.Body, "body_truncated": truncated,
		})
		return
	}
	s.fail(w, http.StatusBadRequest, err)
}
