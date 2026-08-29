package adapter

import (
	"encoding/json"
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
