package server

// Team mode and the aicrew introspection it depends on (identity.v1 §3 and
// §4, docs/DESIGN-AIFORGE-IDENTITY-WIRE.md).
//
// A request carrying X-Aimem-Team-Context is in team mode. No operation is
// served in team mode yet, so the gate refuses every such request before
// any handler runs and before aicrew is contacted; it never serves one as a
// personal request. The introspection client is reached only by the
// operator's peer check.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"aimem/internal/access"
	"aimem/internal/introspect"
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

// refuseTeamMode answers a team-mode request. id is the authenticated caller,
// or nil on the local socket. The handle is never echoed.
func (s *Server) refuseTeamMode(w http.ResponseWriter, r *http.Request, id *Identity) {
	values := r.Header.Values(teamContextHeader)
	switch {
	case len(values) != 1 || !introspect.ValidHandle(values[0]):
		s.identityRefuse(w, "invalid_request")
	case id == nil || id.Role != "user" || id.Scope != access.ScopeUser || id.Project != "":
		s.identityRefuse(w, "credential_scope_forbidden")
	default:
		s.identityRefuse(w, "team_operation_unsupported")
	}
}

// localTeamGate guards the local socket, whose caller is the operator rather
// than an individual credential: a team-mode request is refused there too.
func (s *Server) localTeamGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if teamMode(r) {
			s.refuseTeamMode(w, r, nil)
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
