package server

// Team access profiles: the hub admin's surface for linking an aicrew team to
// this hub and granting it projects (task 01a0deb1, decisions P1-P4). Every
// route is hub-admin only (the route table marks it Admin) and, like the peer
// routes, requires TLS terminated by this hub. A profile hangs under its
// registered peer and is keyed by aicrew's team ID; there is no delete. A
// grant names a project and is stored against that project's access
// instance. The team-mode verifier reads profiles and grants live on every
// request, so a change here takes effect on the next team request.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"

	"aimem/internal/access"
	"aimem/internal/store"
)

// accessInstanceShape is a project access instance ID (a UUIDv7).
var accessInstanceShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type teamGrantView struct {
	Project  string `json:"project,omitempty"` // empty when the project no longer exists
	Instance string `json:"instance"`
}

type teamProfileView struct {
	ProfileID string          `json:"profile_id"`
	ServiceID string          `json:"service_id"`
	TeamID    string          `json:"team_id"`
	Disabled  bool            `json:"disabled"`
	Grants    []teamGrantView `json:"grants"`
}

// projectNamesByInstance maps every existing project's access instance to its
// name, so a listing can name each grant and expose orphaned instances.
func (s *Server) projectNamesByInstance() (map[string]string, error) {
	names, err := s.reg.Projects()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range names {
		if instance, err := s.reg.ExistingProjectAccessID(p); err == nil && instance != "" {
			out[instance] = p
		}
	}
	return out, nil
}

func (s *Server) teamProfileView(db *access.Store, p access.TeamProfile, names map[string]string) (teamProfileView, error) {
	instances, err := db.TeamGrantInstances(p.ID)
	if err != nil {
		return teamProfileView{}, err
	}
	v := teamProfileView{ProfileID: p.ID, ServiceID: p.ServiceID, TeamID: p.TeamID, Disabled: p.Disabled, Grants: []teamGrantView{}}
	for _, i := range instances {
		v.Grants = append(v.Grants, teamGrantView{Project: names[i], Instance: i})
	}
	return v, nil
}

// teamPeerStore applies the admin routes' TLS rule, opens the store and
// requires the path's service to be a registered peer.
func (s *Server) teamPeerStore(w http.ResponseWriter, r *http.Request) (*access.Store, bool) {
	db, ok := s.peerAdminStore(w, r)
	if !ok {
		return nil, false
	}
	peers, err := db.ListIdentityPeers()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot read identity peers"))
		return nil, false
	}
	for _, p := range peers {
		if p.ServiceID == r.PathValue("service_id") {
			return db, true
		}
	}
	s.fail(w, http.StatusNotFound, fmt.Errorf("unknown identity peer"))
	return nil, false
}

// teamProfileFor resolves the path's (service, team) to its profile.
func (s *Server) teamProfileFor(w http.ResponseWriter, r *http.Request) (*access.Store, access.TeamProfile, bool) {
	db, ok := s.teamPeerStore(w, r)
	if !ok {
		return nil, access.TeamProfile{}, false
	}
	p, err := db.TeamProfileByKey(r.PathValue("service_id"), r.PathValue("team_id"))
	if err != nil {
		s.fail(w, http.StatusNotFound, fmt.Errorf("unknown team profile"))
		return nil, access.TeamProfile{}, false
	}
	return db, p, true
}

func (s *Server) listTeamProfiles(w http.ResponseWriter, r *http.Request) {
	db, ok := s.teamPeerStore(w, r)
	if !ok {
		return
	}
	profiles, err := db.ListTeamProfiles(r.PathValue("service_id"))
	if err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot list team profiles"))
		return
	}
	names, err := s.projectNamesByInstance()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot list projects"))
		return
	}
	out := []teamProfileView{}
	for _, p := range profiles {
		v, err := s.teamProfileView(db, p, names)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot list team grants"))
			return
		}
		out = append(out, v)
	}
	s.ok(w, map[string]any{"service_id": r.PathValue("service_id"), "teams": out})
}

func (s *Server) createTeamProfile(w http.ResponseWriter, r *http.Request) {
	db, ok := s.teamPeerStore(w, r)
	if !ok {
		return
	}
	var req struct {
		TeamID string `json:"team_id"`
	}
	if !decodeIdentity(r, w, &req) {
		s.fail(w, http.StatusBadRequest, fmt.Errorf(`body must be {"team_id": "..."}`))
		return
	}
	p, err := db.CreateTeamProfile(accessActor(r), r.PathValue("service_id"), req.TeamID)
	switch {
	case errors.Is(err, access.ErrTeamProfileExists):
		s.fail(w, http.StatusConflict, err)
		return
	case errors.Is(err, access.ErrInvalidRequest):
		s.fail(w, http.StatusBadRequest, err)
		return
	case errors.Is(err, access.ErrPeerUnknown):
		s.fail(w, http.StatusNotFound, fmt.Errorf("unknown identity peer"))
		return
	case err != nil:
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot create team profile"))
		return
	}
	s.created(w, teamProfileView{ProfileID: p.ID, ServiceID: p.ServiceID, TeamID: p.TeamID, Grants: []teamGrantView{}})
}

func (s *Server) updateTeamProfile(w http.ResponseWriter, r *http.Request) {
	db, p, ok := s.teamProfileFor(w, r)
	if !ok {
		return
	}
	var req struct {
		Disabled *bool `json:"disabled"`
	}
	if !decodeIdentity(r, w, &req) || req.Disabled == nil {
		s.fail(w, http.StatusBadRequest, fmt.Errorf(`body must be {"disabled": true|false}`))
		return
	}
	if err := db.SetTeamProfileDisabled(accessActor(r), p.ID, *req.Disabled); err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot update team profile"))
		return
	}
	s.ok(w, map[string]any{"ok": true, "team_id": p.TeamID, "disabled": *req.Disabled})
}

func (s *Server) listTeamGrants(w http.ResponseWriter, r *http.Request) {
	db, p, ok := s.teamProfileFor(w, r)
	if !ok {
		return
	}
	names, err := s.projectNamesByInstance()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot list projects"))
		return
	}
	v, err := s.teamProfileView(db, p, names)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot list team grants"))
		return
	}
	s.ok(w, v)
}

// grantTeamProject resolves the project to its access instance, minting the
// instance if the project has none yet, as a personal grant does.
func (s *Server) grantTeamProject(w http.ResponseWriter, r *http.Request) {
	db, p, ok := s.teamProfileFor(w, r)
	if !ok {
		return
	}
	project := r.PathValue("project")
	if store.IsReservedProject(project) {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("reserved projects cannot be granted"))
		return
	}
	instance, err := s.reg.ProjectAccessID(project)
	if err != nil {
		s.projectLookupFailed(w, project, err)
		return
	}
	if err := db.SetTeamGrant(accessActor(r), instance, p.ID, true); err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot grant the project"))
		return
	}
	s.ok(w, map[string]any{"ok": true, "team_id": p.TeamID, "project": project, "instance": instance})
}

// revokeTeamProject revokes by project name. It never mints an instance: a
// project that never had one cannot hold a grant, and the answer says so.
func (s *Server) revokeTeamProject(w http.ResponseWriter, r *http.Request) {
	db, p, ok := s.teamProfileFor(w, r)
	if !ok {
		return
	}
	project := r.PathValue("project")
	if store.IsReservedProject(project) {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("reserved projects cannot be granted"))
		return
	}
	instance, err := s.reg.ExistingProjectAccessID(project)
	if err != nil {
		s.projectLookupFailed(w, project, err)
		return
	}
	if instance == "" {
		s.ok(w, map[string]any{"ok": true, "team_id": p.TeamID, "project": project, "revoked": false})
		return
	}
	if err := db.SetTeamGrant(accessActor(r), instance, p.ID, false); err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot revoke the project"))
		return
	}
	s.ok(w, map[string]any{"ok": true, "team_id": p.TeamID, "project": project, "instance": instance, "revoked": true})
}

// revokeTeamInstance revokes a grant by its instance, for a project that was
// renamed away or deleted.
func (s *Server) revokeTeamInstance(w http.ResponseWriter, r *http.Request) {
	db, p, ok := s.teamProfileFor(w, r)
	if !ok {
		return
	}
	instance := r.PathValue("instance")
	if !accessInstanceShape.MatchString(instance) {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("instance must be a project access ID"))
		return
	}
	if err := db.SetTeamGrant(accessActor(r), instance, p.ID, false); err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot revoke the instance"))
		return
	}
	s.ok(w, map[string]any{"ok": true, "team_id": p.TeamID, "instance": instance, "revoked": true})
}

// projectLookupFailed answers a failed project resolution: an unknown or
// ineligible project is 404, anything else a storage fault.
func (s *Server) projectLookupFailed(w http.ResponseWriter, project string, err error) {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, store.ErrInvalidAccessProject) {
		s.fail(w, http.StatusNotFound, fmt.Errorf("unknown project; an orphaned grant is revoked by its instance"))
		return
	}
	s.log.Error("resolve team grant project", "project", project, "err", err)
	s.fail(w, http.StatusInternalServerError, fmt.Errorf("project access identity unavailable"))
}
