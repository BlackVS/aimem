package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"aimem/internal/adapter"
)

// An MCP server spawned in a linked worktree or a subdirectory serves the
// project whose .aimem.json governs it (project = cwd, resolved), and a
// bound doc it updates is rewritten where the project's bound files live.
func TestMCPProjectFromWorktreeAndSubdirectory(t *testing.T) {
	var gotProject string
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/projects/{p}/docs/{name}", func(w http.ResponseWriter, r *http.Request) {
		gotProject = r.PathValue("p")
		json.NewEncoder(w).Encode(map[string]any{"rev": 1})
	})
	hub := httptest.NewServer(mux)
	defer hub.Close()
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{
		"default": {URL: "http://default.invalid", Token: "t"},
		"named":   {URL: hub.URL, Token: "t"},
	}, "default"); err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	main := filepath.Join(base, "main")
	wt := filepath.Join(base, "wt")
	gitDir := filepath.Join(main, ".git", "worktrees", "wt")
	for _, d := range []string{filepath.Join(main, "docs"), filepath.Join(main, "internal"), gitDir, wt} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		filepath.Join(main, ".aimem.json"):        `{"project":"pinned","hub":"named","groups":["fw"],"docs":["docs/RUNBOOK.md"]}`,
		filepath.Join(main, "docs", "RUNBOOK.md"): "old\n",
		filepath.Join(wt, ".git"):                 "gitdir: " + filepath.ToSlash(gitDir) + "\n",
		filepath.Join(gitDir, "commondir"):        "../..\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for i, dir := range []string{filepath.Join(main, "internal"), wt} {
		t.Chdir(dir)
		id, groups, err := DefaultProject()
		if err != nil || id != "pinned" || len(groups) != 1 || groups[0] != "group-fw" {
			t.Fatalf("DefaultProject in %s: %q %v %v", dir, id, groups, err)
		}
		s := &srv{project: id, groups: groups}
		var p toolParams
		p.Name = "update_doc"
		p.Arguments.Name = "RUNBOOK"
		p.Arguments.Body = "body " + string(rune('a'+i)) + "\n"
		p.Arguments.BaseRev = 0
		if _, err := s.docTool(&p); err != nil {
			t.Fatalf("update_doc in %s: %v", dir, err)
		}
		if gotProject != "pinned" {
			t.Errorf("update_doc in %s went to project %q", dir, gotProject)
		}
		if got, _ := os.ReadFile(filepath.Join(main, "docs", "RUNBOOK.md")); string(got) != p.Arguments.Body {
			t.Errorf("bound file in the main checkout not rewritten from %s: %q", dir, got)
		}
		if _, err := os.Stat(filepath.Join(dir, "docs", "RUNBOOK.md")); !os.IsNotExist(err) {
			t.Errorf("update_doc in %s conjured a local copy", dir)
		}
	}
}
