package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

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
	// The drop may wait for a save and then run store after store: its
	// answer must outlive the listener's 30-second write deadline.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(dropDeadline))
	s.modelsMu.Lock()
	defer s.modelsMu.Unlock()
	// A request whose client gave up while it waited deletes nothing.
	if err := r.Context().Err(); err != nil {
		s.fail(w, http.StatusServiceUnavailable, fmt.Errorf("refused: the request was cancelled before the drop began: %w", err))
		return
	}
	live := s.embedder().Key()
	fileModel, fileKey, err := s.envFileEmbedKey()
	if err != nil {
		// The key the timer will embed with cannot be known: refuse
		// rather than drop a key the next save or run makes live.
		s.fail(w, http.StatusConflict, fmt.Errorf("refused: the env file could not be read: %w", err))
		return
	}
	if fileModel != "" && fileKey == "" {
		s.fail(w, http.StatusConflict, fmt.Errorf("refused: the env file names AIMEM_EMBED_MODEL=%q, which cannot be resolved here, so its key is unknown", fileModel))
		return
	}
	// req.Key is not empty, so an empty live or file key (embeddings off)
	// matches nothing.
	switch req.Key {
	case live:
		s.fail(w, http.StatusConflict, fmt.Errorf("refused: %q is the key the running service queries with", req.Key))
		return
	case fileKey:
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
		u, err := store.EmbedKeyUsage{}, r.Context().Err()
		if err != nil {
			err = fmt.Errorf("the request was cancelled; this project and the ones after it were not touched: %w", err)
		} else {
			u, err = s.dropOne(id, req.Key, req.DryRun)
		}
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

// dropDeadline bounds one drop's answer: at the measured 0.2 s per store
// of 20,000 vectors of 1,536 floats it leaves room for far more stores
// than a hub holds.
const dropDeadline = 10 * time.Minute

// dropOne removes, or on a dry run counts, one project's vectors of key.
// A project with no database file holds none and is not created. The dry
// run reads the file read-only, so it neither creates nor migrates it;
// the real drop opens the store as every command does, which migrates a
// store of an older schema first.
func (s *Server) dropOne(id, key string, dry bool) (store.EmbedKeyUsage, error) {
	if dry {
		return s.reg.EmbedKeyRows(id, key)
	}
	if has, err := s.reg.HasJournal(id); err != nil || !has {
		return store.EmbedKeyUsage{Model: key}, err
	}
	db, err := s.reg.Open(id)
	if err != nil {
		return store.EmbedKeyUsage{Model: key}, err
	}
	return db.DropEmbeddings(key)
}

// envFileEmbedKey reads the embed model the env file names now, and the
// key it resolves to ("" when it cannot be served here). A missing file
// names no model; any other read error is returned.
func (s *Server) envFileEmbedKey() (model, key string, err error) {
	var dim int
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
		// The timer resolves the key with the file's dimension, not with
		// whatever this process was started with.
		if v, ok := parseEnvValue(line, "AIMEM_EMBED_DIM"); ok {
			dim, _ = strconv.Atoi(v)
		}
	}
	return model, embed.ForModelDim(s.reg.Root(), model, dim).Key(), nil
}
