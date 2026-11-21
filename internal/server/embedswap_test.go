package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"aimem/internal/embed"
	"aimem/internal/store"
)

// fakeEmbeddings answers the OpenAI embeddings call with one vector per
// model, so a query's vector shows which model embedded it. wide makes
// "new-embed" answer in three dimensions instead of two.
func fakeEmbeddings(t *testing.T, wide *atomic.Bool) *httptest.Server {
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
			if wide.Load() {
				vec = []float32{0, 1, 0}
			}
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		out := struct {
			Data []item `json:"data"`
		}{}
		for i := range in.Input {
			out.Data = append(out.Data, item{i, vec})
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(up.Close)
	return up
}

func recallIDs(t *testing.T, h http.Handler, q string) []string {
	t.Helper()
	w := req(t, h, "GET", "/v1/projects/proj-es/memories/recall?q="+q, "")
	if w.Code != 200 {
		t.Fatalf("recall: %d %s", w.Code, w.Body)
	}
	var out struct {
		Memories []struct {
			ID string `json:"id"`
		} `json:"memories"`
	}
	json.Unmarshal(w.Body.Bytes(), &out)
	var ids []string
	for _, m := range out.Memories {
		ids = append(ids, m.ID)
	}
	return ids
}

func liveKey(t *testing.T, h http.Handler) any {
	t.Helper()
	var m struct {
		Embed map[string]string `json:"embed"`
	}
	json.Unmarshal(req(t, h, "GET", "/v1/config/models", "").Body.Bytes(), &m)
	if m.Embed == nil {
		t.Fatal("GET models has no embed state")
	}
	return m.Embed["key"]
}

// After the models PUT, live recall embeds queries with the new model
// at once. Before this, the service went on querying the old key until a
// restart, while the timer already wrote new facts under the new key
// only: those facts were invisible to the semantic leg.
func TestModelsPutSwitchesLiveRecallToTheNewKey(t *testing.T) {
	var wide atomic.Bool
	up := fakeEmbeddings(t, &wide)
	envFile := t.TempDir() + "/env"
	os.WriteFile(envFile, []byte("AIMEM_CURATE_MODEL=c\nAIMEM_EMBED_MODEL=old-embed\n"), 0o600)
	t.Setenv("AIMEM_ENV_FILE", envFile)
	t.Setenv("AIMEM_OPENAI_API_KEY", "k")
	t.Setenv("AIMEM_OPENAI_BASE_URL", up.URL)
	t.Setenv("AIMEM_EMBED_MODEL", "old-embed")
	t.Setenv("AIMEM_EMBED_DIM", "")
	reg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	logs := &lockedBuilder{}
	s := New(reg, slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { s.Close() })
	h := s.Handler()
	db, _ := reg.Open("proj-es")
	// A fact the timer curated after the change: a vector under the new
	// key only. The query shares no word with it, so only the semantic
	// leg can find it.
	id, _, rerr := db.Remember("journals live in sqlite", "curator", store.RememberOpts{})
	if rerr != nil {
		t.Fatal(rerr)
	}
	db.SetEmbedding(id, "new-embed", 2, embed.Encode([]float32{0, 1}))
	// An older fact, embedded under the old key only, not yet backfilled.
	older, _, _ := db.Remember("the console is served at admin", "curator", store.RememberOpts{})
	db.SetEmbedding(older, "old-embed", 2, embed.Encode([]float32{1, 0}))
	if got := liveKey(t, h); got != "old-embed" {
		t.Fatalf("live key before the PUT: %v", got)
	}
	if ids := recallIDs(t, h, "zzqx"); slices.Contains(ids, id) {
		t.Fatalf("under the old key the new fact is unreachable, got %v", ids)
	}

	w := req(t, h, "PUT", "/v1/config/models", `{"curate_model":"c","embed_model":"new-embed"}`)
	var put struct {
		Embed map[string]string `json:"embed"`
		Note  string            `json:"note"`
	}
	json.Unmarshal(w.Body.Bytes(), &put)
	if w.Code != 200 || put.Embed["key"] != "new-embed" || !strings.Contains(put.Note, "facts in 1 project(s) have no vector under new-embed yet") ||
		!strings.Contains(put.Note, "restart aimem.service to apply the text check model") {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	if got := liveKey(t, h); got != "new-embed" {
		t.Fatalf("live key after the PUT: %v", got)
	}
	if ids := recallIDs(t, h, "zzqx"); len(ids) != 1 || ids[0] != id {
		t.Fatalf("recall after the PUT: %v, want [%s]", ids, id)
	}
	var ovr struct {
		Projects []struct {
			Stats struct {
				Embedded   int    `json:"embedded"`
				EmbedModel string `json:"embed_model"`
			} `json:"stats"`
		} `json:"projects"`
	}
	json.Unmarshal(req(t, h, "GET", "/v1/overview", "").Body.Bytes(), &ovr)
	ov := ovr.Projects
	if len(ov) == 0 || ov[0].Stats.EmbedModel != "new-embed" || ov[0].Stats.Embedded != 1 {
		t.Fatalf("overview stats after the PUT: %+v", ov)
	}

	// Another model under the same name answers in another dimension:
	// its query vector cannot score against the stored ones, so recall
	// ranks by keywords alone and still answers.
	wide.Store(true)
	if ids := recallIDs(t, h, "sqlite"); len(ids) != 1 || ids[0] != id {
		t.Fatalf("keyword recall with a wider query vector: %v", ids)
	}
	if l := logs.String(); !strings.Contains(l, "another dimension than stored vectors of its key") || !strings.Contains(l, "stored=2 query=3") {
		t.Fatalf("the dimension mismatch was not logged:\n%s", l)
	}

	// A model the service cannot serve turns semantic recall off, and
	// the answer says so.
	t.Setenv("AIMEM_OPENAI_API_KEY", "")
	w = req(t, h, "PUT", "/v1/config/models", `{"curate_model":"c","embed_model":"lost-embed"}`)
	json.Unmarshal(w.Body.Bytes(), &put)
	if w.Code != 200 || put.Embed["key"] != "" || !strings.Contains(put.Embed["why"], "AIMEM_OPENAI_API_KEY is unset") ||
		!strings.Contains(put.Note, "semantic recall is OFF from now on") {
		t.Fatalf("PUT of an unservable model: %d %s", w.Code, w.Body)
	}
	if got := liveKey(t, h); got != "" {
		t.Fatalf("live key with semantic recall off: %v", got)
	}
}

// lockedBuilder is a log sink the handlers may write from their own
// goroutines while the test reads it.
type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// modelsRig is a service with an env file and an embeddings endpoint;
// AIMEM_EMBED_MODEL names old-embed.
func modelsRig(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	var wide atomic.Bool
	up := fakeEmbeddings(t, &wide)
	envFile := t.TempDir() + "/env"
	os.WriteFile(envFile, []byte("AIMEM_CURATE_MODEL=c\nAIMEM_EMBED_MODEL=old-embed\n"), 0o600)
	t.Setenv("AIMEM_ENV_FILE", envFile)
	t.Setenv("AIMEM_OPENAI_API_KEY", "k")
	t.Setenv("AIMEM_OPENAI_BASE_URL", up.URL)
	t.Setenv("AIMEM_EMBED_MODEL", "old-embed")
	t.Setenv("AIMEM_EMBED_DIM", "")
	s, _ := testServer(t)
	return s, s.Handler(), envFile
}

// Saves in flight at once end with the env file and live recall on the
// same model: the file rewrite and the swap are one step.
func TestOverlappingModelSavesAgree(t *testing.T) {
	_, h, envFile := modelsRig(t)
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m := []string{"old-embed", "new-embed"}[i%2]
			r := httptest.NewRequest("PUT", "/v1/config/models", strings.NewReader(`{"curate_model":"c","embed_model":"`+m+`"}`))
			h.ServeHTTP(httptest.NewRecorder(), r)
		}()
	}
	wg.Wait()
	raw, _ := os.ReadFile(envFile)
	live := liveKey(t, h)
	if !strings.Contains(string(raw), `AIMEM_EMBED_MODEL="`+live.(string)+`"`) {
		t.Fatalf("live recall queries %v, the env file says:\n%s", live, raw)
	}
}

// A project the save cannot check makes the coverage unknown, never
// "every live fact already has a vector".
func TestModelsPutSaysCoverageUnknownWhenAProjectCannotBeChecked(t *testing.T) {
	s, h, _ := modelsRig(t)
	db, _ := s.reg.Open("proj-ok")
	id, _, _ := db.Remember("a fact", "test", store.RememberOpts{})
	db.SetEmbedding(id, "new-embed", 2, embed.Encode([]float32{0, 1}))
	// A project whose database file is not a database: Open fails.
	bad := filepath.Join(s.reg.Root(), "projects", "proj-bad")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "journal.db"), []byte("not a database, at some length to pass any header read"), 0o600); err != nil {
		t.Fatal(err)
	}
	ids, _ := s.reg.Projects()
	if !slices.Contains(ids, "proj-bad") {
		t.Skipf("the rig's unreadable project is not listed (%v); the layout changed", ids)
	}
	w := req(t, h, "PUT", "/v1/config/models", `{"curate_model":"c","embed_model":"new-embed"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "coverage under new-embed is unknown: 1 project(s) could not be checked") ||
		strings.Contains(w.Body.String(), "every live fact") {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
}

// An empty embed model switches embeddings off: the file gets the empty
// line, live recall turns BM25-only, the answer says so.
func TestModelsPutWithAnEmptyEmbedModelSwitchesEmbeddingsOff(t *testing.T) {
	_, h, envFile := modelsRig(t)
	w := req(t, h, "PUT", "/v1/config/models", `{"curate_model":"c","embed_model":""}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "semantic recall is OFF from now on") {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
	raw, _ := os.ReadFile(envFile)
	if !strings.Contains(string(raw), `AIMEM_EMBED_MODEL=""`) || liveKey(t, h) != "" {
		t.Fatalf("after an empty save: live %v, file:\n%s", liveKey(t, h), raw)
	}
	if w := req(t, h, "PUT", "/v1/config/models", `{"curate_model":"","embed_model":"x"}`); w.Code != 400 {
		t.Fatalf("an empty curate model was accepted: %d", w.Code)
	}
}
