package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"aimem/internal/projectrepo"
	"aimem/internal/store"
)

// A project's one repository (docs/DESIGN-AIFORGE-PILOT-1.md §1): a project
// property next to the process reference, stored as given and never
// fetched. Setting and clearing are admin actions; every change is kept in
// a bounded history with the actor and the values before and after, in the
// same storage step as the change. Reading is open to an ordinary token
// with a grant on the project; team mode does not serve it yet.
const (
	repositoryMetaKey    = "repository"
	repositoryHistoryKey = "repository_history"
	repositoryHistoryMax = 50
)

// repositoryView is the stored property plus the host its clone URL names:
// the host identifies the credential a member needs.
type repositoryView struct {
	projectrepo.Repository
	Host string `json:"host"`
}

func viewRepository(r *projectrepo.Repository) *repositoryView {
	if r == nil {
		return nil
	}
	host, _ := projectrepo.Host(r.URL)
	return &repositoryView{Repository: *r, Host: host}
}

func decodeRepository(v string) (*projectrepo.Repository, error) {
	if v == "" {
		return nil, nil
	}
	var r projectrepo.Repository
	if err := json.Unmarshal([]byte(v), &r); err != nil {
		return nil, fmt.Errorf("stored repository is unreadable: %w", err)
	}
	return &r, nil
}

// repositoryReadDenied applies the read rule: an ordinary token needs the
// knowledge-read predicate on the project (a live token whose scope reaches
// it and a live direct or group grant). An unknown project reads the same
// as one without a grant. Admin credentials and the local socket pass.
func (s *Server) repositoryReadDenied(w http.ResponseWriter, r *http.Request) bool {
	id, ok := IdentityFrom(r.Context())
	p := r.PathValue("p")
	if tc, team := teamContextFrom(r.Context()); team {
		// A team session reads the repository of a project its profile is
		// granted, and nothing else: no history, no reserved store, and an
		// unknown project reads as one without a grant.
		detail := teamAuditDetail(tc.Context, r) + fmt.Sprintf(" project=%q", p)
		switch {
		case r.URL.Query().Get("history") != "":
			s.teamDeny(w, r, tc.Role, id, "invalid_request", detail+" reason=history", tc.CorrelationID)
			return true
		case store.IsReservedProject(p):
			s.teamDeny(w, r, tc.Role, id, "grant_denied", detail+" reason=reserved", tc.CorrelationID)
			return true
		}
		instance, err := s.knowledgeInstance(p)
		switch {
		case err != nil:
			s.teamDeny(w, r, tc.Role, id, "identity_unavailable", detail+" reason=grant_store", tc.CorrelationID)
			return true
		case instance == "":
			s.teamDeny(w, r, tc.Role, id, "grant_denied", detail, tc.CorrelationID)
			return true
		}
		return s.teamGrantDenied(w, r, p)
	}
	if !ok || id.Role != "user" {
		return false
	}
	if store.IsReservedProject(p) {
		s.fail(w, http.StatusBadRequest, store.ErrTaskReservedScope)
		return true
	}
	if r.URL.Query().Get("history") != "" {
		s.fail(w, http.StatusForbidden, errors.New("the repository history is an admin read"))
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
		if allowed, err = db.CanWriteToken(id.UserID, id.TokenID, instance); err != nil {
			s.log.Error("repository read authorization", "project", p, "err", err)
			s.fail(w, http.StatusInternalServerError, errors.New("cannot check project access"))
			return true
		}
	}
	if !allowed {
		s.fail(w, http.StatusForbidden, errors.New("token scope or current grant does not permit reading this project's repository"))
		return true
	}
	return false
}

// getRepository answers the project's repository, null when none is set;
// ?history=1 adds the recorded changes, newest first (admin only).
func (s *Server) getRepository(w http.ResponseWriter, r *http.Request) {
	if s.repositoryReadDenied(w, r) {
		return
	}
	db := s.processProject(w, r)
	if db == nil {
		return
	}
	v, err := db.GetMeta(repositoryMetaKey)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	cur, err := decodeRepository(v)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	out := map[string]any{"project": r.PathValue("p"), "repository": viewRepository(cur)}
	if r.URL.Query().Get("history") != "" {
		hist, err := db.GetMeta(repositoryHistoryKey)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		entries := []projectrepo.Change{}
		if hist != "" {
			if err := json.Unmarshal([]byte(hist), &entries); err != nil {
				s.fail(w, http.StatusInternalServerError, fmt.Errorf("stored repository history is unreadable: %w", err))
				return
			}
		}
		out["history"] = entries
	}
	s.ok(w, out)
}

var errNoRepository = errors.New("no repository is set for this project")

// putRepository sets or clears the repository. The change and its history
// entry are one storage step, so no change goes unrecorded.
func (s *Server) putRepository(w http.ResponseWriter, r *http.Request) {
	db := s.processProject(w, r)
	if db == nil {
		return
	}
	var req struct {
		Kind   string `json:"kind"`
		URL    string `json:"url"`
		Access string `json:"access"`
		Clear  bool   `json:"clear"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	by := taskActor(r).Name
	var next *projectrepo.Repository
	if req.Clear {
		if req.Kind != "" || req.URL != "" || req.Access != "" {
			s.fail(w, http.StatusBadRequest, errors.New("clear takes no kind, url or access"))
			return
		}
	} else {
		next = &projectrepo.Repository{Kind: req.Kind, URL: req.URL, Access: req.Access, SetAt: now, SetBy: by}
		if next.Access == "" {
			next.Access = "write"
		}
		if err := next.Validate(); err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
	}
	change := projectrepo.Change{Action: projectrepo.ActionSet, At: now, By: by, New: next}
	if req.Clear {
		change.Action = projectrepo.ActionClear
	}
	err := db.MetaTx(func(get func(string) (string, error), set func(string, string) error) error {
		v, err := get(repositoryMetaKey)
		if err != nil {
			return err
		}
		if change.Old, err = decodeRepository(v); err != nil {
			return err
		}
		if req.Clear && change.Old == nil {
			return errNoRepository
		}
		value := ""
		if next != nil {
			raw, err := json.Marshal(next)
			if err != nil {
				return err
			}
			value = string(raw)
		}
		if err := set(repositoryMetaKey, value); err != nil {
			return err
		}
		var hist []projectrepo.Change
		if h, err := get(repositoryHistoryKey); err != nil {
			return err
		} else if h != "" {
			if err := json.Unmarshal([]byte(h), &hist); err != nil {
				return fmt.Errorf("stored repository history is unreadable: %w", err)
			}
		}
		hist = append([]projectrepo.Change{change}, hist...)
		if len(hist) > repositoryHistoryMax {
			hist = hist[:repositoryHistoryMax]
		}
		raw, err := json.Marshal(hist)
		if err != nil {
			return err
		}
		return set(repositoryHistoryKey, string(raw))
	})
	switch {
	case errors.Is(err, errNoRepository):
		s.fail(w, http.StatusNotFound, err)
		return
	case err != nil:
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("project repository changed", "project", r.PathValue("p"), "action", change.Action, "by", by)
	s.ok(w, map[string]any{"project": r.PathValue("p"), "action": change.Action, "repository": viewRepository(next), "previous": viewRepository(change.Old)})
}

// getProjectGrants lists who is granted the project: users, groups and team
// profiles, with names beside IDs (admin only). A project that has never
// been granted has no access instance and lists nobody.
func (s *Server) getProjectGrants(w http.ResponseWriter, r *http.Request) {
	if s.processProject(w, r) == nil {
		return
	}
	p := r.PathValue("p")
	instance, err := s.reg.ExistingProjectAccessID(p)
	if err != nil {
		s.log.Error("project grants", "project", p, "err", err)
		s.fail(w, http.StatusInternalServerError, errors.New("project access identity unavailable"))
		return
	}
	db, ok := s.accessStore(w)
	if !ok {
		return
	}
	grants, err := db.ProjectGrantees(instance)
	if err != nil {
		s.log.Error("project grants", "project", p, "err", err)
		s.fail(w, http.StatusInternalServerError, errors.New("cannot list the project's grants"))
		return
	}
	s.ok(w, map[string]any{"project": p, "instance": instance, "grants": grants})
}
