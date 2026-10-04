package server

// aicrewd's two bounded peer operations of the first pilot
// (docs/DESIGN-AIFORGE-PILOT-1.md §3; identity.v1 "Team registration and
// read"). team.register names the calling peer's own team on the hub;
// team.read returns the calling peer's own teams with their grants and each
// granted project's repository and process pin. Neither can attach a
// project, re-enable a profile or reach another peer's teams; grants stay
// the hub admin's. Both run over TLS this hub terminated, with identity
// version 1, for a credential issued for exactly that operation, and share
// the read scope's per-credential bound.

import (
	"net/http"
	"slices"

	"aimem/internal/access"
	"aimem/internal/process"
)

// teamPeer applies the checks both operations share and returns the
// credential's peer. It has answered the refusal when it returns false.
func (s *Server) teamPeer(w http.ResponseWriter, r *http.Request) (access.PeerIdentity, bool) {
	if !s.identityWireGate(w, r) {
		return access.PeerIdentity{}, false
	}
	id, ok := IdentityFrom(r.Context())
	switch {
	case !ok || id.Role != "peer":
		s.identityRefuse(w, "peer_unauthenticated")
		return access.PeerIdentity{}, false
	case !peerRouteAllowed(r, id.Peer), r.PathValue("service_id") != id.Peer.ServiceID:
		s.identityRefuse(w, "peer_forbidden")
		return access.PeerIdentity{}, false
	case !s.readLimiter().allow(id.Peer.CredentialID):
		s.identityRefuse(w, "rate_limited")
		return access.PeerIdentity{}, false
	}
	return id.Peer, true
}

// teamRegistrationView is team.register's answer.
type teamRegistrationView struct {
	ProfileID string `json:"profile_id"`
	TeamID    string `json:"team_id"`
	TeamName  string `json:"team_name"`
	Created   bool   `json:"created"`
	OldName   string `json:"previous_name"`
}

// registerTeam is PUT /v1/identity/peers/{service_id}/team-registrations/{team_id}.
func (s *Server) registerTeam(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.teamPeer(w, r)
	if !ok {
		return
	}
	var req struct {
		TeamName string `json:"team_name"`
	}
	if !decodeIdentity(r, w, &req) {
		s.identityRefuse(w, "invalid_request")
		return
	}
	db, err := s.openAccess(false)
	if err != nil {
		s.identityRefuse(w, "identity_unavailable")
		return
	}
	reg, err := db.RegisterTeam("peer:"+peer.ServiceID, peer.ServiceID, r.PathValue("team_id"), req.TeamName)
	if err != nil {
		s.identityRefuse(w, identityStoreError(err))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, teamRegistrationView{reg.Profile.ID, reg.Profile.TeamID, reg.Profile.TeamName, reg.Created, reg.OldName})
}

// teamReadProject is one granted project as team.read reports it.
type teamReadProject struct {
	Project    string          `json:"project"`
	Repository *repositoryView `json:"repository"`
	Process    *process.Ref    `json:"process"`
}

type teamReadView struct {
	TeamID   string            `json:"team_id"`
	TeamName string            `json:"team_name"`
	Enabled  bool              `json:"enabled"`
	Projects []teamReadProject `json:"projects"`
}

// readTeams is GET /v1/identity/peers/{service_id}/team-reads and
// .../team-reads/{team_id}: the calling peer's own teams only. A disabled
// profile is reported with enabled false and no grants; a grant whose
// project is gone is left out.
func (s *Server) readTeams(w http.ResponseWriter, r *http.Request) {
	peer, ok := s.teamPeer(w, r)
	if !ok {
		return
	}
	db, err := s.openAccess(false)
	if err != nil {
		s.identityRefuse(w, "identity_unavailable")
		return
	}
	actor, teamID := "peer:"+peer.ServiceID, r.PathValue("team_id")
	profiles, err := db.ListTeamProfiles(peer.ServiceID)
	if err != nil {
		s.teamReadFailed(w, err)
		return
	}
	if teamID != "" {
		i := slices.IndexFunc(profiles, func(p access.TeamProfile) bool { return p.TeamID == teamID })
		if i < 0 {
			db.RecordTeamRequest(actor, "team.read.refused.not_found", "service="+peer.ServiceID+" team="+teamID)
			s.identityRefuse(w, "not_found")
			return
		}
		profiles = profiles[i : i+1]
	}
	names, err := s.projectNamesByInstance()
	if err != nil {
		s.teamReadFailed(w, err)
		return
	}
	teams := []teamReadView{}
	for _, p := range profiles {
		v := teamReadView{TeamID: p.TeamID, TeamName: p.TeamName, Enabled: !p.Disabled, Projects: []teamReadProject{}}
		if !p.Disabled {
			instances, err := db.TeamGrantInstances(p.ID)
			if err != nil {
				s.teamReadFailed(w, err)
				return
			}
			for _, inst := range instances {
				project := names[inst]
				if project == "" {
					continue
				}
				tp, err := s.teamReadProject(project)
				if err != nil {
					s.teamReadFailed(w, err)
					return
				}
				v.Projects = append(v.Projects, tp)
			}
		}
		teams = append(teams, v)
	}
	subject := "service=" + peer.ServiceID
	if teamID != "" {
		subject += " team=" + teamID
	}
	if err := db.RecordTeamRequest(actor, "team.read", subject); err != nil {
		s.teamReadFailed(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if teamID != "" {
		s.ok(w, teams[0])
		return
	}
	s.ok(w, map[string]any{"teams": teams})
}

// teamReadProject reads one granted project's repository and process pin.
func (s *Server) teamReadProject(project string) (teamReadProject, error) {
	tp := teamReadProject{Project: project}
	db, err := s.reg.OpenExisting(project)
	if err != nil {
		return tp, err
	}
	v, err := db.GetMeta(repositoryMetaKey)
	if err != nil {
		return tp, err
	}
	repo, err := decodeRepository(v)
	if err != nil {
		return tp, err
	}
	tp.Repository = viewRepository(repo)
	tp.Process, err = currentProcessRef(db)
	return tp, err
}

// teamReadFailed answers a store that could not give a reliable answer; an
// incomplete list of grants is never answered.
func (s *Server) teamReadFailed(w http.ResponseWriter, err error) {
	s.log.Error("team read", "err", err)
	s.identityRefuse(w, identityStoreError(err))
}
