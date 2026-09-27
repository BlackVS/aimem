package server

// Team mode and the aicrew introspection it depends on (identity.v1 §3 and
// §4, docs/DESIGN-AIFORGE-IDENTITY-WIRE.md).
//
// A request carrying X-Aimem-Team-Context is in team mode. It is served only
// on the routes in teamRoutes, and only after the context is verified online:
// one introspection of the handle, an exact match with the authenticated
// individual credential, and a linked, enabled team access profile. The
// resource check then evaluates that profile's live grant alone, never the
// caller's personal grants. Any other route is refused before aicrew is
// contacted, and no team-mode request is ever served as a personal one.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"aimem/internal/access"
	"aimem/internal/introspect"
	"aimem/internal/store"
	"aimem/internal/uuidv7"
)

const teamContextHeader = "X-Aimem-Team-Context"

// introspectionTokenEnv names the private file holding the bearer aicrew
// issued to this hub for introspection.
const introspectionTokenEnv = "AIMEM_INTROSPECTION_TOKEN_FILE"

func newIntrospectionClient() *introspect.Client {
	return &introspect.Client{TokenFile: os.Getenv(introspectionTokenEnv)}
}

// SetIntrospectionClient replaces the hub's introspection client; tests use
// it to trust a fake aicrew's CA.
func (s *Server) SetIntrospectionClient(c *introspect.Client) { s.introspect = c }

// teamMode reports whether the request asks for team mode. The header's
// presence decides, whatever its value, so an empty or garbled header is
// refused rather than served in personal mode.
func teamMode(r *http.Request) bool {
	_, ok := r.Header[http.CanonicalHeaderKey(teamContextHeader)]
	return ok
}

// teamRoutes is the complete team-mode surface (E4 seq186, D2): the context
// report and the task and epic reads. Every other route, including every
// write, /mcp and the knowledge routes, refuses a team-mode request with
// team_operation_unsupported before aicrew is contacted.
var teamRoutes = []string{
	"GET /v1/access/identity",
	"GET /v1/projects/{p}/tasks",
	"GET /v1/tasks/{id}",
	"GET /v1/tasks/{id}/history",
	"GET /v1/tasks/{id}/comments",
	"GET /v1/tasks/{id}/comments/{c}",
	"GET /v1/projects/{p}/epics",
	"GET /v1/projects/{p}/epics/{e}",
}

// teamRouteMux matches teamRoutes by the route mux's own rules.
var teamRouteMux = func() *http.ServeMux {
	m := http.NewServeMux()
	for _, p := range teamRoutes {
		m.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	return m
}()

func teamRouteServed(r *http.Request) bool {
	if !canonicalPath(r) {
		return false
	}
	_, pattern := teamRouteMux.Handler(r)
	return pattern != ""
}

// teamReadRoles are aicrew's member roles; each may use every team route.
var teamReadRoles = map[string]bool{"coordinator": true, "worker": true, "independent": true}

// teamContext is a team context verified for one request.
type teamContext struct {
	introspect.Context
	ProfileID     string
	CorrelationID string
}

type teamContextKey struct{}

func teamContextFrom(ctx context.Context) (teamContext, bool) {
	tc, ok := ctx.Value(teamContextKey{}).(teamContext)
	return tc, ok
}

// teamRefuse answers a team-mode refusal once the header and the credential
// are valid: the envelope names the team mode and the correlation ID that
// the audit record carries.
func (s *Server) teamRefuse(w http.ResponseWriter, code, correlationID string) {
	s.identityRefuseWith(w, code, "team", correlationID)
}

// teamAuditActor names the authenticated caller in the audit.
func teamAuditActor(id Identity) string {
	if id.Role == "user" {
		return "user:" + id.UserID
	}
	return "credential:" + id.Name
}

// teamRequestRoute is the request line as the audit records it for a refusal
// that happens before any route or session is known.
func teamRequestRoute(r *http.Request) string {
	line := r.Method + " " + r.URL.EscapedPath()
	if len(line) > 200 {
		line = line[:200]
	}
	return fmt.Sprintf("request=%q", line)
}

// teamDeny answers every team-mode refusal after authentication. It audits
// team.refused.<code> under the caller, with detail and the correlation ID,
// then answers in team mode under that same ID. An audit failure is logged;
// the request is refused either way.
func (s *Server) teamDeny(w http.ResponseWriter, id Identity, code, detail, cid string) {
	subject := strings.TrimSpace(detail + " correlation=" + cid)
	if db, err := s.openAccess(false); err != nil {
		s.log.Error("team audit", "err", err)
	} else if err := db.RecordTeamRequest(teamAuditActor(id), "team.refused."+code, subject); err != nil {
		s.log.Error("team audit", "err", err)
	}
	s.log.Warn("team request refused", "code", code, "correlation_id", cid)
	s.teamRefuse(w, code, cid)
}

// teamGate decides a team-mode request. id is the authenticated caller, or nil
// on the local socket. It returns the verified context, or false once it has
// answered the refusal. The handle is never echoed, logged or audited.
func (s *Server) teamGate(w http.ResponseWriter, r *http.Request, id *Identity) (teamContext, bool) {
	values := r.Header.Values(teamContextHeader)
	shapeOK := len(values) == 1 && introspect.ValidHandle(values[0])
	if id == nil {
		// The local socket's caller is the operator, not an authenticated
		// credential; there is no one to attribute an audit record to.
		if !shapeOK {
			s.identityRefuse(w, "invalid_request")
		} else {
			s.identityRefuse(w, "credential_scope_forbidden")
		}
		return teamContext{}, false
	}
	cid := uuidv7.New()
	switch {
	case !shapeOK:
		s.teamDeny(w, *id, "invalid_request", teamRequestRoute(r), cid)
		return teamContext{}, false
	case id.Role != "user" || id.Scope != access.ScopeUser || id.Project != "":
		s.teamDeny(w, *id, "credential_scope_forbidden", teamRequestRoute(r), cid)
		return teamContext{}, false
	case !teamRouteServed(r):
		s.teamDeny(w, *id, "team_operation_unsupported", teamRequestRoute(r), cid)
		return teamContext{}, false
	}
	return s.verifyTeamContext(w, r, *id, values[0], cid)
}

// verifyTeamContext runs the context contract's verifier after
// authentication: the single operational peer, one introspection, the exact
// binding to the authenticated credential, the linked enabled profile and the
// role policy. Every outcome is audited under the request's correlation ID.
func (s *Server) verifyTeamContext(w http.ResponseWriter, r *http.Request, id Identity, handle, cid string) (teamContext, bool) {
	refuse := func(code, reason, detail string) (teamContext, bool) {
		s.teamDeny(w, id, code, strings.TrimSpace(detail+" "+teamRequestRoute(r)+" reason="+reason), cid)
		return teamContext{}, false
	}
	db, err := s.openAccess(false)
	if err != nil {
		return refuse("context_unavailable", "access_store", "")
	}
	peers, err := s.introspectionPeers(db)
	if err != nil {
		return refuse("context_unavailable", "peer_store", "")
	}
	var peer *peerState
	for i := range peers {
		if peers[i].NotOperational == "" {
			if peer != nil {
				return refuse("context_unavailable", "peer_ambiguous", "")
			}
			peer = &peers[i]
		}
	}
	if peer == nil {
		return refuse("context_unavailable", "not_operational", "")
	}
	got, err := s.introspect.Introspect(r.Context(), toIntrospectPeer(peer.IdentityPeer), handle)
	if err != nil {
		var f *introspect.Failure
		code, reason := introspect.CodeUnavailable, "transport"
		if errors.As(err, &f) {
			code, reason = f.Code, f.Reason
		}
		// A reply naming another hub is about whose session this is, not
		// about the peer: identity.v1 §4 calls it an identity mismatch.
		if reason == "wrong_hub" {
			code = "identity_mismatch"
		}
		return refuse(code, reason, "service="+peer.ServiceID)
	}
	detail := teamAuditDetail(got, r)
	switch {
	case got.UserID != id.UserID:
		return refuse("identity_mismatch", "other_user", detail)
	case got.TokenID != id.TokenID:
		// The same person under a rotated credential: aicrew must re-prove
		// the session before it binds to the new token.
		return refuse("context_stale", "other_token", detail)
	}
	profile, err := db.TeamProfileByKey(got.ServiceID, got.TeamID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return refuse("context_stale", "no_profile", detail)
	case err != nil:
		return refuse("context_unavailable", "profile_store", detail)
	case profile.Disabled:
		return refuse("context_stale", "profile_disabled", detail)
	case !teamReadRoles[got.Role]:
		return refuse("role_forbidden", "role", detail)
	}
	if err := db.RecordTeamRequest(teamAuditActor(id), "team.verified", detail+" correlation="+cid); err != nil {
		s.log.Error("team audit", "err", err)
		return refuse("identity_unavailable", "audit", detail)
	}
	return teamContext{Context: got, ProfileID: profile.ID, CorrelationID: cid}, true
}

// teamAuditDetail names what the audit records about a verified session: the
// service, team, session, generation, role and route. Never the handle.
func teamAuditDetail(c introspect.Context, r *http.Request) string {
	_, pattern := teamRouteMux.Handler(r)
	return fmt.Sprintf("service=%s team=%s session=%s generation=%s role=%s route=%q",
		c.ServiceID, c.TeamID, c.SessionID, c.Generation, c.Role, pattern)
}

// teamGrantDenied applies a verified team context's resource grant to
// project, and answers the refusal when the linked profile has no live grant.
// A personal request passes through untouched. It is called wherever a task
// or epic read resolves its project (taskProject, locateTask).
func (s *Server) teamGrantDenied(w http.ResponseWriter, r *http.Request, project string) bool {
	tc, ok := teamContextFrom(r.Context())
	if !ok {
		return false
	}
	id, _ := IdentityFrom(r.Context())
	detail := teamAuditDetail(tc.Context, r) + fmt.Sprintf(" project=%q", project)
	allowed, err := s.teamGrantAllows(id, tc, project)
	if err != nil {
		s.log.Error("team grant check", "project", project, "err", err)
		s.teamDeny(w, id, "identity_unavailable", detail+" reason=grant_store", tc.CorrelationID)
		return true
	}
	if allowed {
		return false
	}
	s.teamDeny(w, id, "grant_denied", detail, tc.CorrelationID)
	return true
}

// teamGrantAllows evaluates only the linked profile's live grant on the
// project's access instance, together with the caller's live individual
// token. Personal and group grants never count.
func (s *Server) teamGrantAllows(id Identity, tc teamContext, project string) (bool, error) {
	instance, err := s.reg.ExistingProjectAccessID(project)
	if err != nil || instance == "" {
		return false, err
	}
	db, err := s.openAccess(false)
	if err != nil {
		return false, err
	}
	return db.TeamGrantAllows(id.UserID, id.TokenID, tc.ProfileID, instance)
}

// teamContextReport is GET /v1/access/identity in team mode: the verified
// session and the projects its profile currently grants. Knowledge access is
// reported unavailable until the knowledge matrix exists.
func (s *Server) teamContextReport(w http.ResponseWriter, r *http.Request, id Identity, tc teamContext) {
	unavailable := func(reason string) {
		s.teamDeny(w, id, "identity_unavailable", teamAuditDetail(tc.Context, r)+" reason="+reason, tc.CorrelationID)
	}
	db, err := s.openAccess(false)
	if err != nil {
		unavailable("access_store")
		return
	}
	instances, err := db.TeamGrantProjects(tc.ProfileID)
	if err != nil {
		unavailable("grant_store")
		return
	}
	granted := map[string]bool{}
	for _, i := range instances {
		granted[i] = true
	}
	names, err := s.reg.Projects()
	if err != nil {
		unavailable("registry")
		return
	}
	projects := []string{}
	for _, p := range names {
		if store.IsReservedProject(p) {
			continue
		}
		if instance, err := s.reg.ExistingProjectAccessID(p); err == nil && instance != "" && granted[instance] {
			projects = append(projects, p)
		}
	}
	sort.Strings(projects)
	out := map[string]any{
		"mode": "team", "name": id.Name, "user_id": id.UserID, "token_id": id.TokenID, "role": "user", "scope": id.Scope,
		"team": map[string]string{
			"service_id": tc.ServiceID, "team_id": tc.TeamID, "agent_id": tc.AgentID, "role": tc.Role,
			"session_id": tc.SessionID, "generation": tc.Generation, "handle_expires_at": tc.HandleExpiresAt.Format(time.RFC3339),
		},
		"task_read": "granted-projects", "task_write": false, "projects": projects,
		"knowledge": "unavailable", "correlation_id": tc.CorrelationID,
	}
	if project := r.URL.Query().Get("project"); project != "" {
		pdb, err := s.reg.OpenExisting(project)
		if errors.Is(err, store.ErrNoSuchProject) {
			s.fail(w, http.StatusNotFound, fmt.Errorf("unknown project"))
			return
		}
		if err != nil {
			unavailable("project_store")
			return
		}
		tasksOn, err := pdb.TasksEnabled()
		if err != nil {
			unavailable("project_store")
			return
		}
		allowed, err := s.teamGrantAllows(id, tc, project)
		if err != nil {
			unavailable("grant_store")
			return
		}
		out["project"], out["tasks_enabled"], out["project_read"] = project, tasksOn, allowed
	}
	w.Header().Set("Cache-Control", "no-store")
	s.ok(w, out)
}

// localTeamGate guards the local socket, whose caller is the operator rather
// than an individual credential: a team-mode request is refused there.
func (s *Server) localTeamGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if teamMode(r) {
			s.teamGate(w, r, nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// peerState is a registered peer as the introspection client sees it.
// NotOperational is the fixed reason it cannot be introspected, or "".
type peerState struct {
	access.IdentityPeer
	NotOperational string
}

func (p peerState) view() peerView {
	v := toPeerView(p.IdentityPeer)
	v.Introspection = p.NotOperational == ""
	return v
}

// introspectionPeers returns every registered peer, in listing order, with
// its introspection state. Nothing here touches the network.
func (s *Server) introspectionPeers(db *access.Store) ([]peerState, error) {
	peers, err := db.ListIdentityPeers()
	if err != nil {
		return nil, err
	}
	hub, err := db.HubID()
	if err != nil {
		return nil, err
	}
	enabled := 0
	for _, p := range peers {
		if !p.Disabled {
			enabled++
		}
	}
	out := make([]peerState, 0, len(peers))
	for _, p := range peers {
		st := peerState{IdentityPeer: p}
		switch {
		case p.Disabled:
			st.NotOperational = "peer_disabled"
		case enabled != 1:
			st.NotOperational = "peer_ambiguous"
		case p.HubID != hub:
			st.NotOperational = "wrong_hub"
		default:
			_, st.NotOperational = s.introspect.Operational(toIntrospectPeer(p))
		}
		out = append(out, st)
	}
	return out, nil
}

func findPeer(peers []peerState, service string) (peerState, bool) {
	for _, p := range peers {
		if p.ServiceID == service {
			return p, true
		}
	}
	return peerState{}, false
}

func toIntrospectPeer(p access.IdentityPeer) introspect.Peer {
	return introspect.Peer{ServiceID: p.ServiceID, HubID: p.HubID, Endpoint: p.Endpoint, TLSMode: p.TLSMode, TLSValue: p.TLSValue}
}

// checkIdentityPeer is the operator's end-to-end check of one peer's
// introspection: one call with a random handle that no session holds, which
// a working peer answers as inactive. The answer names a fixed outcome only;
// the handle and the credential never leave the hub's memory.
func (s *Server) checkIdentityPeer(w http.ResponseWriter, r *http.Request) {
	db, ok := s.peerAdminStore(w, r)
	if !ok {
		return
	}
	service := r.PathValue("service_id")
	peers, err := s.introspectionPeers(db)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot read identity peers"))
		return
	}
	p, found := findPeer(peers, service)
	if !found {
		s.fail(w, http.StatusNotFound, fmt.Errorf("unknown identity peer"))
		return
	}
	outcome := p.NotOperational
	if outcome == "" {
		outcome = s.probePeer(r.Context(), toIntrospectPeer(p.IdentityPeer))
	}
	if err := db.RecordIdentityPeerCheck(accessActor(r), service, outcome); err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("cannot record the peer check"))
		return
	}
	s.ok(w, map[string]any{"service_id": service, "ok": outcome == "inactive", "outcome": outcome})
}

// probePeer runs one introspection with a fresh random handle. Only a
// verified inactive answer is healthy; an active one for a handle nobody
// holds means the peer does not answer from its own session state.
func (s *Server) probePeer(ctx context.Context, p introspect.Peer) string {
	h, err := introspect.NewHandle()
	if err != nil {
		return "handle"
	}
	_, err = s.introspect.Introspect(ctx, p, h)
	var f *introspect.Failure
	switch {
	case err == nil:
		return "unexpected_active"
	case errors.As(err, &f):
		return f.Reason
	}
	return "transport"
}
