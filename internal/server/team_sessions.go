package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"aimem/internal/store"
)

var teamSessionOperations = []string{"join", "members", "profile", "heartbeat", "resume", "leave", "send", "messages", "inbox", "ack"}

const teamWaitingInstructions = "Team workers wait for an explicit coordinator assignment; do not pick backlog work independently. Use inbox reads or bounded waits for messages; all messages are team-visible and a message never assigns work. Explicit ack records receipt, not task completion. Task assignment is not implemented on this hub yet. Do not execute work on the basis of join or a role. Resume requires reconciliation of any old local execution; a changed generation cannot stop local processes. Explicit leave returns to standalone mode."

// Explicit projection: session handles and declared profiles are public to
// members; internal access user/token bindings never leave the store here.
type teamMemberView struct {
	ID                    string `json:"id"`
	TeamID                string `json:"team_id"`
	Generation            int64  `json:"generation"`
	CoordinatorGeneration int64  `json:"coordinator_generation,omitempty"`
	Role                  string `json:"role"`
	State                 string `json:"state"`
	Availability          string `json:"availability"`
	ProfileRevision       int64  `json:"profile_revision"`
	store.TeamProfile
	LastSeenAt string `json:"last_seen_at"`
	Suspect    bool   `json:"suspect"`
}

func memberView(s store.TeamSession, now time.Time, timeout time.Duration) teamMemberView {
	return teamMemberView{s.ID, s.TeamID, s.Generation, s.CoordinatorGeneration, s.Role, s.State, s.Availability, s.ProfileRevision, s.TeamProfile, s.LastSeenAt, s.Suspect(now, timeout)}
}

func sessionPolicy() (limit int, timeout time.Duration, err error) {
	limit = store.MaxTeamSessions
	seconds := 120
	for _, v := range []struct {
		name     string
		dst      *int
		min, max int
	}{{"AIMEM_TEAM_MAX_SESSIONS", &limit, 1, 10000}, {"AIMEM_TEAM_SUSPECT_SECONDS", &seconds, 30, 86400}} {
		if raw := os.Getenv(v.name); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil || n < v.min || n > v.max {
				return 0, 0, errors.New("invalid operator team policy")
			}
			*v.dst = n
		}
	}
	return limit, time.Duration(seconds) * time.Second, nil
}

func (s *Server) sessionProject(w http.ResponseWriter, r *http.Request) *store.DB {
	id, ok := IdentityFrom(r.Context())
	if !ok || id.Role != "user" {
		s.fail(w, 403, errors.New("team sessions require an ordinary project write token"))
		return nil
	}
	p, db := s.taskProject(w, r)
	if db == nil {
		return nil
	}
	if !s.authorizeTaskWrite(w, r, p) {
		return nil
	}
	return db
}

func decodeSessionBody(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("invalid session JSON")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("expected one session JSON object")
	}
	return nil
}

func sessionAudit(w http.ResponseWriter, r *http.Request, db *store.DB) (store.TeamAuditContext, error) {
	a := store.TeamAuditContext{Actor: taskActor(r), RequestID: w.Header().Get("X-Request-ID"), ServerVersion: Version}
	ref, err := currentProcessRef(db)
	if ref != nil {
		a.ProcessCommit = ref.Commit
	}
	return a, err
}

func (s *Server) sessionResult(w http.ResponseWriter, r *http.Request, member store.TeamSession, timeout time.Duration, created bool) {
	now := time.Now().UTC()
	out := map[string]any{"protocol_version": 1, "operations": teamSessionOperations, "session": memberView(member, now, timeout), "server_time": now.Format(time.RFC3339Nano), "heartbeat_seconds": 30, "suspect_seconds": int(timeout.Seconds()), "instructions": teamWaitingInstructions}
	if created {
		s.created(w, out)
	} else {
		s.ok(w, out)
	}
}

func (s *Server) joinTeam(w http.ResponseWriter, r *http.Request) {
	db := s.sessionProject(w, r)
	if db == nil {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var req struct {
		Role    string            `json:"role"`
		Profile store.TeamProfile `json:"profile"`
	}
	if err := decodeSessionBody(w, r, &req); err != nil {
		s.fail(w, 400, err)
		return
	}
	limit, timeout, err := sessionPolicy()
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	team, err := db.ResolveTeam(r.PathValue("team"))
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	audit, err := sessionAudit(w, r, db)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	member, err := db.JoinTeamLimit(team.ID, req.Role, req.Profile, audit, key, limit)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	s.sessionResult(w, r, member, timeout, true)
}

func (s *Server) changeTeamSession(w http.ResponseWriter, r *http.Request) {
	db := s.sessionProject(w, r)
	if db == nil {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var req store.TeamSessionCommand
	if err := decodeSessionBody(w, r, &req); err != nil {
		s.fail(w, 400, err)
		return
	}
	_, timeout, err := sessionPolicy()
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	audit, err := sessionAudit(w, r, db)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	op := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	member, err := db.ChangeTeamSession(r.PathValue("team"), op, req, audit, key)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	s.sessionResult(w, r, member, timeout, false)
}

func (s *Server) teamMembers(w http.ResponseWriter, r *http.Request) {
	db := s.sessionProject(w, r)
	if db == nil {
		return
	}
	q := r.URL.Query()
	for k, v := range q {
		if (k != "session_id" && k != "generation" && k != "after" && k != "limit") || len(v) != 1 {
			s.fail(w, 400, errors.New("invalid roster query"))
			return
		}
	}
	generation, err := strconv.ParseInt(q.Get("generation"), 10, 64)
	if err != nil || generation < 1 {
		s.fail(w, 400, errors.New("session generation required"))
		return
	}
	limit, err := teamPageLimit(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	_, timeout, err := sessionPolicy()
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	members, err := db.TeamRoster(r.PathValue("team"), q.Get("session_id"), generation, taskActor(r), q.Get("after"), limit+1)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	next := ""
	if len(members) > limit {
		members = members[:limit]
		next = members[len(members)-1].ID
	}
	now := time.Now().UTC()
	views := []teamMemberView{}
	for _, member := range members {
		views = append(views, memberView(member, now, timeout))
	}
	s.ok(w, map[string]any{"protocol_version": 1, "members": views, "next_cursor": next, "server_time": now.Format(time.RFC3339Nano), "operations": teamSessionOperations, "instructions": teamWaitingInstructions})
}
