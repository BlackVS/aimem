// Package server exposes the store over a local HTTP API bound to a Unix
// socket. The API is deliberately small: append, list, timeline, latest,
// search, retention, health. No LLM in any write path; recall optionally
// embeds the query (read-side, best-effort) when this machine opts in.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"aimem/internal/embed"
	"aimem/internal/schema"
	"aimem/internal/store"
)

// maxBodyBytes caps one ingestion request (all fields are further capped
// individually by the redaction layer).
const maxBodyBytes = 1 << 20

// Version is stamped by main so /v1/health (and the admin header) can
// show which binary is serving — indispensable when a browser tab may
// be running a stale copy of the GUI.
var Version = ""

// Server routes API requests onto a store registry.
type Server struct {
	reg  *store.Registry
	log  *slog.Logger
	emb  *embed.Client // nil = semantic recall off (BM25 only)
	ring *LogRing      // nil = no admin Log tab data
}

func New(reg *store.Registry, log *slog.Logger) *Server {
	return &Server{reg: reg, log: log, emb: embed.ForRoot(reg.Root())}
}

// WithLogRing exposes ring on /v1/logs for the admin GUI's Log tab.
func (s *Server) WithLogRing(ring *LogRing) *Server { s.ring = ring; return s }

// SocketPath returns the canonical socket location. Unix socket paths are
// limited to ~108 bytes, so the state root (which can be arbitrarily deep)
// is only the last resort: AIMEM_SOCKET wins, then XDG_RUNTIME_DIR.
func SocketPath(root string) string {
	if v := os.Getenv("AIMEM_SOCKET"); v != "" {
		return v
	}
	if rd := os.Getenv("XDG_RUNTIME_DIR"); rd != "" {
		return filepath.Join(rd, "aimem.sock")
	}
	return filepath.Join(root, "aimem.sock")
}

// SentinelPath is the clean-shutdown marker location for a state root.
func SentinelPath(root string) string { return filepath.Join(root, "clean-shutdown") }

// Route is one entry of the API surface. The table is the single
// source of truth: the mux is built from it, admin-only gating hangs
// off it, and the OpenAPI parity test walks it — so a route cannot
// exist without appearing in the spec, or carry the wrong gate.
type Route struct {
	Method  string
	Pattern string // net/http mux pattern; "/{$}" is the root page
	handler http.HandlerFunc
	Admin   bool // admin-only over the authenticated TCP listener
}

// Routes enumerates the complete HTTP surface. Admin marks operator
// actions (config, destructive project ops, logs); everything else is
// available to writer tokens. The three public pages (/admin, /,
// /v1/status) are exempted from auth in authWrapper, not here.
func (s *Server) Routes() []Route {
	return []Route{
		{"GET", "/v1/health", s.health, false},
		{"POST", "/v1/events", s.append, false},
		{"GET", "/v1/projects", s.projects, false},
		{"GET", "/v1/projects/{p}/sessions", s.sessions, false},
		{"GET", "/v1/projects/{p}/sessions/{s}/timeline", s.timeline, false},
		{"GET", "/v1/projects/{p}/sessions/{s}/latest", s.latest, false},
		{"GET", "/v1/projects/{p}/search", s.search, false},
		{"POST", "/v1/projects/{p}/retention", s.retention, true},
		{"POST", "/v1/projects/{p}/memories", s.remember, false},
		{"GET", "/v1/projects/{p}/memories", s.memories, false},
		{"GET", "/v1/projects/{p}/memories/recall", s.recall, false},
		{"GET", "/v1/projects/{p}/memories/review", s.reviewQueue, false},
		{"POST", "/v1/projects/{p}/memories/{id}/confirm", s.confirmMemory, false},
		{"POST", "/v1/projects/{p}/memories/import", s.importMemory, false},
		{"POST", "/v1/projects/{p}/curate-runs/import", s.importCurateRun, false},
		{"POST", "/v1/projects/{p}/memories/{id}/forget", s.forget, false},
		{"POST", "/v1/projects/{p}/memories/{id}/supersede", s.supersede, false},
		{"POST", "/v1/projects/{p}/memories/{id}/pin", s.pin, false},
		{"POST", "/v1/projects/{p}/memories/{id}/link", s.link, false},
		{"POST", "/v1/projects/{p}/memories/{id}/tag", s.tag, false},
		{"POST", "/v1/projects/{p}/memories/{id}/untag", s.untag, false},
		{"GET", "/v1/projects/{p}/meta/{key}", s.getMeta, false},
		{"PUT", "/v1/projects/{p}/meta/{key}", s.putMeta, false},
		{"GET", "/v1/projects/{p}/curate-runs", s.curateRuns, false},
		{"GET", "/v1/projects/{p}/audit", s.auditLog, false},
		{"GET", "/v1/overview", s.overview, false},
		{"DELETE", "/v1/projects/{p}", s.dropProject, true},
		{"POST", "/v1/projects/{p}/rename", s.renameProject, true},
		{"GET", "/v1/projects/{p}/docs", s.listDocs, false},
		{"GET", "/v1/projects/{p}/docs/{name}", s.getDoc, false},
		{"GET", "/v1/projects/{p}/docs/{name}/log", s.docLog, false},
		{"POST", "/v1/projects/{p}/docs/{name}/merge", s.mergeDoc, false},
		{"PUT", "/v1/projects/{p}/docs/{name}", s.putDoc, false},
		{"DELETE", "/v1/projects/{p}/docs/{name}", s.deleteDoc, false},
		{"GET", "/v1/projects/{p}/collections", s.listCollections, false},
		{"GET", "/v1/projects/{p}/collections/{c}/records", s.listRecords, false},
		{"GET", "/v1/projects/{p}/collections/{c}/records/{id...}", s.getRecord, false},
		{"PUT", "/v1/projects/{p}/collections/{c}/records/{id...}", s.putRecord, false},
		{"DELETE", "/v1/projects/{p}/collections/{c}/records/{id...}", s.deleteRecord, false},
		{"GET", "/v1/sync/events", s.syncEventsOut, false},
		{"POST", "/v1/sync/events", s.syncEventsIn, false},
		{"GET", "/v1/sync/memories", s.syncMemoriesOut, false},
		{"POST", "/v1/sync/memories", s.syncMemoriesIn, false},
		{"GET", "/v1/sync/group-config", s.syncConfigOut, false},
		{"POST", "/v1/sync/group-config", s.syncConfigIn, false},
		{"GET", "/v1/config/models", s.getModels, true},
		{"PUT", "/v1/config/models", s.putModels, true},
		{"GET", "/v1/config/providers", s.getProviders, true},
		{"PUT", "/v1/config/providers", s.putProviders, true},
		{"POST", "/v1/config/providers/test", s.testProvider, true},
		{"GET", "/v1/config/providers/{name}/models", s.providerModels, true},
		{"GET", "/v1/logs", s.logs, true},
		{"POST", "/v1/projects/{p}/chapter-proposal", s.proposeChapters, true},
		{"POST", "/v1/projects/{p}/chapter-proposal/apply", s.applyChapters, true},
		{"GET", "/v1/openapi.json", s.openAPI, false},
		{"GET", "/admin", s.adminPage, false},
		{"GET", "/v1/status", s.status, false},
		{"GET", "/{$}", s.statusPage, false},
	}
}

// Handler builds the HTTP mux from the route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.Routes() {
		h := rt.handler
		if rt.Admin {
			h = s.requireAdmin(h)
		}
		mux.HandleFunc(rt.Method+" "+rt.Pattern, h)
	}
	return mux
}

type memoryRequest struct {
	Text    string   `json:"text"`
	Actor   string   `json:"actor"`
	ValidAt string   `json:"valid_at"`
	Kind    string   `json:"kind"`
	Tags    []string `json:"tags"`
	Sources []string `json:"sources"`
	Pinned  *bool    `json:"pinned"`
	To      string   `json:"to"`  // link target
	Rel     string   `json:"rel"` // link relation
}

func (s *Server) decodeMem(w http.ResponseWriter, r *http.Request) (*memoryRequest, bool) {
	var req memoryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return nil, false
	}
	return &req, true
}

func (s *Server) remember(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	req, ok := s.decodeMem(w, r)
	if !ok {
		return
	}
	id, reasserted, err := db.Remember(req.Text, req.Actor, store.RememberOpts{
		ValidAt: req.ValidAt, Kind: req.Kind, Tags: req.Tags, Sources: req.Sources,
	})
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.log.Info("remember", "project", r.PathValue("p"), "id", id, "reasserted", reasserted)
	s.ok(w, map[string]any{"id": id, "reasserted": reasserted})
}

func (s *Server) memories(w http.ResponseWriter, r *http.Request) {
	if db := s.withDB(w, r); db != nil {
		mems, err := db.Memories(r.URL.Query().Get("stale") == "1")
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		s.ok(w, map[string]any{"memories": mems})
	}
}

func (s *Server) recall(w http.ResponseWriter, r *http.Request) {
	if db := s.withDB(w, r); db != nil {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			s.fail(w, http.StatusBadRequest, errors.New("missing q"))
			return
		}
		budget, _ := strconv.Atoi(r.URL.Query().Get("budget"))
		opts := store.RecallOpts{
			TokenBudget: budget,
			Tag:         r.URL.Query().Get("tag"),
			Kind:        r.URL.Query().Get("kind"),
		}
		// Semantic leg is best-effort: an embedding failure (proxy down,
		// egress blocked) degrades silently to BM25-only.
		if s.emb != nil {
			if vecs, _, err := s.emb.Embed([]string{q}); err == nil {
				opts.QueryVec, opts.EmbedModel = vecs[0], s.emb.Key()
			} else {
				s.log.Warn("query embedding failed; BM25-only recall", "err", err)
			}
		}
		mems, err := db.Recall(q, opts)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		s.ok(w, map[string]any{"memories": mems})
	}
}

func (s *Server) importMemory(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	var req struct {
		Memory store.Memory `json:"memory"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if err := db.ImportMemory(&req.Memory); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.ok(w, map[string]any{"imported": true})
}

func (s *Server) importCurateRun(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	var req struct {
		Run store.CurateRun `json:"run"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	if req.Run.ID == "" || req.Run.TS == "" {
		s.fail(w, http.StatusBadRequest, errors.New("run id and ts required"))
		return
	}
	if err := db.AddCurateRun(&req.Run); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.ok(w, map[string]any{"imported": true})
}

func (s *Server) forget(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	req, ok := s.decodeMem(w, r)
	if !ok {
		return
	}
	if err := db.Forget(r.PathValue("id"), req.Actor); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.ok(w, map[string]any{"forgotten": r.PathValue("id")})
}

func (s *Server) supersede(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	req, ok := s.decodeMem(w, r)
	if !ok {
		return
	}
	newID, err := db.Supersede(r.PathValue("id"), req.Text, req.Actor,
		store.RememberOpts{Kind: req.Kind, Tags: req.Tags, Sources: req.Sources})
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.ok(w, map[string]any{"superseded": r.PathValue("id"), "id": newID})
}

func (s *Server) link(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	req, ok := s.decodeMem(w, r)
	if !ok {
		return
	}
	if err := db.Link(r.PathValue("id"), req.To, req.Rel, req.Actor); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.ok(w, map[string]any{"from": r.PathValue("id"), "to": req.To, "rel": req.Rel})
}

func (s *Server) pin(w http.ResponseWriter, r *http.Request) {
	db := s.withDB(w, r)
	if db == nil {
		return
	}
	req, ok := s.decodeMem(w, r)
	if !ok {
		return
	}
	pinned := true
	if req.Pinned != nil {
		pinned = *req.Pinned
	}
	if err := db.Pin(r.PathValue("id"), pinned, req.Actor); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.ok(w, map[string]any{"id": r.PathValue("id"), "pinned": pinned})
}

// ListenAndServe serves on the Unix socket until the listener is closed.
// It manages the clean-shutdown sentinel: removed while running, written on
// graceful stop by the caller via WriteSentinel.
func (s *Server) ListenAndServe(root string) (*http.Server, net.Listener, error) {
	sock := SocketPath(root)
	// A leftover socket from a dead process would block binding.
	if _, err := os.Stat(sock); err == nil {
		if _, derr := net.DialTimeout("unix", sock, time.Second); derr == nil {
			return nil, nil, fmt.Errorf("another aimem is already serving on %s", sock)
		}
		os.Remove(sock)
	}
	if _, err := os.Stat(SentinelPath(root)); errors.Is(err, os.ErrNotExist) {
		s.log.Warn("no clean-shutdown sentinel: previous run may have crashed",
			"state_root", root)
	}
	os.Remove(SentinelPath(root))
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, nil, err
	}
	srv := &http.Server{Handler: s.Handler(), ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("serve", "err", err)
		}
	}()
	s.log.Info("listening", "socket", sock)
	return srv, ln, nil
}

// authWrapper is the hub's entire auth surface, extracted so tests
// exercise the REAL gate rather than a re-implementation that can drift.
func (s *Server) authWrapper(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Three unauthenticated GETs, and only three. /admin is static
		// chrome with zero data — it collects the hub token in the browser
		// and calls the API with it. / and /v1/status are public by
		// construction: liveness, build, uptime, nothing about what the
		// hub holds. Everything else stays token-gated.
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/admin":
				s.adminPage(w, r)
				return
			case "/":
				s.statusPage(w, r)
				return
			case "/v1/status":
				s.status(w, r)
				return
			}
		}
		presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		id, ok := s.authenticate(token, presented)
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), id)))
	})
}

// ListenTCP starts the optional authenticated TCP listener (hub mode):
// the full API plus extra routes (e.g. /mcp), every request gated by a
// bearer token. TLS is used when cert and key are provided; on a trusted
// segment plain HTTP is the operator's explicit choice.
func (s *Server) ListenTCP(addr, token, certFile, keyFile string, extra map[string]http.Handler) (*http.Server, error) {
	if token == "" {
		return nil, errors.New("AIMEM_HTTP_TOKEN is required when AIMEM_HTTP_LISTEN is set")
	}
	mux := http.NewServeMux()
	mux.Handle("/", s.Handler())
	for pattern, h := range extra {
		mux.Handle(pattern, h)
	}
	authed := s.authWrapper(token, mux)
	srv := &http.Server{
		Addr: addr, Handler: authed,
		// Header timeout guards slowloris; the body timeouts are wide
		// because the /v1/sync streams legitimately run for minutes on a
		// first sync (DESIGN-hub-sync open question 3).
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       15 * time.Minute, WriteTimeout: 15 * time.Minute,
	}
	go func() {
		var err error
		if certFile != "" && keyFile != "" {
			err = srv.ListenAndServeTLS(certFile, keyFile)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("tcp serve", "err", err)
		}
	}()
	s.log.Info("tcp listening", "addr", addr, "tls", certFile != "")
	return srv, nil
}

// WriteSentinel records a graceful shutdown.
func WriteSentinel(root string) error {
	return os.WriteFile(SentinelPath(root), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

type appendRequest struct {
	ProjectID string       `json:"project_id"`
	Groups    *[]string    `json:"groups,omitempty"` // declared knowledge groups (nil = unknown)
	Hub       *string      `json:"hub,omitempty"`    // declared hub binding (nil = unknown)
	Event     schema.Event `json:"event"`
}

func (s *Server) append(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		s.fail(w, http.StatusRequestEntityTooLarge, err)
		return
	}
	var req appendRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	id, inserted, err := s.appendOne(&req)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	s.log.Info("append", "project", req.ProjectID, "session", req.Event.SessionID,
		"turn", req.Event.TurnID, "inserted", inserted)
	s.ok(w, map[string]any{"id": id, "inserted": inserted})
}

// appendOne is the storage core shared by the real-time push handler
// and the sync import stream (POST /v1/sync/events).
func (s *Server) appendOne(req *appendRequest) (id string, inserted bool, err error) {
	db, err := s.reg.Open(req.ProjectID)
	if err != nil {
		return "", false, err
	}
	id, inserted, err = db.Append(&req.Event)
	if err != nil {
		return "", false, err
	}
	// Group membership rides on event pushes; keep the project's meta
	// current so curation (here or on the hub) knows where group-scoped
	// facts may land. Best-effort — never fails the append.
	if req.Groups != nil {
		if b, err := json.Marshal(*req.Groups); err == nil {
			if cur, _ := db.GetMeta("groups"); cur != string(b) {
				if err := db.SetMeta("groups", string(b)); err != nil {
					s.log.Warn("groups meta", "project", req.ProjectID, "err", err)
				}
			}
		}
	}
	// Hub binding rides along the same way; recorded as project meta it
	// lets `aimem sync --hub` partition projects between hubs without
	// needing access to each project's working directory.
	if req.Hub != nil {
		if cur, _ := db.GetMeta("hub"); cur != *req.Hub {
			if err := db.SetMeta("hub", *req.Hub); err != nil {
				s.log.Warn("hub meta", "project", req.ProjectID, "err", err)
			}
		}
	}
	return id, inserted, nil
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	ids, err := s.reg.Projects()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	body := map[string]any{"status": "ok", "projects": len(ids), "state_root": s.reg.Root()}
	if Version != "" {
		body["version"] = Version
	}
	// A friendly name distinguishes hubs in browser tabs and headers;
	// without one the hostname already differs, so this never renders
	// two identical-looking consoles.
	if n := os.Getenv("AIMEM_HUB_NAME"); n != "" {
		body["hub_name"] = n
	}
	if hn, err := os.Hostname(); err == nil {
		// Lets clients (GUI usage view) separate this host's own runs
		// from imported history that traveled with migrated projects.
		body["host"] = hn
	}
	if res := resources(s.reg.Root()); len(res) > 0 {
		body["resources"] = res
	}
	s.ok(w, body)
}

func (s *Server) projects(w http.ResponseWriter, _ *http.Request) {
	ids, err := s.reg.Projects()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.ok(w, map[string]any{"projects": ids})
}

func (s *Server) withDB(w http.ResponseWriter, r *http.Request) *store.DB {
	// Reads must never create: a GET for a dropped/mistyped project would
	// otherwise resurrect it as an empty husk (stale MCP facades poll old
	// ids long after a rename). Writes keep create-on-open.
	open := s.reg.Open
	if r.Method == http.MethodGet {
		open = s.reg.OpenExisting
	}
	db, err := open(r.PathValue("p"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return nil
	}
	return db
}

func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	if db := s.withDB(w, r); db != nil {
		list, err := db.Sessions()
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		s.ok(w, map[string]any{"sessions": list})
	}
}

func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	if db := s.withDB(w, r); db != nil {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		evs, err := db.Timeline(r.PathValue("s"), limit)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		s.ok(w, map[string]any{"events": evs})
	}
}

func (s *Server) latest(w http.ResponseWriter, r *http.Request) {
	if db := s.withDB(w, r); db != nil {
		ev, err := db.Latest(r.PathValue("s"))
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		s.ok(w, map[string]any{"event": ev})
	}
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	if db := s.withDB(w, r); db != nil {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			s.fail(w, http.StatusBadRequest, errors.New("missing q"))
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		evs, err := db.Search(q, limit)
		if err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		// Shared documents ride the same query (FEATURE-PROPOSALS #4):
		// search FINDS a doc, retrieval stays fetch-by-name and whole —
		// never ranked into the event list.
		docs, err := db.SearchDocs(q, 5)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		if docs == nil {
			docs = []store.DocMatch{}
		}
		s.ok(w, map[string]any{"events": evs, "docs": docs})
	}
}

func (s *Server) retention(w http.ResponseWriter, r *http.Request) {
	if db := s.withDB(w, r); db != nil {
		var req struct {
			MaxAgeDays int   `json:"max_age_days"`
			MaxBytes   int64 `json:"max_bytes"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		n, err := db.Retention(time.Duration(req.MaxAgeDays)*24*time.Hour, req.MaxBytes)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		s.log.Info("retention", "project", r.PathValue("p"), "deleted", n)
		s.ok(w, map[string]any{"deleted": n})
	}
}

func (s *Server) ok(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *Server) fail(w http.ResponseWriter, code int, err error) {
	s.log.Warn("request failed", "code", code, "err", err)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
