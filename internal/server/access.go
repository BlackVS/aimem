package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"aimem/internal/access"
	"aimem/internal/store"
)

// Access management is admin-only. No request field can create an admin token.
// The local operator uses the same routes through the existing trusted socket.
func (s *Server) accessStore(w http.ResponseWriter) (*access.Store, bool) {
	db, err := s.openAccess(true)
	if err != nil {
		s.fail(w, 500, fmt.Errorf("access store unavailable"))
		s.log.Error("open access store", "err", err)
		return nil, false
	}
	return db, true
}

func (s *Server) openAccess(create bool) (*access.Store, error) {
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if s.accessClosed {
		return nil, fmt.Errorf("server is closed")
	}
	if s.accessDB != nil {
		return s.accessDB, nil
	}
	open := access.OpenExisting
	if create {
		open = access.Open
	}
	db, err := open(s.reg.Root())
	if err != nil {
		return nil, err
	}
	s.accessDB = db
	return db, nil
}

// Close releases server-owned storage after its HTTP listeners have stopped.
func (s *Server) Close() error {
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if s.accessClosed {
		return nil
	}
	s.accessClosed = true
	if s.accessDB != nil {
		return s.accessDB.Close()
	}
	return nil
}

func (s *Server) removeAccessGrant(w http.ResponseWriter, r *http.Request) {
	db, ok := s.accessStore(w)
	if !ok {
		return
	}
	if err := db.SetGrant(accessActor(r), r.PathValue("instance"), r.PathValue("kind"), r.PathValue("id"), false); err != nil {
		s.accessError(w, err)
		return
	}
	s.ok(w, map[string]bool{"ok": true})
}
func accessActor(r *http.Request) string {
	if id, ok := IdentityFrom(r.Context()); ok {
		return id.Name
	}
	return "local-operator"
}
func (s *Server) accessError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, access.ErrDenied) {
		status = http.StatusForbidden
	}
	s.fail(w, status, err)
}
func (s *Server) decodeAccess(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		s.fail(w, 400, fmt.Errorf("invalid access request: %w", err))
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		s.fail(w, 400, fmt.Errorf("request must contain one JSON object"))
		return false
	}
	return true
}
func (s *Server) getAccess(w http.ResponseWriter, r *http.Request) {
	db, ok := s.accessStore(w)
	if !ok {
		return
	}
	result, err := db.Snapshot()
	if err != nil {
		s.fail(w, 500, fmt.Errorf("cannot read access state"))
		return
	}
	projects, err := s.reg.Projects()
	if err != nil {
		s.fail(w, 500, fmt.Errorf("cannot list access projects"))
		return
	}
	instances := make(map[string]string)
	for _, project := range projects {
		if project == store.UserScopeProject || strings.HasPrefix(project, "group-") {
			continue
		}
		instance, err := s.reg.ExistingProjectAccessID(project)
		if err != nil {
			s.log.Warn("cannot read project access identity", "err", err)
			continue
		}
		if instance == "" {
			continue
		}
		instances[instance] = project
	}
	s.ok(w, struct {
		access.Snapshot
		Projects map[string]string `json:"projects"`
	}{result, instances})
}
func (s *Server) createAccessUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !s.decodeAccess(w, r, &req) {
		return
	}
	db, ok := s.accessStore(w)
	if !ok {
		return
	}
	u, err := db.CreateUser(accessActor(r), req.Name)
	if err != nil {
		s.accessError(w, err)
		return
	}
	s.ok(w, u)
}
func (s *Server) updateAccessUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Disabled *bool  `json:"disabled"`
	}
	if !s.decodeAccess(w, r, &req) {
		return
	}
	if req.Disabled == nil {
		s.fail(w, 400, fmt.Errorf("disabled is required"))
		return
	}
	db, ok := s.accessStore(w)
	if !ok {
		return
	}
	if err := db.SetUser(accessActor(r), r.PathValue("id"), req.Name, *req.Disabled); err != nil {
		s.accessError(w, err)
		return
	}
	s.ok(w, map[string]bool{"ok": true})
}
func (s *Server) createAccessGroup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !s.decodeAccess(w, r, &req) {
		return
	}
	db, ok := s.accessStore(w)
	if !ok {
		return
	}
	g, err := db.CreateGroup(accessActor(r), req.Name)
	if err != nil {
		s.accessError(w, err)
		return
	}
	s.ok(w, g)
}
func (s *Server) setAccessMember(w http.ResponseWriter, r *http.Request) {
	db, ok := s.accessStore(w)
	if !ok {
		return
	}
	if err := db.SetMember(accessActor(r), r.PathValue("g"), r.PathValue("u"), r.Method == "PUT"); err != nil {
		s.accessError(w, err)
		return
	}
	s.ok(w, map[string]bool{"ok": true})
}
func (s *Server) setAccessGrant(w http.ResponseWriter, r *http.Request) {
	project, err := s.reg.ProjectAccessID(r.PathValue("p"))
	if err != nil {
		s.log.Error("resolve access project", "err", err)
		s.fail(w, 400, fmt.Errorf("project is unavailable for access assignments"))
		return
	}
	db, ok := s.accessStore(w)
	if !ok {
		return
	}
	if err := db.SetGrant(accessActor(r), project, r.PathValue("kind"), r.PathValue("id"), r.Method == "PUT"); err != nil {
		s.accessError(w, err)
		return
	}
	s.ok(w, map[string]any{"ok": true, "project_instance": project})
}
func (s *Server) issueAccessToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID    string    `json:"user_id"`
		Label     string    `json:"label"`
		Project   string    `json:"project"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if !s.decodeAccess(w, r, &req) {
		return
	}
	project := ""
	if req.Project != "" {
		var err error
		project, err = s.reg.ProjectAccessID(req.Project)
		if err != nil {
			s.log.Error("resolve access project", "err", err)
			s.fail(w, 400, fmt.Errorf("project is unavailable for access assignments"))
			return
		}
	}
	db, ok := s.accessStore(w)
	if !ok {
		return
	}
	token, secret, err := db.Issue(accessActor(r), req.UserID, req.Label, project, req.ExpiresAt)
	if err != nil {
		s.accessError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, map[string]any{"token": token, "secret": secret})
}
func (s *Server) revokeAccessToken(w http.ResponseWriter, r *http.Request) {
	db, ok := s.accessStore(w)
	if !ok {
		return
	}
	if err := db.Revoke(accessActor(r), r.PathValue("id")); err != nil {
		s.accessError(w, err)
		return
	}
	s.ok(w, map[string]bool{"ok": true})
}

// accessIdentity exposes no broad legacy API authority. This first increment
// lets clients verify their token and current task-write project before task
// routes are added. Ordinary tokens are denied every other existing data route.
func (s *Server) accessIdentity(w http.ResponseWriter, r *http.Request) {
	id, ok := IdentityFrom(r.Context())
	if !ok {
		s.fail(w, 401, fmt.Errorf("bearer authentication required"))
		return
	}
	if id.Role != "user" {
		s.ok(w, map[string]any{"name": id.Name, "role": id.Role})
		return
	}
	project := r.URL.Query().Get("project")
	allowed := false
	if project != "" {
		instance, err := s.reg.ExistingProjectAccessID(project)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, store.ErrInvalidAccessProject) {
				s.fail(w, 404, fmt.Errorf("unknown project"))
			} else {
				s.log.Error("read project access identity", "err", err)
				s.fail(w, 500, fmt.Errorf("project access identity unavailable"))
			}
			return
		}
		if instance != "" {
			db, ok := s.accessStore(w)
			if !ok {
				return
			}
			_, err = db.Authorize(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), instance)
			if err != nil && !errors.Is(err, access.ErrDenied) {
				s.fail(w, 500, fmt.Errorf("cannot check project access"))
				return
			}
			allowed = err == nil
		}
	}
	s.ok(w, map[string]any{"user_id": id.UserID, "token_id": id.TokenID, "name": id.Name, "role": "user", "task_read": "all-projects", "project": project, "task_write": allowed})
}
