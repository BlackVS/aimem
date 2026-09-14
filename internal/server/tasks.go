package server

// Task service (docs/DESIGN-task-backend-implementation.md, stage 2): the
// one boundary where a request's credential becomes a trusted storage
// actor. Every handler re-derives project and permission from the
// request; nothing here trusts a body field for identity, time or
// revision, and a retry receipt never grants what the caller lost.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"aimem/internal/store"
)

// maxTaskRequestBytes bounds a raw task body: fields decode to at most
// 32 KiB, escaping and whitespace need headroom, nothing needs more.
const maxTaskRequestBytes = 256 << 10

// ordinaryRoutes is the exact surface an ordinary (scoped user) token may
// reach besides its identity check: the task routes, whose handlers
// authorize every write themselves, and POST /mcp, whose dispatcher hides
// every legacy tool from such a caller. Matched by the mux's own rules
// against these exact patterns — never by prefix — so a future route is
// refused until it is listed here (and Route.Ordinary shows it).
var ordinaryRoutes = map[string]bool{
	"GET /v1/projects/{p}/tasks":      true,
	"POST /v1/projects/{p}/tasks":     true,
	"GET /v1/tasks/{id}":              true,
	"PUT /v1/tasks/{id}":              true,
	"GET /v1/tasks/{id}/history":      true,
	"GET /v1/tasks/{id}/comments":     true,
	"POST /v1/tasks/{id}/comments":    true,
	"GET /v1/tasks/{id}/comments/{c}": true,
	"POST /mcp":                       true,
}

func (s *Server) ordinaryMux() *http.ServeMux {
	s.ordOnce.Do(func() {
		m := http.NewServeMux()
		for pattern := range ordinaryRoutes {
			m.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
		}
		s.ord = m
	})
	return s.ord
}

// ordinaryAllowed reports whether r matches one of ordinaryRoutes exactly.
// A non-canonical path (dot segments) makes the mux answer with a redirect
// target instead of a registered pattern, so it is refused here.
func (s *Server) ordinaryAllowed(r *http.Request) bool {
	_, pattern := s.ordinaryMux().Handler(r)
	return ordinaryRoutes[pattern]
}

// taskActor is the trusted actor a mutation is stamped with. The unix
// socket carries no identity and is the local operator; admin credentials
// keep their name; ordinary tokens carry stable user and token IDs. A
// legacy writer never reaches a mutation (authorizeTaskWrite refuses it).
func taskActor(r *http.Request) store.TaskActor {
	id, ok := IdentityFrom(r.Context())
	if !ok {
		return store.TaskActor{Kind: "admin", Name: "local"}
	}
	if id.Role == "user" {
		return store.TaskActor{Kind: "user", UserID: id.UserID, TokenID: id.TokenID, Name: id.Name}
	}
	return store.TaskActor{Kind: "admin", Name: id.Name}
}

// authorizeTaskWrite decides, for this attempt, whether the caller may
// mutate tasks in project. Admin credentials and the local operator may;
// legacy writer tokens never do; an ordinary token only when it was issued
// for this project's instance and its user holds a current grant. Both
// are re-read every time — a grant removed a second ago is gone now.
func (s *Server) authorizeTaskWrite(w http.ResponseWriter, r *http.Request, project string) bool {
	id, ok := IdentityFrom(r.Context())
	if !ok || id.Role == "admin" {
		return true
	}
	if id.Role != "user" {
		s.fail(w, http.StatusForbidden, fmt.Errorf("token %q has role %q; task writes need an ordinary project token or an admin token", id.Name, id.Role))
		return false
	}
	instance, err := s.reg.ExistingProjectAccessID(project)
	if err != nil {
		s.log.Error("task write authorization", "project", project, "err", err)
		s.fail(w, http.StatusInternalServerError, errors.New("project access identity unavailable"))
		return false
	}
	if instance == "" || id.Project != instance {
		s.fail(w, http.StatusForbidden, errors.New("token is not issued for this project"))
		return false
	}
	db, ok := s.accessStore(w)
	if !ok {
		return false
	}
	allowed, err := db.CanWrite(id.UserID, instance)
	if err != nil {
		s.log.Error("task write authorization", "project", project, "err", err)
		s.fail(w, http.StatusInternalServerError, errors.New("cannot check project access"))
		return false
	}
	if !allowed {
		s.fail(w, http.StatusForbidden, errors.New("no current write grant on this project"))
		return false
	}
	return true
}

// taskProject opens the ordinary project named in the path, never
// creating one: a task write into a mistyped name must not resurrect a
// dropped project as an empty husk.
func (s *Server) taskProject(w http.ResponseWriter, r *http.Request) (string, *store.DB) {
	p := r.PathValue("p")
	if store.IsReservedProject(p) {
		s.fail(w, http.StatusBadRequest, store.ErrTaskReservedScope)
		return "", nil
	}
	db, err := s.reg.OpenExisting(p)
	if err != nil {
		s.log.Warn("task project open", "project", p, "err", err)
		s.fail(w, http.StatusNotFound, errors.New("unknown project"))
		return "", nil
	}
	return p, db
}

// locateTask resolves a task ID to its owning project across every
// existing ordinary project: the partition is the authority, so a rename
// is reflected on the next read.
func (s *Server) locateTask(w http.ResponseWriter, r *http.Request) (string, *store.DB, bool) {
	project, db, err := s.reg.LocateTask(r.PathValue("id"))
	if err != nil {
		s.taskError(w, err)
		return "", nil, false
	}
	return project, db, true
}

type taskLinks struct {
	Self     string `json:"self"`
	Project  string `json:"project"`
	History  string `json:"history"`
	Comments string `json:"comments"`
}

// taskResponse is the task as served: the storage snapshot, its owning
// project (resolved now, never stored), and origin-relative links a
// configured client resolves. Links carry no credential.
type taskResponse struct {
	store.Task
	Project string    `json:"project"`
	Links   taskLinks `json:"links"`
}

func taskView(project string, t store.Task) taskResponse {
	self := "/v1/tasks/" + t.ID
	return taskResponse{Task: t, Project: project, Links: taskLinks{
		Self: self, Project: "/v1/projects/" + project, History: self + "/history", Comments: self + "/comments",
	}}
}

type commentLinks struct {
	Self string `json:"self"`
	Task string `json:"task"`
}

type commentResponse struct {
	store.TaskComment
	Project string       `json:"project"`
	Links   commentLinks `json:"links"`
}

func commentView(project string, c store.TaskComment) commentResponse {
	return commentResponse{TaskComment: c, Project: project, Links: commentLinks{
		Self: "/v1/tasks/" + c.TaskID + "/comments/" + c.ID, Task: "/v1/tasks/" + c.TaskID,
	}}
}

// decodeTaskBody reads one bounded JSON object strictly: unknown fields,
// trailing values and oversized bodies are rejected before any field is
// looked at.
func (s *Server) decodeTaskBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body := http.MaxBytesReader(w, r.Body, maxTaskRequestBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.fail(w, http.StatusRequestEntityTooLarge, fmt.Errorf("request body exceeds %d bytes", maxTaskRequestBytes))
			return false
		}
		s.fail(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %v", err))
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.fail(w, http.StatusRequestEntityTooLarge, fmt.Errorf("request body exceeds %d bytes", maxTaskRequestBytes))
			return false
		}
		s.fail(w, http.StatusBadRequest, errors.New("request body must be a single JSON object"))
		return false
	}
	return true
}

// created answers 201 with a JSON body; the content type must be set
// before the status is written or net/http drops it.
func (s *Server) created(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(v)
}

// idempotencyKey is the one transport for retry keys over HTTP.
func (s *Server) idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		s.fail(w, http.StatusBadRequest, errors.New("Idempotency-Key header is required for task writes"))
		return "", false
	}
	return key, true
}

// taskError maps storage outcomes to the documented statuses. A revision
// conflict carries the current task — the caller has already passed the
// read authorization every task route requires. Storage faults never echo
// internal detail.
func (s *Server) taskError(w http.ResponseWriter, err error) {
	var conflict *store.TaskConflict
	switch {
	case errors.As(err, &conflict):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "current": conflict.Current})
	case errors.Is(err, store.ErrTaskNotFound):
		s.fail(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrTaskArchived), errors.Is(err, store.ErrTaskRetryConflict):
		s.fail(w, http.StatusConflict, err)
	case errors.Is(err, store.ErrTaskInvalid), errors.Is(err, store.ErrTaskReservedScope):
		s.fail(w, http.StatusBadRequest, err)
	default:
		s.log.Error("task storage", "err", err)
		s.fail(w, http.StatusInternalServerError, errors.New("task storage failure"))
	}
}

func queryInt(r *http.Request, name string) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return n, nil
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request) {
	project, db := s.taskProject(w, r)
	if db == nil {
		return
	}
	q := r.URL.Query()
	limit, err := queryInt(r, "limit")
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	f := store.TaskFilter{State: q.Get("state"), After: q.Get("after"), Limit: int(limit)}
	switch q.Get("include_archived") {
	case "", "false", "0":
	case "true", "1":
		f.IncludeArchived = true
	default:
		s.fail(w, http.StatusBadRequest, errors.New("include_archived must be true or false"))
		return
	}
	if a := q.Get("assignee"); a != "" {
		kind, id, ok := strings.Cut(a, "/")
		if !ok {
			s.fail(w, http.StatusBadRequest, errors.New("assignee filter must be kind/id"))
			return
		}
		f.Assignee = &store.TaskAssignee{Kind: kind, ID: id}
	}
	page, err := db.ListTasks(f)
	if err != nil {
		s.taskError(w, err)
		return
	}
	s.ok(w, map[string]any{"project": project, "tasks": page.Tasks, "next_cursor": page.Next})
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	project, db := s.taskProject(w, r)
	if db == nil {
		return
	}
	if !s.authorizeTaskWrite(w, r, project) {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var content store.TaskContent
	if !s.decodeTaskBody(w, r, &content) {
		return
	}
	task, err := db.CreateTask(content, taskActor(r), key)
	if err != nil {
		s.taskError(w, err)
		return
	}
	s.log.Info("task created", "project", project, "task", task.ID, "actor", taskActor(r).Name)
	s.created(w, taskView(project, task))
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	project, db, ok := s.locateTask(w, r)
	if !ok {
		return
	}
	task, err := db.GetTask(r.PathValue("id"))
	if err != nil {
		s.taskError(w, err)
		return
	}
	s.ok(w, taskView(project, task))
}

type taskUpdateBody struct {
	store.TaskContent
	ExpectedRevision int64 `json:"expected_revision"`
}

func (s *Server) updateTask(w http.ResponseWriter, r *http.Request) {
	project, db, ok := s.locateTask(w, r)
	if !ok {
		return
	}
	if !s.authorizeTaskWrite(w, r, project) {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var body taskUpdateBody
	if !s.decodeTaskBody(w, r, &body) {
		return
	}
	task, err := db.UpdateTask(r.PathValue("id"), body.TaskContent, body.ExpectedRevision, taskActor(r), key)
	if err != nil {
		s.taskError(w, err)
		return
	}
	s.log.Info("task updated", "project", project, "task", task.ID, "revision", task.Revision, "actor", taskActor(r).Name)
	s.ok(w, taskView(project, task))
}

func (s *Server) taskHistory(w http.ResponseWriter, r *http.Request) {
	project, db, ok := s.locateTask(w, r)
	if !ok {
		return
	}
	after, err := queryInt(r, "after")
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	limit, err := queryInt(r, "limit")
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	page, err := db.TaskHistory(r.PathValue("id"), after, int(limit))
	if err != nil {
		s.taskError(w, err)
		return
	}
	s.ok(w, map[string]any{"project": project, "changes": page.Changes, "next_cursor": page.Next})
}

func (s *Server) listTaskComments(w http.ResponseWriter, r *http.Request) {
	project, db, ok := s.locateTask(w, r)
	if !ok {
		return
	}
	after, err := queryInt(r, "after")
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	limit, err := queryInt(r, "limit")
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	page, err := db.TaskComments(r.PathValue("id"), after, int(limit))
	if err != nil {
		s.taskError(w, err)
		return
	}
	out := make([]commentResponse, 0, len(page.Comments))
	for _, c := range page.Comments {
		out = append(out, commentView(project, c))
	}
	s.ok(w, map[string]any{"project": project, "comments": out, "next_cursor": page.Next})
}

func (s *Server) addTaskComment(w http.ResponseWriter, r *http.Request) {
	project, db, ok := s.locateTask(w, r)
	if !ok {
		return
	}
	if !s.authorizeTaskWrite(w, r, project) {
		return
	}
	key, ok := s.idempotencyKey(w, r)
	if !ok {
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if !s.decodeTaskBody(w, r, &body) {
		return
	}
	c, err := db.AddTaskComment(r.PathValue("id"), body.Body, taskActor(r), key)
	if err != nil {
		s.taskError(w, err)
		return
	}
	s.log.Info("task comment added", "project", project, "task", c.TaskID, "comment", c.ID, "actor", taskActor(r).Name)
	s.created(w, commentView(project, c))
}

func (s *Server) getTaskComment(w http.ResponseWriter, r *http.Request) {
	project, db, ok := s.locateTask(w, r)
	if !ok {
		return
	}
	c, err := db.GetTaskComment(r.PathValue("id"), r.PathValue("c"))
	if err != nil {
		s.taskError(w, err)
		return
	}
	s.ok(w, commentView(project, c))
}

// --- MCP dispatch with the caller's own authority ---------------------

// taskMux is the route table handler, built once, used to dispatch task
// tool calls in-process.
func (s *Server) taskMux() http.Handler {
	s.muxOnce.Do(func() { s.mux = s.Handler() })
	return s.mux
}

type memResponse struct {
	status int
	header http.Header
	body   bytes.Buffer
}

func (m *memResponse) Header() http.Header { return m.header }
func (m *memResponse) Write(b []byte) (int, error) {
	if m.status == 0 {
		m.status = http.StatusOK
	}
	return m.body.Write(b)
}
func (m *memResponse) WriteHeader(code int) { m.status = code }

// MCPPrincipal tells the hub's /mcp endpoint how one request's task tools
// are served: in-process, against the task routes only, stamped with the
// very identity the bearer middleware authenticated for this request (bound
// here, whatever context the dispatcher passes) — never through the
// trusted local-socket client the legacy tools use. An ordinary token
// additionally sees task tools only. A request without identity (only the
// unix socket, where /mcp is not mounted) gets no task caller at all.
func (s *Server) MCPPrincipal(r *http.Request) (func(ctx context.Context, method, path string, headers map[string]string, body []byte) (int, []byte, error), bool) {
	id, ok := IdentityFrom(r.Context())
	if !ok {
		return nil, false
	}
	call := func(ctx context.Context, method, path string, headers map[string]string, body []byte) (int, []byte, error) {
		req, err := http.NewRequestWithContext(withIdentity(ctx, id), method, "http://aimem"+path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		if req.URL.Path == "/mcp" || !s.ordinaryAllowed(req) {
			return 0, nil, errors.New("not a task route")
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := &memResponse{header: http.Header{}}
		s.taskMux().ServeHTTP(rec, req)
		return rec.status, rec.body.Bytes(), nil
	}
	return call, id.Role == "user"
}
