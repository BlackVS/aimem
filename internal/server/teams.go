package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"aimem/internal/store"
	"aimem/internal/uuidv7"
)

type teamStatusWriter struct {
	http.ResponseWriter
	status int
}

func teamView(t store.Team) any {
	return struct {
		ProtocolVersion int `json:"protocol_version"`
		store.Team
	}{1, t}
}

func (w *teamStatusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *teamStatusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *teamStatusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Observe only the team API, outside authentication, without logging supplied
// paths, credentials, retry keys or rejected bodies. Socket and TCP share this.
func (s *Server) observeTeamRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) < 4 || parts[0] != "v1" || parts[1] != "projects" || parts[3] != "teams" || w.Header().Get("X-Request-ID") != "" {
			next.ServeHTTP(w, r)
			return
		}
		id := uuidv7.New()
		w.Header().Set("X-Request-ID", id)
		out := &teamStatusWriter{ResponseWriter: w}
		start := time.Now()
		method := "other"
		switch r.Method {
		case http.MethodGet, http.MethodPost, http.MethodPut:
			method = r.Method
		}
		defer func() {
			status := out.status
			if status == 0 {
				status = 200
			}
			s.log.Info("team request", "request_id", id, "method", method, "status", status, "duration_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(out, r)
	})
}

func (s *Server) teamError(w http.ResponseWriter, r *http.Request, err error) {
	status, message := http.StatusInternalServerError, "team storage failure"
	var conflict *store.TeamConflict
	var taskConflict *store.TaskConflict
	switch {
	case errors.Is(err, store.ErrTeamSessionDenied):
		status, message = 403, "current team enrollment and bound ordinary write session required"
	case errors.Is(err, store.ErrTeamAssignmentConflict):
		status, message = 409, err.Error()
	case errors.Is(err, store.ErrTeamAssignmentNotFound):
		status, message = 404, "team assignment not found"
	case errors.As(err, &taskConflict):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": "task revision conflict", "current": taskView(r.PathValue("p"), taskConflict.Current)})
		return
	case errors.Is(err, store.ErrTeamSessionStale), errors.Is(err, store.ErrTeamCoordinatorOccupied), errors.Is(err, store.ErrTeamSessionQuota), errors.Is(err, store.ErrTeamMessageQuota), errors.Is(err, store.ErrTeamMessageUndelivered):
		status, message = 409, err.Error()
	case errors.As(err, &conflict):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": "team revision conflict", "current": teamView(conflict.Current)})
		return
	case errors.Is(err, store.ErrTeamNotFound), errors.Is(err, store.ErrNoSuchProject), errors.Is(err, store.ErrTeamMessageNotFound), errors.Is(err, store.ErrTaskNotFound):
		status, message = 404, "team or project not found"
	case errors.Is(err, store.ErrTeamNameTaken), errors.Is(err, store.ErrTaskRetryConflict):
		status, message = 409, err.Error()
	case errors.Is(err, store.ErrTaskInvalid), errors.Is(err, store.ErrTaskReservedScope):
		status, message = 400, err.Error()
	}
	s.log.Warn("team request refused", "request_id", w.Header().Get("X-Request-ID"), "status", status, "operation", r.Method)
	s.fail(w, status, errors.New(message))
}

func (s *Server) adminTeamProject(w http.ResponseWriter, r *http.Request) (string, *store.DB) {
	if w.Header().Get("X-Request-ID") == "" {
		w.Header().Set("X-Request-ID", uuidv7.New())
	}
	p, db := s.taskProject(w, r)
	if db == nil {
		return "", nil
	}
	if !s.tasksEnabledFor(w, p) {
		return "", nil
	}
	return p, db
}

func teamPageLimit(r *http.Request) (int, error) {
	if !r.URL.Query().Has("limit") {
		return 50, nil
	}
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n < 1 || n > 100 {
		return 0, errors.New("limit must be 1-100")
	}
	return n, nil
}

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) {
	_, db := s.adminTeamProject(w, r)
	if db == nil {
		return
	}
	limit, err := teamPageLimit(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	ts, err := db.ListTeams(r.URL.Query().Get("after"), limit+1)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	next := ""
	if len(ts) > limit {
		ts = ts[:limit]
		next = ts[len(ts)-1].ID
	}
	s.ok(w, map[string]any{"protocol_version": 1, "teams": ts, "next_cursor": next, "operations": []string{"admin_configure", "admin_read"}})
}

func (s *Server) getTeam(w http.ResponseWriter, r *http.Request) {
	_, db := s.adminTeamProject(w, r)
	if db == nil {
		return
	}
	t, err := db.GetTeam(r.PathValue("team"))
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	s.ok(w, teamView(t))
}

func (s *Server) teamEvents(w http.ResponseWriter, r *http.Request) {
	_, db := s.adminTeamProject(w, r)
	if db == nil {
		return
	}
	limit, err := teamPageLimit(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	var after int64
	if r.URL.Query().Has("after") {
		after, err = strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
		if err != nil || after < 0 {
			s.fail(w, 400, errors.New("after must be a nonnegative sequence"))
			return
		}
	}
	es, err := db.TeamEvents(r.PathValue("team"), after, limit+1)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	var next int64
	if len(es) > limit {
		es = es[:limit]
		next = es[len(es)-1].Sequence
	}
	s.ok(w, map[string]any{"protocol_version": 1, "events": es, "next_cursor": next})
}

func (s *Server) configureTeam(w http.ResponseWriter, r *http.Request) {
	p, db := s.adminTeamProject(w, r)
	if db == nil {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var req struct {
		store.TeamContent
		ExpectedRevision int64 `json:"expected_revision"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.fail(w, 400, errors.New("invalid team configuration JSON"))
		return
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		s.fail(w, 400, errors.New("expected one JSON object"))
		return
	}
	ad, ok := s.accessStore(w)
	if !ok {
		return
	}
	snap, err := ad.Snapshot()
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	users := map[string]bool{}
	for _, u := range snap.Users {
		users[u.ID] = true
	}
	for _, e := range req.Enrollment {
		if !users[e.UserID] {
			s.fail(w, 400, errors.New("enrollment references an unknown user"))
			return
		}
	}
	ref, err := currentProcessRef(db)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	audit := store.TeamAuditContext{Actor: taskActor(r), RequestID: w.Header().Get("X-Request-ID"), ServerVersion: Version}
	if ref != nil {
		audit.ProcessCommit = ref.Commit
	}
	t, err := s.reg.ConfigureTeam(p, r.PathValue("team"), req.ExpectedRevision, req.TeamContent, audit, key)
	if err != nil {
		s.teamError(w, r, err)
		return
	}
	s.log.Info("team configured", "request_id", audit.RequestID, "team_id", t.ID, "revision", t.Revision)
	if r.Method == http.MethodPost {
		s.created(w, teamView(t))
	} else {
		s.ok(w, teamView(t))
	}
}
