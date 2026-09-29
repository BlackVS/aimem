package server

// Actionable team-context refusals (task 19f6, decisions R1 to R3). A refusal
// may name the operation it denied and the caller's own verified role, and
// its one next action may depend on the code, the role and the operation. It
// never names another actor or a secret, and a team-mode next action never
// advises another credential, a personal-mode retry or a bypass.

import (
	"net/http"
	"strings"
)

// refusalContext is what a refusal may say about the caller's own request.
type refusalContext struct {
	Action string // the denied operation
	Role   string // the caller's verified team role; "" outside team mode
}

type nextActionKey struct{ code, role, action string }

// roleNextActions override a code's next action for one role and operation.
// Every other case keeps the code's own next action.
var roleNextActions = map[nextActionKey]string{
	{"role_forbidden", "worker", "reservation.claim"}:       "Check your aicrew inbox and accept the offer; aimem claims for team work come only through aicrew.",
	{"role_forbidden", "independent", "reservation.claim"}:  "Begin the independent claim through aicrew; it supplies the proof aimem requires.",
	{"role_forbidden", "coordinator", "reservation.update"}: "Work updates belong to the holder; review or reassign the work through aicrew.",
}

// teamNextActions replace a code's next action in team mode, where the
// general wording would point at another credential.
var teamNextActions = map[string]string{
	"credential_scope_forbidden": "Start the conversation through aicrew, which binds it to this installation's individual aimem credential.",
}

// nextActionFor is the one next action for a refusal: the role- and
// operation-specific one when the table has it, otherwise the code's own.
func nextActionFor(code string, rc refusalContext, fallback string) string {
	if next, ok := roleNextActions[nextActionKey{code, rc.Role, rc.Action}]; ok {
		return next
	}
	return fallback
}

// reservationAction names a member reservation request's operation:
// reservation.<op>, reservation.status or reservation.receipt.
func reservationAction(r *http.Request) string {
	path := r.URL.Path
	i := strings.LastIndex(path, "/reservation")
	if i < 0 {
		return ""
	}
	rest := strings.TrimPrefix(path[i+len("/reservation"):], "/")
	switch {
	case rest == "":
		return "reservation.status"
	case strings.HasPrefix(rest, "receipts/"):
		return "reservation.receipt"
	case !strings.Contains(rest, "/"):
		return "reservation." + rest
	}
	return ""
}

// routeAction names any other request by the route pattern it matches, such
// as "POST /v1/projects/{p}/tasks", which carries no ID; "" when no route
// matches.
func (s *Server) routeAction(r *http.Request) string {
	s.actionOnce.Do(func() {
		m := http.NewServeMux()
		for _, rt := range s.Routes() {
			m.HandleFunc(rt.Method+" "+rt.Pattern, func(http.ResponseWriter, *http.Request) {})
		}
		m.HandleFunc("/mcp", func(http.ResponseWriter, *http.Request) {})
		s.actionMux = m
	})
	_, pattern := s.actionMux.Handler(r)
	if pattern == "/mcp" {
		return r.Method + " /mcp"
	}
	return pattern
}

// deniedContext is the refusal context of a request: its operation and, in a
// verified team context, the caller's role.
func (s *Server) deniedContext(r *http.Request, role string) refusalContext {
	action := ""
	if reservationRoute(r) {
		action = reservationAction(r)
	}
	if action == "" {
		action = s.routeAction(r)
	}
	if tc, ok := teamContextFrom(r.Context()); ok && role == "" {
		role = tc.Role
	}
	return refusalContext{Action: action, Role: role}
}
