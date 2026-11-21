package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strings"

	"aimem/internal/embed"
	"aimem/internal/store"
)

// dropResult is one project's part of an embeddings drop.
type dropResult struct {
	Project string `json:"project"`
	Rows    int    `json:"rows"`
	Bytes   int64  `json:"bytes"`
	Error   string `json:"error,omitempty"`
}

// dropEmbeddings removes the vectors of one embedding key, project by
// project. It runs under modelsMu, the lock the models PUT holds while it
// rewrites the env file and swaps the live client, so a save waits for
// the whole drop: the key cannot become live while its vectors go. It
// refuses the key the service queries with and the key the env file
// names at that moment. dry_run counts under the same lock and writes
// nothing. It stops at the first project that fails and says which;
// complete is false then, and the projects before it are what was done.
func (s *Server) dropEmbeddings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Key      string   `json:"key"`
		DryRun   bool     `json:"dry_run"`
		Projects []string `json:"projects"` // empty: every project
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&req); err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}
	req.Key = strings.TrimSpace(req.Key)
	if req.Key == "" {
		s.fail(w, http.StatusBadRequest, errors.New("key: the embedding key to drop is required"))
		return
	}
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	live := s.embedder().Key()
	fileModel, fileKey, err := s.envFileEmbedKey()
	if err != nil {
		// The key the timer will embed with cannot be known: refuse
		// rather than drop a key the next save or run makes live.
		s.fail(w, http.StatusConflict, fmt.Errorf("refused: the env file could not be read: %w", err))
		return
	}
	// req.Key is not empty, so an empty live or file key (embeddings off)
	// matches nothing.
	switch req.Key {
	case live:
		s.fail(w, http.StatusConflict, fmt.Errorf("refused: %q is the key the running service queries with", req.Key))
		return
	case fileKey, fileModel:
		s.fail(w, http.StatusConflict, fmt.Errorf("refused: %q is the key the env file names (AIMEM_EMBED_MODEL=%q)", req.Key, fileModel))
		return
	}
	all, err := s.reg.Projects()
	if err != nil {
		s.fail(w, http.StatusInternalServerError, err)
		return
	}
	ids := req.Projects
	if len(ids) == 0 {
		ids = all
	}
	for _, id := range ids {
		if !slices.Contains(all, id) {
			s.fail(w, http.StatusNotFound, fmt.Errorf("no project %q on this hub", id))
			return
		}
	}
	out := map[string]any{"key": req.Key, "dry_run": req.DryRun, "live_key": live, "file_key": fileKey}
	var done []dropResult
	complete := true
	for _, id := range ids {
		res := dropResult{Project: id}
		u, err := s.dropOne(id, req.Key, req.DryRun)
		res.Rows, res.Bytes = u.Rows, u.Bytes
		if err != nil {
			res.Error = err.Error()
			done = append(done, res)
			complete = false
			out["stopped_at"] = id
			break
		}
		done = append(done, res)
	}
	out["projects"] = done
	out["complete"] = complete
	if !req.DryRun && len(ids) > 0 {
		s.log.Warn("admin embeddings dropped", "key", req.Key, "projects", len(done), "complete", complete)
	}
	s.ok(w, out)
}

func (s *Server) dropOne(id, key string, dry bool) (store.EmbedKeyUsage, error) {
	db, err := s.reg.Open(id)
	if err != nil {
		return store.EmbedKeyUsage{Model: key}, err
	}
	if !dry {
		return db.DropEmbeddings(key)
	}
	ks, err := db.EmbedKeys()
	u := store.EmbedKeyUsage{Model: key}
	for _, k := range ks {
		if k.Model == key {
			u.Rows += k.Rows
			u.Bytes += k.Bytes
		}
	}
	return u, err
}

// envFileEmbedKey reads the embed model the env file names now, and the
// key it resolves to ("" when it cannot be served here). A missing file
// names no model; any other read error is returned.
func (s *Server) envFileEmbedKey() (model, key string, err error) {
	raw, err := os.ReadFile(envFilePath())
	if errors.Is(err, fs.ErrNotExist) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if v, ok := parseEnvValue(line, "AIMEM_EMBED_MODEL"); ok {
			model = v
		}
	}
	return model, embed.ForModel(s.reg.Root(), model).Key(), nil
}
