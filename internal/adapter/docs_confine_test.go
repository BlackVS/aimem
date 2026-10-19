package adapter

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"aimem/internal/confine"
	"aimem/internal/confine/linktest"
	"aimem/internal/store"
)

// A bound document reached through a link is neither published nor
// overwritten by the checkpoint publisher and pull: the file the link
// points at stays as it was, and the hub receives nothing.
func TestDocSyncRefusesBoundFilesBehindALink(t *testing.T) {
	cases := []struct {
		name string
		link func(t *testing.T, proj, outside string) string // returns the outside file that must not change
	}{
		{"linked docs directory", func(t *testing.T, proj, outside string) string {
			// docs/SESSION-STATE.md is bound by default whenever it exists,
			// and here it exists only through the link.
			os.WriteFile(filepath.Join(outside, "SESSION-STATE.md"), []byte("outside\n"), 0o600)
			linktest.Dir(t, filepath.Join(proj, "docs"), outside)
			return filepath.Join(outside, "SESSION-STATE.md")
		}},
		{"linked bound file", func(t *testing.T, proj, outside string) string {
			os.WriteFile(filepath.Join(outside, "hub.json"), []byte("outside\n"), 0o600)
			os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
			os.WriteFile(filepath.Join(proj, ".aimem.json"), []byte(`{"docs":["docs/RUNBOOK.md"]}`), 0o600)
			linktest.File(t, filepath.Join(proj, "docs", "RUNBOOK.md"), filepath.Join(outside, "hub.json"))
			return filepath.Join(outside, "hub.json")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDocHub{docs: map[string]store.Doc{}}
			mux := http.NewServeMux()
			mux.Handle("/", fake.handler())
			fake.readHandlers(mux, map[int64]string{})
			srv := httptest.NewServer(mux)
			defer srv.Close()
			root := t.TempDir()
			if err := SaveHubs(root, map[string]*HubConfig{"home": {URL: srv.URL, Token: "t"}}, "home"); err != nil {
				t.Fatal(err)
			}
			base := t.TempDir()
			proj := filepath.Join(base, "checkout")
			outside := filepath.Join(base, "outside")
			os.MkdirAll(proj, 0o755)
			os.MkdirAll(outside, 0o755)
			victim := tc.link(t, proj, outside)
			c := NewClient(root)

			// Publisher: the read is refused and reported, nothing is PUT.
			res := c.PublishDocs(proj, "projL", "home", "h/t")
			if len(res) != 1 || !errors.Is(res[0].Err, confine.ErrLink) {
				t.Fatalf("publish through a link: %+v", res)
			}
			if fake.puts != 0 {
				t.Fatalf("the hub received %d PUT(s) of a file behind a link", fake.puts)
			}

			// Pull: a newer hub revision does not overwrite the target.
			name := "SESSION-STATE"
			if tc.name == "linked bound file" {
				name = "RUNBOOK"
			}
			fake.docs[name] = store.Doc{Name: name, Body: "from the hub\n", Rev: 2, UpdatedBy: "someone"}
			c.ReconcileDocsIn(&HubConfig{URL: srv.URL, Token: "t"}, "projL", proj)
			if b, _ := os.ReadFile(victim); string(b) != "outside\n" {
				t.Fatalf("the file behind the link was rewritten: %q", b)
			}
			es, _ := os.ReadDir(outside)
			if len(es) != 1 {
				t.Fatalf("files appeared outside the checkout: %v", es)
			}
		})
	}
}
