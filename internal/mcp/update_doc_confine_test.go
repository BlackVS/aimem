package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/confine/linktest"
)

// update_doc's rewrite of a bound file never follows a link below the
// checkout: the hub write stands, the file the link points at is left as
// it was, and the reply says the local file was not rewritten.
func TestUpdateDocDoesNotWriteThroughALink(t *testing.T) {
	cases := []struct {
		name string
		link func(t *testing.T, proj, outside string) string
	}{
		{"linked docs directory", func(t *testing.T, proj, outside string) string {
			linktest.Dir(t, filepath.Join(proj, "docs"), outside)
			return filepath.Join(outside, "RUNBOOK.md")
		}},
		{"linked bound file", func(t *testing.T, proj, outside string) string {
			os.MkdirAll(filepath.Join(proj, "docs"), 0o755)
			linktest.File(t, filepath.Join(proj, "docs", "RUNBOOK.md"), filepath.Join(outside, "RUNBOOK.md"))
			return filepath.Join(outside, "RUNBOOK.md")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("PUT /v1/projects/{p}/docs/{name}", func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"rev": 4})
			})
			hub := httptest.NewServer(mux)
			defer hub.Close()
			root := t.TempDir()
			t.Setenv("AIMEM_STATE_DIR", root)
			if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"home": {URL: hub.URL, Token: "t"}}, "home"); err != nil {
				t.Fatal(err)
			}
			base := t.TempDir()
			proj := filepath.Join(base, "checkout")
			outside := filepath.Join(base, "outside")
			os.MkdirAll(proj, 0o755)
			os.MkdirAll(outside, 0o755)
			os.WriteFile(filepath.Join(outside, "RUNBOOK.md"), []byte("outside\n"), 0o644)
			os.WriteFile(filepath.Join(proj, ".aimem.json"), []byte(`{"docs":["docs/RUNBOOK.md"]}`), 0o600)
			victim := tc.link(t, proj, outside)
			t.Chdir(proj)

			s := &srv{project: "projX"}
			var p toolParams
			p.Name = "update_doc"
			p.Arguments.Name = "RUNBOOK"
			p.Arguments.Body = "new body\n"
			p.Arguments.BaseRev = 3
			msg, err := s.docTool(&p)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(msg, "NOT rewritten") || !strings.Contains(msg, "symbolic link or junction") {
				t.Fatalf("the reply does not report the refused rewrite: %q", msg)
			}
			if b, _ := os.ReadFile(victim); string(b) != "outside\n" {
				t.Fatalf("the file behind the link was rewritten: %q", b)
			}
			if es, _ := os.ReadDir(outside); len(es) != 1 {
				t.Fatalf("files appeared outside the checkout: %v", es)
			}
			if rev := adapter.NewClient(root).DocSyncRev("projX", "RUNBOOK"); rev != 0 {
				t.Fatalf("the sidecar recorded rev %d for a file that was not written", rev)
			}
		})
	}
}
