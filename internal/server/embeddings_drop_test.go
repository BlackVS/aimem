package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"aimem/internal/embed"
	"aimem/internal/store"
)

// The drop is an admin route like the models PUT: an ordinary token and
// a team conversation are refused, the full hub token is admitted.
func TestEmbeddingsDropIsAdminOnly(t *testing.T) {
	t.Setenv("AIMEM_ENV_FILE", filepath.Join(t.TempDir(), "env"))
	g := newTeamRig(t)
	for _, rt := range g.s.Routes() {
		if rt.Pattern == "/v1/config/embeddings/drop" && !rt.Admin {
			t.Fatal("the drop route is not marked admin")
		}
	}
	if ordinaryRoutes["POST /v1/config/embeddings/drop"] || slices.Contains(teamRoutes, "POST /v1/config/embeddings/drop") {
		t.Fatal("the drop route is reachable by an ordinary token or a team conversation")
	}
	body := `{"key":"old-embed","dry_run":true}`
	if r := g.call(t, g.tls, "POST", "/v1/config/embeddings/drop", g.alice, nil, body, true); r.status != 403 {
		t.Errorf("an ordinary token: %d %s", r.status, r.body)
	}
	h := validHandle(t)
	g.secrets = append(g.secrets, h)
	if r := g.call(t, g.tls, "POST", "/v1/config/embeddings/drop", g.alice, teamHeader(h), body, true); r.status != 403 {
		t.Errorf("a team conversation: %d %s", r.status, r.body)
	}
	if r := g.call(t, g.tls, "POST", "/v1/config/embeddings/drop", g.env, nil, body, true); r.status != 200 {
		t.Errorf("the hub token: %d %s", r.status, r.body)
	}
}

type dropAnswer struct {
	Complete  bool         `json:"complete"`
	StoppedAt string       `json:"stopped_at"`
	Projects  []dropResult `json:"projects"`
	Error     string       `json:"error"`
}

func seedKey(t *testing.T, reg *store.Registry, proj, key string) {
	t.Helper()
	db, err := reg.Open(proj)
	if err != nil {
		t.Fatal(err)
	}
	id, _, _ := db.Remember("a fact in "+proj, "test", store.RememberOpts{})
	if err := db.SetEmbedding(id, key, 2, embed.Encode([]float32{1, 0})); err != nil {
		t.Fatal(err)
	}
}

func rowsOf(t *testing.T, reg *store.Registry, proj, key string) int {
	t.Helper()
	db, _ := reg.Open(proj)
	ks, _ := db.EmbedKeys()
	n := 0
	for _, k := range ks {
		if k.Model == key {
			n += k.Rows
		}
	}
	return n
}

// The drop refuses the live key and the key the env file names now,
// counts without writing on a dry run, and removes otherwise.
func TestEmbeddingsDropRefusesKeysInUseAndDryRunWritesNothing(t *testing.T) {
	s, h, envFile := modelsRig(t)
	for _, p := range []string{"p1", "p2"} {
		seedKey(t, s.reg, p, "gone-embed")
		seedKey(t, s.reg, p, "old-embed")
	}
	// The file names file-embed while the service still queries old-embed.
	os.WriteFile(envFile, []byte("AIMEM_CURATE_MODEL=c\nAIMEM_EMBED_MODEL=file-embed\n"), 0o600)
	for _, key := range []string{"old-embed", "file-embed"} {
		if w := req(t, h, "POST", "/v1/config/embeddings/drop", `{"key":"`+key+`"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "refused") {
			t.Errorf("drop %s: %d %s", key, w.Code, w.Body)
		}
	}
	var a dropAnswer
	w := req(t, h, "POST", "/v1/config/embeddings/drop", `{"key":"gone-embed","dry_run":true}`)
	json.Unmarshal(w.Body.Bytes(), &a)
	if w.Code != 200 || !a.Complete || len(a.Projects) != 2 || a.Projects[0].Rows != 1 || rowsOf(t, s.reg, "p1", "gone-embed") != 1 {
		t.Fatalf("dry run: %d %s", w.Code, w.Body)
	}
	w = req(t, h, "POST", "/v1/config/embeddings/drop", `{"key":"gone-embed"}`)
	json.Unmarshal(w.Body.Bytes(), &a)
	if w.Code != 200 || !a.Complete || rowsOf(t, s.reg, "p1", "gone-embed") != 0 || rowsOf(t, s.reg, "p2", "gone-embed") != 0 ||
		rowsOf(t, s.reg, "p1", "old-embed") != 1 {
		t.Fatalf("drop: %d %s", w.Code, w.Body)
	}
}

// A project that fails stops the drop there: the answer names it, lists
// what was removed before it, and is not complete.
func TestEmbeddingsDropReportsWhereItStopped(t *testing.T) {
	s, h, _ := modelsRig(t)
	seedKey(t, s.reg, "p1", "gone-embed")
	seedKey(t, s.reg, "p3", "gone-embed")
	bad := filepath.Join(s.reg.Root(), "projects", "p2")
	os.MkdirAll(bad, 0o700)
	os.WriteFile(filepath.Join(bad, "journal.db"), []byte("not a database, at some length to pass any header read"), 0o600)
	var a dropAnswer
	w := req(t, h, "POST", "/v1/config/embeddings/drop", `{"key":"gone-embed"}`)
	json.Unmarshal(w.Body.Bytes(), &a)
	if w.Code != 200 || a.Complete || a.StoppedAt != "p2" || len(a.Projects) != 2 ||
		a.Projects[0].Project != "p1" || a.Projects[0].Rows != 1 || a.Projects[1].Error == "" {
		t.Fatalf("partial drop: %d %s", w.Code, w.Body)
	}
	if rowsOf(t, s.reg, "p1", "gone-embed") != 0 || rowsOf(t, s.reg, "p3", "gone-embed") != 1 {
		t.Fatal("the drop went past the project that failed, or did not remove the one before it")
	}
}

// A save waits for a drop in progress: it cannot make the key live
// while its vectors go.
func TestModelsSaveWaitsForADrop(t *testing.T) {
	s, h, _ := modelsRig(t)
	s.modelsMu.Lock() // a drop holding the lock
	done := make(chan int)
	go func() {
		done <- req(t, h, "PUT", "/v1/config/models", `{"curate_model":"c","embed_model":"new-embed"}`).Code
	}()
	select {
	case c := <-done:
		t.Fatalf("the save finished (%d) while the drop held the lock", c)
	case <-time.After(200 * time.Millisecond):
	}
	if liveKey(t, h) != "old-embed" {
		t.Fatal("the live key changed during the drop")
	}
	s.modelsMu.Unlock()
	if c := <-done; c != 200 {
		t.Fatalf("the save after the drop: %d", c)
	}
}

// An env file that exists but cannot be read is a refusal: the key the
// timer will embed with is unknown.
func TestEmbeddingsDropRefusesWhenTheEnvFileCannotBeRead(t *testing.T) {
	s, h, _ := modelsRig(t)
	seedKey(t, s.reg, "p1", "gone-embed")
	dir := filepath.Join(t.TempDir(), "env-is-a-directory")
	os.MkdirAll(dir, 0o700)
	t.Setenv("AIMEM_ENV_FILE", dir)
	if w := req(t, h, "POST", "/v1/config/embeddings/drop", `{"key":"gone-embed"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "env file could not be read") {
		t.Fatalf("drop with an unreadable env file: %d %s", w.Code, w.Body)
	}
	if rowsOf(t, s.reg, "p1", "gone-embed") != 1 {
		t.Fatal("vectors went although the env file could not be read")
	}
}
