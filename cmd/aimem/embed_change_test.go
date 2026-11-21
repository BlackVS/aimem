package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/embed"
	"aimem/internal/server"
	"aimem/internal/store"
)

// embedChangeRig is a state root with one project of two facts embedded
// under old-embed, and an embeddings endpoint answering per model.
func embedChangeRig(t *testing.T) (root string, reg *store.Registry, ids []string) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		vec := []float32{1, 0}
		if in.Model == "new-embed" {
			vec = []float32{0, 1}
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var out struct {
			Data []item `json:"data"`
		}
		for i := range in.Input {
			out.Data = append(out.Data, item{i, vec})
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(up.Close)
	root = t.TempDir()
	home := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("AIMEM_ENV_FILE", filepath.Join(home, ".config", "aimem", "env"))
	t.Setenv("AIMEM_OPENAI_API_KEY", "k")
	t.Setenv("AIMEM_OPENAI_BASE_URL", up.URL)
	t.Setenv("AIMEM_EMBED_DIM", "")
	t.Setenv("AIMEM_EMBED_MODEL", "old-embed")
	var err error
	if reg, err = store.NewRegistry(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reg.Close)
	db, _ := reg.Open("proj-ec")
	for _, txt := range []string{"journals live in sqlite", "the hub runs as a user service"} {
		id, _, err := db.Remember(txt, "test", store.RememberOpts{})
		if err != nil {
			t.Fatal(err)
		}
		db.SetEmbedding(id, "old-embed", 2, embed.Encode([]float32{1, 0}))
		ids = append(ids, id)
	}
	return root, reg, ids
}

// The order without a dip: the backfill runs under the new model first,
// named for that one command, while the service and the env file still
// say the old one; then the PUT. The service's first query under the new
// key already finds every fact.
func TestBackfillUnderTheNewModelBeforeThePut(t *testing.T) {
	root, reg, ids := embedChangeRig(t)
	envFile := os.Getenv("AIMEM_ENV_FILE")
	os.MkdirAll(filepath.Dir(envFile), 0o700)
	os.WriteFile(envFile, []byte("AIMEM_CURATE_MODEL=c\nAIMEM_EMBED_MODEL=old-embed\n"), 0o600)
	srv := server.New(reg, slog.New(slog.NewTextHandler(new(strings.Builder), nil)))
	t.Cleanup(func() { srv.Close() })
	h := srv.Handler()

	// The command line names the new model; the env file still names the
	// old one. Process env wins over the file (loadEnvFile), so the one
	// command embeds under the new key and nothing else changes.
	t.Setenv("AIMEM_EMBED_MODEL", "new-embed")
	loadEnvFile()
	if got := os.Getenv("AIMEM_EMBED_MODEL"); got != "new-embed" {
		t.Fatalf("the env file overrode the command's model: %q", got)
	}
	if err := embedCmd([]string{"--all"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AIMEM_EMBED_MODEL", "old-embed")
	db, _ := reg.Open("proj-ec")
	if s, _ := db.Stats("new-embed"); s.Embedded != 2 || s.Memories != 2 {
		t.Fatalf("coverage under the new key before the PUT: %d of %d", s.Embedded, s.Memories)
	}

	if w := doReq(h, "PUT", "/v1/config/models", `{"curate_model":"c","embed_model":"new-embed"}`); w.Code != 200 ||
		!strings.Contains(w.Body.String(), "every live fact already has a vector under new-embed") {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	w := doReq(h, "GET", "/v1/projects/proj-ec/memories/recall?q=zzqx", "")
	var out struct {
		Memories []struct {
			ID string `json:"id"`
		} `json:"memories"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Memories) != 2 {
		t.Fatalf("first semantic recall after the PUT found %d of %v", len(out.Memories), ids)
	}
	_ = root
}

func doReq(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// --drop refuses the key this command resolves to and the key the
// running service queries with, and refuses when the service cannot say
// which key that is. A dry run counts and removes nothing.
func TestEmbedDropRefusesTheKeysInUse(t *testing.T) {
	_, reg, _ := embedChangeRig(t)
	db, _ := reg.Open("proj-ec")
	live := func(key string, known bool, err error) func() (string, bool, error) {
		return func() (string, bool, error) { return key, known, err }
	}
	for name, tc := range map[string]struct {
		key, own string
		svc      func() (string, bool, error)
		want     string
	}{
		"own key":             {"old-embed", "old-embed", live("other", true, nil), "is the current embedding key here"},
		"service key":         {"old-embed", "new-embed", live("old-embed", true, nil), "is the key the running service queries with"},
		"service unknown":     {"old-embed", "new-embed", live("", false, nil), "does not report its embedding key"},
		"service unreachable": {"old-embed", "new-embed", live("", false, errors.New("dial: refused")), "could not be asked"},
	} {
		t.Run(name, func(t *testing.T) {
			err := embedDrop(reg, []string{"proj-ec"}, tc.key, false, tc.own, tc.svc)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if s, _ := db.Stats("old-embed"); s.Embedded != 2 {
				t.Fatalf("a refused drop removed vectors: %d left", s.Embedded)
			}
		})
	}
	if err := embedDrop(reg, []string{"proj-ec"}, "old-embed", true, "new-embed", live("new-embed", true, nil)); err != nil {
		t.Fatal(err)
	}
	if s, _ := db.Stats("old-embed"); s.Embedded != 2 {
		t.Fatalf("a dry run removed vectors: %d left", s.Embedded)
	}
	if err := embedDrop(reg, []string{"proj-ec"}, "old-embed", false, "new-embed", live("new-embed", true, nil)); err != nil {
		t.Fatal(err)
	}
	if ks, _ := db.EmbedKeys(); len(ks) != 0 {
		t.Fatalf("keys left after the drop: %+v", ks)
	}
}
