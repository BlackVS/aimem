package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/confine"
	"aimem/internal/confine/linktest"
)

// linkedCheckout makes a checkout whose docs/ is a link to a sibling
// directory holding RUNBOOK.md and SESSION-STATE.md, binds RUNBOOK and a
// collection rendered under docs/, points the CLI at a fake hub that
// counts writes, and changes into the checkout.
func linkedCheckout(t *testing.T) (outside string, puts *int) {
	t.Helper()
	puts = new(int)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			*puts++
		}
		if strings.Contains(r.URL.Path, "/collections/") {
			json.NewEncoder(w).Encode(map[string]any{"records": []map[string]any{
				{"id": "a/one", "body": map[string]any{"title": "one"}, "rev": 1},
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"name": "RUNBOOK", "body": "hub version\n", "rev": 5,
			"updated_at": "2026-10-19T00:00:00Z", "updated_by": "someone",
		})
	}))
	t.Cleanup(hub.Close)
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"home": {URL: hub.URL, Token: "t"}}, "home"); err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	proj := filepath.Join(base, "checkout")
	outside = filepath.Join(base, "outside")
	os.MkdirAll(proj, 0o755)
	os.MkdirAll(outside, 0o755)
	for _, n := range []string{"RUNBOOK.md", "SESSION-STATE.md"} {
		os.WriteFile(filepath.Join(outside, n), []byte("outside\n"), 0o644)
	}
	os.WriteFile(filepath.Join(proj, ".aimem.json"), []byte(`{"docs":["docs/RUNBOOK.md"],"collections":[
		{"name":"notes","render":"docs/NOTES.md"},{"name":"tree","render":"docs/tree"}]}`), 0o600)
	linktest.Dir(t, filepath.Join(proj, "docs"), outside)
	t.Chdir(proj)
	return outside, puts
}

func assertOutsideUntouched(t *testing.T, outside string) {
	t.Helper()
	es, _ := os.ReadDir(outside)
	if len(es) != 2 {
		t.Fatalf("files appeared outside the checkout: %v", es)
	}
	for _, e := range es {
		if b, _ := os.ReadFile(filepath.Join(outside, e.Name())); string(b) != "outside\n" {
			t.Fatalf("%s behind the link was rewritten: %q", e.Name(), b)
		}
	}
}

// The docs commands neither read nor write a bound file through a link
// below the checkout.
func TestDocsCommandsRefuseALinkedBoundFile(t *testing.T) {
	outside, puts := linkedCheckout(t)
	for _, args := range [][]string{
		{"pull", "RUNBOOK", "--force"},
		{"push", "RUNBOOK"},
		{"merge", "RUNBOOK"},
		{"diff", "RUNBOOK"},
	} {
		if err := docsCmd(args); !errors.Is(err, confine.ErrLink) {
			t.Errorf("docs %s through a link: %v", strings.Join(args, " "), err)
		}
	}
	if *puts != 0 {
		t.Fatalf("the hub received %d write(s) of a file behind a link", *puts)
	}
	assertOutsideUntouched(t, outside)
}

// A collection rendered to the path its binding names is not written
// through a link, in file mode or directory mode.
func TestColRenderRefusesALinkedBindingTarget(t *testing.T) {
	outside, _ := linkedCheckout(t)
	for _, name := range []string{"notes", "tree"} {
		if err := colCmd([]string{"render", name}); !errors.Is(err, confine.ErrLink) {
			t.Errorf("col render %s through a link: %v", name, err)
		}
	}
	assertOutsideUntouched(t, outside)
}

// handoff compact does not rewrite the handoff, or write its archive,
// through a link below the checkout.
func TestHandoffCompactRefusesALinkedHandoff(t *testing.T) {
	outside, _ := linkedCheckout(t)
	if err := handoffCmd([]string{"compact"}); !errors.Is(err, confine.ErrLink) {
		t.Fatalf("handoff compact through a link: %v", err)
	}
	assertOutsideUntouched(t, outside)
}
