package server

// Hub admin GUI: a single embedded page served at /admin plus the few
// JSON endpoints it needs beyond the existing API. The page itself holds
// no data — it asks for the hub token once (localStorage) and calls the
// API with it, so serving the page unauthenticated leaks nothing.

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed admin.html
var adminHTML []byte

func (s *Server) adminPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'")
	// The page evolves with the binary; a stale cached copy talks to a
	// newer API and confuses its operator.
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(adminHTML)
}

// metaKeys the GUI may read/write; readOnlyMetaKeys are readable but only
// the service itself writes them (doc synthesis output). Everything else
// stays CLI/service-only.
var metaKeys = map[string]bool{"about": true, "policy": true, "chapters": true, "groups": true, "features": true}
var readOnlyMetaKeys = map[string]bool{"design_doc": true, "design_doc_ts": true, "chapter_proposal": true}

func (s *Server) getMeta(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !metaKeys[key] && !readOnlyMetaKeys[key] {
		s.fail(w, http.StatusForbidden, fmt.Errorf("meta key %q is not exposed", key))
		return
	}
	if db := s.withDB(w, r); db != nil {
		v, err := db.GetMeta(key)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		s.ok(w, map[string]string{"key": key, "value": v})
	}
}

func (s *Server) putMeta(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if !metaKeys[key] {
		s.fail(w, http.StatusForbidden, fmt.Errorf("meta key %q is not exposed", key))
		return
	}
	var req struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	// JSON-typed keys must stay parseable — a torn value here would
	// silently disable group routing everywhere.
	if key == "chapters" || key == "groups" || key == "features" {
		var v any
		if req.Value != "" && json.Unmarshal([]byte(req.Value), &v) != nil {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("%s must be valid JSON", key))
			return
		}
	}
	if key == "policy" && req.Value != "" && req.Value != "all" && req.Value != "domain" {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("policy must be all or domain"))
		return
	}
	if db := s.withDB(w, r); db != nil {
		if err := db.SetMeta(key, req.Value); err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		s.log.Info("admin meta", "project", r.PathValue("p"), "key", key)
		s.ok(w, map[string]string{"key": key, "value": req.Value})
	}
}

// Model config lives in the aimem env file (sourced by curate-all.sh on
// every run, and by the service unit at start). The GUI may edit ONLY
// the two model keys; the file's other lines — API keys among them —
// are never read out or returned.
var modelKeys = []string{"AIMEM_CURATE_MODEL", "AIMEM_EMBED_MODEL"}

func envFilePath() string {
	if v := os.Getenv("AIMEM_ENV_FILE"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "aimem", "env")
}

func parseEnvValue(line, key string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), key+"=")
	if !ok {
		return "", false
	}
	return strings.Trim(rest, `"`), true
}

func (s *Server) getModels(w http.ResponseWriter, _ *http.Request) {
	out := map[string]string{"curate_model": "", "embed_model": ""}
	if raw, err := os.ReadFile(envFilePath()); err == nil {
		for line := range strings.SplitSeq(string(raw), "\n") {
			if v, ok := parseEnvValue(line, "AIMEM_CURATE_MODEL"); ok {
				out["curate_model"] = v
			}
			if v, ok := parseEnvValue(line, "AIMEM_EMBED_MODEL"); ok {
				out["embed_model"] = v
			}
		}
	}
	s.ok(w, out)
}

func (s *Server) putModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CurateModel string `json:"curate_model"`
		EmbedModel  string `json:"embed_model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	want := map[string]string{
		"AIMEM_CURATE_MODEL": strings.TrimSpace(req.CurateModel),
		"AIMEM_EMBED_MODEL":  strings.TrimSpace(req.EmbedModel),
	}
	for k, v := range want {
		if v == "" || strings.ContainsAny(v, "\n\"$`\\") {
			s.fail(w, http.StatusBadRequest, fmt.Errorf("%s: model name must be non-empty without quotes/newlines", k))
			return
		}
	}
	path := envFilePath()
	raw, err := os.ReadFile(path)
	if err != nil {
		s.fail(w, http.StatusInternalServerError, fmt.Errorf("env file: %w", err))
		return
	}
	// Rewrite only the model lines; every other line survives verbatim.
	lines := strings.Split(string(raw), "\n")
	seen := map[string]bool{}
	for i, line := range lines {
		for _, k := range modelKeys {
			if _, ok := parseEnvValue(line, k); ok {
				lines[i] = fmt.Sprintf("%s=%q", k, want[k])
				seen[k] = true
			}
		}
	}
	for _, k := range modelKeys {
		if !seen[k] {
			lines = append(lines, fmt.Sprintf("%s=%q", k, want[k]))
		}
	}
	// Always end with a newline: without it, anything appended later
	// (an operator adding AIMEM_HUB_NAME, say) glues onto the last
	// value and silently corrupts two settings at once.
	body := strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Warn("admin models updated", "curate", want["AIMEM_CURATE_MODEL"], "embed", want["AIMEM_EMBED_MODEL"])
	s.ok(w, map[string]string{
		"curate_model": want["AIMEM_CURATE_MODEL"], "embed_model": want["AIMEM_EMBED_MODEL"],
		"note": "takes effect on the next curation run; restart aimem.service to apply the embed model to live recall",
	})
}

// auditLog serves a project's knowledge-mutation trail (the admin Log
// tab's "knowledge" source). Curation runs in its own process, so its
// conflicts and supersessions reach the GUI through this table rather
// than the serve process's in-memory log ring.
func (s *Server) auditLog(w http.ResponseWriter, r *http.Request) {
	if db := s.withDB(w, r); db != nil {
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		entries, err := db.AuditLog(n)
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		s.ok(w, map[string]any{"entries": entries})
	}
}

// tag attaches one tag to a memory — the explicit filing path. A fact
// may carry several chapters (store.MaxChaptersPerFact); the first one
// filed stays primary and drives the design document.
func (s *Server) tag(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Tag == "" {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("body wants {\"tag\": \"...\"}"))
		return
	}
	if db := s.withDB(w, r); db != nil {
		if err := db.Tag(r.PathValue("id"), req.Tag, "admin"); err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		s.log.Info("admin tag", "project", r.PathValue("p"), "tag", req.Tag)
		s.ok(w, map[string]string{"tagged": req.Tag})
	}
}

// untag detaches one tag from a memory — the correction path for a
// mis-filed fact.
func (s *Server) untag(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tag string `json:"tag"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Tag == "" {
		s.fail(w, http.StatusBadRequest, fmt.Errorf("body wants {\"tag\": \"...\"}"))
		return
	}
	if db := s.withDB(w, r); db != nil {
		if err := db.RemoveTag(r.PathValue("id"), req.Tag); err != nil {
			s.fail(w, http.StatusBadRequest, err)
			return
		}
		s.ok(w, map[string]string{"untagged": req.Tag})
	}
}

// dropProject deletes a whole project DB. Destructive and audited in the
// log; the store refuses the user DB, everything else is the operator's
// explicit call (the id must be spelled out exactly).
func (s *Server) dropProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("p")
	if err := s.reg.Drop(id); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	// Stale curate cursor would confuse a future project reusing the id.
	os.Remove(filepath.Join(s.reg.Root(), "curate", id+".cursor"))
	s.log.Warn("project dropped", "project", id)
	s.ok(w, map[string]string{"dropped": id})
}

// renameProject re-keys a project. Derived project ids are unreadable by
// design (a slug plus a hash of the remote, root commit or path), and a
// hub accumulates ids whose origin nobody remembers; this makes the
// catalog legible without touching what it holds.
func (s *Server) renameProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("p")
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	name := strings.TrimSpace(body.Name)
	if err := s.reg.Rename(id, name); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	// The curate cursor is keyed by project id. Left behind, the renamed
	// project looks never-curated and replays its whole journal, while a
	// future project reusing the old id inherits someone else's position.
	cur := filepath.Join(s.reg.Root(), "curate")
	if err := os.Rename(filepath.Join(cur, id+".cursor"), filepath.Join(cur, name+".cursor")); err != nil && !os.IsNotExist(err) {
		s.log.Warn("curate cursor not moved with rename", "from", id, "to", name, "err", err)
	}
	s.log.Warn("project renamed", "from", id, "to", name)
	s.ok(w, map[string]string{"renamed": id, "to": name})
}

// mergeProject folds one project into another (store.MergeProject):
// the fix for one real project split across two ids — a derived id from
// before its .aimem.json pin plus the pinned name — which doubles the
// KB origin facet and the catalog.
func (s *Server) mergeProject(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("p")
	var body struct {
		Into string `json:"into"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	into := strings.TrimSpace(body.Into)
	events, mems, runs, err := s.reg.MergeProject(id, into)
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	// The source's curate cursor is meaningless once the source is gone.
	if err := os.Remove(filepath.Join(s.reg.Root(), "curate", id+".cursor")); err != nil && !os.IsNotExist(err) {
		s.log.Warn("stale curate cursor not removed after merge", "project", id, "err", err)
	}
	s.log.Warn("project merged", "from", id, "into", into,
		"events", events, "memories", mems, "runs", runs)
	s.ok(w, map[string]any{"merged": id, "into": into,
		"events": events, "memories": mems, "curate_runs": runs})
}

func (s *Server) curateRuns(w http.ResponseWriter, r *http.Request) {
	if db := s.withDB(w, r); db != nil {
		runs, err := db.CurateRuns()
		if err != nil {
			s.fail(w, http.StatusInternalServerError, err)
			return
		}
		s.ok(w, map[string]any{"runs": runs})
	}
}

// overview is the GUI's one-shot bootstrap: every project with stats,
// plus group config and membership, so the page renders without an N+1
// request storm.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ids, err := s.reg.Projects()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	type row struct {
		ID       string          `json:"id"`
		Stats    any             `json:"stats"`
		Groups   []string        `json:"groups,omitempty"`
		About    string          `json:"about,omitempty"`
		Policy   string          `json:"policy,omitempty"`
		Chapters json.RawMessage `json:"chapters,omitempty"`
		Features json.RawMessage `json:"features,omitempty"`
		DocTS    string          `json:"doc_ts,omitempty"` // design_doc_ts when a doc exists
	}
	out := make([]row, 0, len(ids))
	for _, id := range ids {
		db, err := s.reg.Open(id)
		if err != nil {
			continue
		}
		rw := row{ID: id}
		rw.Stats, _ = db.Stats()
		if raw, _ := db.GetMeta("groups"); raw != "" {
			json.Unmarshal([]byte(raw), &rw.Groups)
		}
		if strings.HasPrefix(id, "group-") {
			rw.About, _ = db.GetMeta("about")
			rw.Policy, _ = db.GetMeta("policy")
			if raw, _ := db.GetMeta("chapters"); raw != "" {
				rw.Chapters = json.RawMessage(raw)
			}
			if raw, _ := db.GetMeta("features"); raw != "" {
				rw.Features = json.RawMessage(raw)
			}
			rw.DocTS, _ = db.GetMeta("design_doc_ts")
		}
		out = append(out, rw)
	}
	s.ok(w, map[string]any{"projects": out})
}
