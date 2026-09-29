package server

// The first pilot's knowledge reads (task 19d8, docs/DESIGN-AIFORGE-KNOWLEDGE.md).
// An ordinary user token, personal or in a verified team context, reaches
// exactly three existing reads: recall, the doc list and one doc at its
// current revision. Each rechecks the effective profile's live grant on every
// request. Legacy writer and admin tokens and the local socket are unchanged
// (K9); every other knowledge route stays refused to scoped callers.

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"

	"aimem/internal/store"
)

// knowledgeRoutes is the pilot set (K1a): read-only and project-scoped.
var knowledgeRoutes = []string{
	"GET /v1/projects/{p}/memories/recall",
	"GET /v1/projects/{p}/docs",
	"GET /v1/projects/{p}/docs/{name}",
}

var knowledgeRoute = func() map[string]bool {
	m := map[string]bool{}
	for _, p := range knowledgeRoutes {
		m[p] = true
	}
	return m
}()

// errCurrentRevisionOnly refuses ?rev= to a scoped caller: it reads the
// current revision only (seq232).
var errCurrentRevisionOnly = errors.New("this credential reads a document's current revision only; ?rev= is not available to it")

// knowledgeReadDenied applies the scoped read check (K2) to the project in
// the path and answers the refusal when it denies. currentOnly marks the doc
// read, where a scoped caller may not name a revision. Legacy credentials
// and the local socket pass through untouched.
func (s *Server) knowledgeReadDenied(w http.ResponseWriter, r *http.Request, currentOnly bool) bool {
	id, ok := IdentityFrom(r.Context())
	tc, team := teamContextFrom(r.Context())
	if !team && (!ok || id.Role != "user") {
		return false
	}
	p := r.PathValue("p")
	_, rev := r.URL.Query()["rev"]
	rev = rev && currentOnly
	if team {
		detail := teamAuditDetail(tc.Context, r) + fmt.Sprintf(" project=%q", p)
		switch {
		case rev:
			s.teamDeny(w, r, tc.Role, id, "invalid_request", detail+" reason=revision", tc.CorrelationID)
			return true
		case store.IsReservedProject(p):
			// No profile is ever granted the personal store or a group space.
			s.teamDeny(w, r, tc.Role, id, "grant_denied", detail+" reason=reserved", tc.CorrelationID)
			return true
		}
		instance, err := s.knowledgeInstance(p)
		switch {
		case err != nil:
			s.teamDeny(w, r, tc.Role, id, "identity_unavailable", detail+" reason=grant_store", tc.CorrelationID)
			return true
		case instance == "":
			// An unknown project reads the same as one without a grant.
			s.teamDeny(w, r, tc.Role, id, "grant_denied", detail, tc.CorrelationID)
			return true
		}
		return s.teamGrantDenied(w, r, p)
	}
	switch {
	case rev:
		s.fail(w, http.StatusBadRequest, errCurrentRevisionOnly)
		return true
	case store.IsReservedProject(p):
		s.fail(w, http.StatusBadRequest, store.ErrTaskReservedScope)
		return true
	}
	instance, err := s.knowledgeInstance(p)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, errors.New("project access identity unavailable"))
		return true
	}
	allowed := false
	if instance != "" {
		db, ok := s.accessStore(w)
		if !ok {
			return true
		}
		// The task write predicate, used here for a read: a live token of an
		// enabled user, scoped to the user or to this project, and a live
		// direct or access-group grant. A read-only-scope token fails it.
		if allowed, err = db.CanWriteToken(id.UserID, id.TokenID, instance); err != nil {
			s.log.Error("knowledge read authorization", "project", p, "err", err)
			s.fail(w, http.StatusInternalServerError, errors.New("cannot check project access"))
			return true
		}
	}
	if !allowed {
		// An unknown project reads the same as one without a grant.
		s.fail(w, http.StatusForbidden, errors.New("token scope or current grant does not permit reading this project's knowledge"))
		return true
	}
	return false
}

// knowledgeInstance is p's access instance, or "" for a project that does
// not exist or cannot have one: a scoped caller learns nothing about which
// projects exist from a refusal.
func (s *Server) knowledgeInstance(p string) (string, error) {
	instance, err := s.reg.ExistingProjectAccessID(p)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, store.ErrInvalidAccessProject) {
		return "", nil
	}
	if err != nil {
		s.log.Error("knowledge read authorization", "project", p, "err", err)
	}
	return instance, err
}
