package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
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

// The console's coverage line over the overview the service serves: a
// project whose store cannot be read makes it "coverage unavailable",
// never a total over the projects that could be read. It runs the page's
// own embCoverage under node (CI's lint job requires node).
func TestSetupCoverageIsUnavailableWithAnUnreadableProject(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		if os.Getenv("AIMEM_TEST_REQUIRE_NODE") != "" {
			t.Fatal("node is required here and not on PATH")
		}
		t.Skip("node is not on PATH")
	}
	s, h, _ := modelsRig(t)
	seedKey(t, s.reg, "p1", "old-embed")
	bad := filepath.Join(s.reg.Root(), "projects", "p2")
	os.MkdirAll(bad, 0o700)
	os.WriteFile(filepath.Join(bad, "journal.db"), []byte("not a database, at some length to pass any header read"), 0o600)
	w := req(t, h, "GET", "/v1/overview", "")
	var ov struct {
		Projects []map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &ov); err != nil || len(ov.Projects) != 2 {
		t.Fatalf("overview: %d %s", w.Code, w.Body)
	}
	script, _ := readConsolePage(t, "admin.html")
	i := strings.Index(script, "function embCoverage(")
	if i < 0 {
		t.Fatal("admin.html: function embCoverage( not found")
	}
	j := strings.Index(script[i:], "\n}\n")
	if j < 0 {
		t.Fatal("admin.html: embCoverage has no closing line")
	}
	rows, _ := json.Marshal(ov.Projects)
	js := "const esc=x=>x;\n" + script[i:i+j+2] + "\nconsole.log(embCoverage(\"old-embed\", " + string(rows) + "));\n"
	f := filepath.Join(t.TempDir(), "cov.js")
	os.WriteFile(f, []byte(js), 0o600)
	out, err := exec.Command(node, f).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); !strings.HasPrefix(got, "coverage unavailable: 1 project(s) could not be read (p2)") {
		t.Fatalf("Setup coverage with an unreadable project: %q", got)
	}
}

// The env file's key is resolved with the file's own dimension setting,
// in both directions of a dimension change: a prefilled M@256 that the
// file names is kept although the service queries bare M, and once both
// are on M@256 the retired bare M can go.
func TestEmbeddingsDropResolvesTheFileKeyWithTheFileDimension(t *testing.T) {
	s, h, envFile := modelsRig(t)
	seedKey(t, s.reg, "p1", "old-embed")
	seedKey(t, s.reg, "p1", "old-embed@256")
	os.WriteFile(envFile, []byte("AIMEM_CURATE_MODEL=c\nAIMEM_EMBED_MODEL=old-embed\nAIMEM_EMBED_DIM=256\n"), 0o600)
	w := req(t, h, "POST", "/v1/config/embeddings/drop", `{"key":"old-embed@256"}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "the env file names") || rowsOf(t, s.reg, "p1", "old-embed@256") != 1 {
		t.Fatalf("drop of the file's M@256 while the service queries M: %d %s", w.Code, w.Body)
	}
	// A single-quoted value is the same setting (the CLI's loader and
	// systemd read it so); a value that is not a number is a refusal.
	for _, line := range []string{"AIMEM_EMBED_DIM='256'", "AIMEM_EMBED_DIM=\"256\""} {
		os.WriteFile(envFile, []byte("AIMEM_CURATE_MODEL=c\nAIMEM_EMBED_MODEL=old-embed\n"+line+"\n"), 0o600)
		w := req(t, h, "POST", "/v1/config/embeddings/drop", `{"key":"old-embed@256"}`)
		if w.Code != 409 || !strings.Contains(w.Body.String(), "is the key the env file names") || rowsOf(t, s.reg, "p1", "old-embed@256") != 1 {
			t.Fatalf("%s: drop of the file's key: %d %s", line, w.Code, w.Body)
		}
	}
	os.WriteFile(envFile, []byte("AIMEM_CURATE_MODEL=c\nAIMEM_EMBED_MODEL=old-embed\nAIMEM_EMBED_DIM=wide\n"), 0o600)
	if w := req(t, h, "POST", "/v1/config/embeddings/drop", `{"key":"old-embed@256"}`); w.Code != 409 || !strings.Contains(w.Body.String(), "is not a number") {
		t.Fatalf("an unreadable dimension: %d %s", w.Code, w.Body)
	}
	os.WriteFile(envFile, []byte("AIMEM_CURATE_MODEL=c\nAIMEM_EMBED_MODEL=old-embed\nAIMEM_EMBED_DIM=256\n"), 0o600)
	// The service moves to M@256 too (as a restart under the file would).
	t.Setenv("AIMEM_EMBED_DIM", "256")
	s.setEmbedModel("old-embed")
	if got := s.embedder().Key(); got != "old-embed@256" {
		t.Fatalf("live key %q", got)
	}
	w = req(t, h, "POST", "/v1/config/embeddings/drop", `{"key":"old-embed"}`)
	if w.Code != 200 || rowsOf(t, s.reg, "p1", "old-embed") != 0 || rowsOf(t, s.reg, "p1", "old-embed@256") != 1 {
		t.Fatalf("drop of the retired bare key: %d %s", w.Code, w.Body)
	}
}

// A dry run over a project directory without a database file creates
// nothing; nor does the real drop.
func TestEmbeddingsDropCreatesNoStore(t *testing.T) {
	s, h, _ := modelsRig(t)
	seedKey(t, s.reg, "p1", "gone-embed")
	empty := filepath.Join(s.reg.Root(), "projects", "p0")
	os.MkdirAll(empty, 0o700)
	for _, body := range []string{`{"key":"gone-embed","dry_run":true}`, `{"key":"gone-embed"}`} {
		var a dropAnswer
		w := req(t, h, "POST", "/v1/config/embeddings/drop", body)
		json.Unmarshal(w.Body.Bytes(), &a)
		if w.Code != 200 || !a.Complete {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
		if ents, _ := os.ReadDir(empty); len(ents) != 0 {
			t.Fatalf("%s created %v in a project without a store", body, ents)
		}
	}
	if rowsOf(t, s.reg, "p1", "gone-embed") != 0 {
		t.Fatal("the real drop did not remove p1's vectors")
	}
}

// A drop whose request was cancelled while it waited for the lock
// deletes nothing.
func TestEmbeddingsDropCancelledWhileWaitingDeletesNothing(t *testing.T) {
	s, h, _ := modelsRig(t)
	seedKey(t, s.reg, "p1", "gone-embed")
	s.modelsMu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/v1/config/embeddings/drop", strings.NewReader(`{"key":"gone-embed"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, r); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	s.modelsMu.Unlock()
	<-done
	if w.Code == 200 || !strings.Contains(w.Body.String(), "cancelled before the drop began") || rowsOf(t, s.reg, "p1", "gone-embed") != 1 {
		t.Fatalf("a cancelled drop: %d %s", w.Code, w.Body)
	}
}

// The projects cannot be listed: the save says the coverage is unknown.
func TestModelsPutSaysCoverageUnknownWhenProjectsCannotBeListed(t *testing.T) {
	s, h, _ := modelsRig(t)
	dir := filepath.Join(s.reg.Root(), "projects")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := req(t, h, "PUT", "/v1/config/models", `{"curate_model":"c","embed_model":"new-embed"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "coverage under new-embed is unknown: the projects could not be listed") ||
		strings.Contains(w.Body.String(), "every live fact") {
		t.Fatalf("PUT: %d %s", w.Code, w.Body)
	}
}
