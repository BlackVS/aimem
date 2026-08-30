package adapter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/store"
)

// fakeDocHub implements just enough of the hub's doc API to exercise the
// publisher: an in-memory CAS store behind the real endpoint shapes.
type fakeDocHub struct {
	docs map[string]store.Doc
	puts int
}

func (f *fakeDocHub) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/projects/{p}/docs/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.puts++
		var body struct {
			Body      string `json:"body"`
			BaseRev   int64  `json:"base_rev"`
			UpdatedBy string `json:"updated_by"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		name := r.PathValue("name")
		cur := f.docs[name]
		if body.Body == cur.Body && cur.Rev > 0 {
			json.NewEncoder(w).Encode(map[string]any{"rev": cur.Rev})
			return
		}
		if body.BaseRev != cur.Rev {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]any{
				"error": "stale", "rev": cur.Rev, "updated_by": cur.UpdatedBy, "body": cur.Body})
			return
		}
		f.docs[name] = store.Doc{Name: name, Body: body.Body, Rev: cur.Rev + 1, UpdatedBy: body.UpdatedBy}
		json.NewEncoder(w).Encode(map[string]any{"rev": cur.Rev + 1})
	})
	return mux
}

func docTestSetup(t *testing.T) (*Client, *fakeDocHub, string, string) {
	t.Helper()
	fake := &fakeDocHub{docs: map[string]store.Doc{}}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	root := t.TempDir()
	if err := SaveHubs(root, map[string]*HubConfig{
		"home": {URL: srv.URL, Token: "t"},
	}, "home"); err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	os.WriteFile(filepath.Join(proj, "docs", "SESSION-STATE.md"), []byte("# Handoff v1\n"), 0o644)
	return NewClient(root), fake, root, proj
}

func TestPublishDocsLifecycle(t *testing.T) {
	c, fake, root, proj := docTestSetup(t)

	// First publish: creates rev 1.
	res := c.PublishDocs(proj, "projX", "home", "host/test")
	if len(res) != 1 || res[0].Err != nil || res[0].Rev != 1 {
		t.Fatalf("first publish: %+v", res)
	}
	if fake.docs["SESSION-STATE"].Body != "# Handoff v1\n" {
		t.Fatalf("hub body wrong: %+v", fake.docs["SESSION-STATE"])
	}

	// Unchanged file: zero HTTP calls — the free case must stay free.
	before := fake.puts
	if res := c.PublishDocs(proj, "projX", "home", "host/test"); len(res) != 0 {
		t.Fatalf("unchanged publish produced results: %+v", res)
	}
	if fake.puts != before {
		t.Fatalf("unchanged publish still called the hub (%d -> %d puts)", before, fake.puts)
	}

	// Edit: next publish bumps to rev 2 with the recorded base.
	os.WriteFile(filepath.Join(proj, "docs", "SESSION-STATE.md"), []byte("# Handoff v2\n"), 0o644)
	res = c.PublishDocs(proj, "projX", "home", "host/test")
	if len(res) != 1 || res[0].Err != nil || res[0].Rev != 2 {
		t.Fatalf("second publish: %+v", res)
	}

	// Divergence: hub moves ahead, local edit conflicts, sidecar keeps the
	// stale hash so the NEXT publish retries (no queue needed).
	fake.docs["SESSION-STATE"] = store.Doc{Name: "SESSION-STATE", Body: "hub side", Rev: 9, UpdatedBy: "minis/x"}
	os.WriteFile(filepath.Join(proj, "docs", "SESSION-STATE.md"), []byte("# Handoff v3\n"), 0o644)
	res = c.PublishDocs(proj, "projX", "home", "host/test")
	if len(res) != 1 || res[0].Err == nil {
		t.Fatalf("conflict not reported: %+v", res)
	}
	var conflict *DocConflictError
	if !asDocConflict(res[0].Err, &conflict) || conflict.Doc.Rev != 9 || conflict.Doc.UpdatedBy != "minis/x" {
		t.Fatalf("conflict payload: %v", res[0].Err)
	}
	// Retry happens on the next checkpoint because the hash stayed stale.
	before = fake.puts
	res = c.PublishDocs(proj, "projX", "home", "host/test")
	if fake.puts != before+1 || len(res) != 1 || res[0].Err == nil {
		t.Fatalf("conflicted doc not retried: puts %d->%d, %+v", before, fake.puts, res)
	}

	// No hub configured for the binding: silent no-op.
	os.Remove(filepath.Join(root, "hub.json"))
	if res := c.PublishDocs(proj, "projX", "home", "host/test"); res != nil {
		t.Fatalf("publish without hub: %+v", res)
	}
}

func TestPublishDocsRefusesOversizeLocally(t *testing.T) {
	c, fake, _, proj := docTestSetup(t)
	big := strings.Repeat("x", store.MaxDocBytes+1)
	os.WriteFile(filepath.Join(proj, "docs", "SESSION-STATE.md"), []byte(big), 0o644)
	res := c.PublishDocs(proj, "projX", "home", "host/test")
	if len(res) != 1 || res[0].Err == nil {
		t.Fatalf("oversize not refused: %+v", res)
	}
	if fake.puts != 0 {
		t.Fatal("oversize body was sent to the hub anyway")
	}
}

func asDocConflict(err error, out **DocConflictError) bool {
	c, ok := err.(*DocConflictError)
	if ok {
		*out = c
	}
	return ok
}

func TestPublishDocsRefusesSecretLocally(t *testing.T) {
	c, fake, _, proj := docTestSetup(t)
	// Fixture split so source-scanners never see a token shape.
	body := "key " + strings.Join([]string{"gh", "p_abcdefghijklmnopqrstuvwxyz123456789"}, "")
	os.WriteFile(filepath.Join(proj, "docs", "SESSION-STATE.md"), []byte(body), 0o644)
	res := c.PublishDocs(proj, "projX", "home", "host/test")
	if len(res) != 1 || res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "secret-shaped") {
		t.Fatalf("secret not refused: %+v", res)
	}
	if fake.puts != 0 {
		t.Fatal("secret-bearing body was sent to the hub anyway")
	}
}

func TestPublishDocsRefusesNameCollision(t *testing.T) {
	c, fake, _, proj := docTestSetup(t)
	// docs/SESSION-STATE.md is bound by default; bind a second file whose
	// base name collides. Both would publish as document "SESSION-STATE"
	// and fight each other on every checkpoint.
	os.MkdirAll(filepath.Join(proj, "notes"), 0o755)
	os.WriteFile(filepath.Join(proj, ".aimem.json"),
		[]byte(`{"docs":["notes/SESSION-STATE.txt"]}`), 0o600)
	os.WriteFile(filepath.Join(proj, "notes", "SESSION-STATE.txt"), []byte("other\n"), 0o644)
	res := c.PublishDocs(proj, "projX", "home", "host/test")
	if len(res) != 2 {
		t.Fatalf("expected one publish and one refusal, got %+v", res)
	}
	if res[0].Err != nil || res[0].Rev != 1 {
		t.Fatalf("first binding (default handoff) did not publish: %+v", res[0])
	}
	if res[1].Err == nil || !strings.Contains(res[1].Err.Error(), "both bind") {
		t.Fatalf("collision not refused: %+v", res[1])
	}
	if fake.puts != 1 {
		t.Fatalf("colliding binding reached the hub: %d puts", fake.puts)
	}
	if fake.docs["SESSION-STATE"].Body != "# Handoff v1\n" {
		t.Fatalf("wrong file won the name: %q", fake.docs["SESSION-STATE"].Body)
	}
}

// reconcile fixtures: extend the fake hub with the read surface
// (list, current, historical revision) that ReconcileDocs uses.
func (f *fakeDocHub) readHandlers(mux *http.ServeMux, history map[int64]string) {
	mux.HandleFunc("GET /v1/projects/{p}/docs", func(w http.ResponseWriter, r *http.Request) {
		var list []store.Doc
		for _, d := range f.docs {
			list = append(list, d)
		}
		json.NewEncoder(w).Encode(map[string]any{"docs": list})
	})
	mux.HandleFunc("GET /v1/projects/{p}/docs/{name}", func(w http.ResponseWriter, r *http.Request) {
		d := f.docs[r.PathValue("name")]
		if rev := r.URL.Query().Get("rev"); rev != "" {
			var n int64
			fmt.Sscanf(rev, "%d", &n)
			if body, ok := history[n]; ok {
				d = store.Doc{Name: d.Name, Body: body, Rev: n, UpdatedBy: d.UpdatedBy}
			}
		}
		json.NewEncoder(w).Encode(d)
	})
}

func TestReconcileDocs(t *testing.T) {
	fake := &fakeDocHub{docs: map[string]store.Doc{}}
	mux := http.NewServeMux()
	mux.Handle("/", fake.handler())
	history := map[int64]string{}
	fake.readHandlers(mux, history)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	root := t.TempDir()
	if err := SaveHubs(root, map[string]*HubConfig{"home": {URL: srv.URL, Token: "t"}}, "home"); err != nil {
		t.Fatal(err)
	}
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
	file := filepath.Join(proj, "docs", "SESSION-STATE.md")
	os.WriteFile(file, []byte("a\nb\nc\n"), 0o644)
	c := NewClient(root)
	hub := &HubConfig{URL: srv.URL, Token: "t"}

	// Publish rev 1 (also records the project dir for the timer).
	if res := c.PublishDocs(proj, "projR", "home", "h/t"); len(res) != 1 || res[0].Err != nil {
		t.Fatalf("seed publish: %+v", res)
	}
	if c.DocDir("projR") == "" {
		t.Fatal("publisher did not record the project dir")
	}
	history[1] = "a\nb\nc\n"

	// FAST-FORWARD: console edits rev 2; local unchanged -> file follows.
	fake.docs["SESSION-STATE"] = store.Doc{Name: "SESSION-STATE", Body: "a\nB2\nc\n", Rev: 2, UpdatedBy: "console"}
	c.ReconcileDocs(hub, "projR")
	if got, _ := os.ReadFile(file); string(got) != "a\nB2\nc\n" {
		t.Fatalf("fast-forward did not apply: %q", got)
	}
	if c.DocSyncRev("projR", "SESSION-STATE") != 2 {
		t.Fatalf("sidecar not rebased: %d", c.DocSyncRev("projR", "SESSION-STATE"))
	}
	history[2] = "a\nB2\nc\n"

	// CLEAN MERGE: hub rev 3 changes the tail, local changes the head.
	fake.docs["SESSION-STATE"] = store.Doc{Name: "SESSION-STATE", Body: "a\nB2\nC3\n", Rev: 3, UpdatedBy: "console"}
	os.WriteFile(file, []byte("A-local\nB2\nc\n"), 0o644)
	c.ReconcileDocs(hub, "projR")
	if got, _ := os.ReadFile(file); string(got) != "A-local\nB2\nC3\n" {
		t.Fatalf("clean merge result: %q", got)
	}
	// Sidecar rebased onto rev 3 with the HUB body's hash, so PublishDocs
	// pushes the merged file - the sync loop's next step.
	if c.DocSyncRev("projR", "SESSION-STATE") != 3 {
		t.Fatalf("merge did not rebase sidecar: %d", c.DocSyncRev("projR", "SESSION-STATE"))
	}
	if res := c.PublishDocs(proj, "projR", "home", "h/t"); len(res) != 1 || res[0].Err != nil || res[0].Rev != 4 {
		t.Fatalf("merged push: %+v", res)
	}
	history[4] = "A-local\nB2\nC3\n"

	// CONFLICT: both sides change the same line -> preview file, bound
	// file untouched, sidecar untouched.
	fake.docs["SESSION-STATE"] = store.Doc{Name: "SESSION-STATE", Body: "A-local\nB2\nC-hub\n", Rev: 5, UpdatedBy: "console"}
	os.WriteFile(file, []byte("A-local\nB2\nC-mine\n"), 0o644)
	c.ReconcileDocs(hub, "projR")
	if got, _ := os.ReadFile(file); string(got) != "A-local\nB2\nC-mine\n" {
		t.Fatalf("conflict must not touch the bound file: %q", got)
	}
	prev, err := os.ReadFile(file + ".merge")
	if err != nil || !strings.Contains(string(prev), "<<<<<<<") ||
		!strings.Contains(string(prev), "C-hub") || !strings.Contains(string(prev), "C-mine") {
		t.Fatalf("preview missing or wrong: %q err=%v", prev, err)
	}
	if !strings.Contains(string(prev), "base rev 4") || !strings.Contains(string(prev), "hub rev 5") {
		t.Fatalf("preview header must name base and hub revs: %q", prev)
	}
	if c.DocSyncRev("projR", "SESSION-STATE") != 4 {
		t.Fatalf("conflict must not rebase sidecar: %d", c.DocSyncRev("projR", "SESSION-STATE"))
	}
}
