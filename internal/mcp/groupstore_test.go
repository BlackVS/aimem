package mcp

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/server"
	"aimem/internal/store"
)

// groupStoreSrv runs the real local service over a fresh state root that
// holds the project's and the user's stores and no store of the declared
// group "group-g1", the state of a machine that has never synced it.
func groupStoreSrv(t *testing.T) (*srv, string) {
	t.Helper()
	root := t.TempDir()
	reg, err := store.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	for id, text := range map[string]string{"projA": "the project's widget fact", store.UserScopeProject: "the user's widget fact"} {
		db, err := reg.Open(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.ImportMemory(&store.Memory{
			ID: id + "-m1", Text: text, Kind: "fact", Confidence: 0.7,
			CreatedAt: "2026-11-16T00:00:00Z", Actor: "test",
		}); err != nil {
			t.Fatal(err)
		}
	}
	svc := httptest.NewServer(server.New(reg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))).Handler())
	t.Cleanup(svc.Close)
	s := &srv{api: svc.Client(), project: "projA", groups: []string{"group-g1", "group-g2"}}
	s.api.Transport = &rewriteHost{to: svc.URL[len("http://"):], rt: http.DefaultTransport}
	return s, root
}

func runTool(s *srv, name, scope string) (string, error) {
	var p toolParams
	p.Name = name
	p.Arguments.Query = "widget"
	p.Arguments.Scope = scope
	return s.run(&p)
}

// A declared group without a local store reads as empty in every stdio
// tool that reads it, and the read creates no store.
func TestDeclaredGroupWithoutLocalStoreReadsEmpty(t *testing.T) {
	s, root := groupStoreSrv(t)

	// The explicit scope says why it is empty and how the group arrives,
	// since a mistyped group name answers the same way.
	out, err := runTool(s, "recall_memory", "group:g1")
	if err != nil || !strings.HasPrefix(out, `no memories match (this machine holds no store of group "g1" yet`) ||
		!strings.Contains(out, "aimem sync --hub <name>") {
		t.Fatalf("recall group:g1 = %q, %v; want no memories match with the note", out, err)
	}
	out, err = runTool(s, "recall_memory", "both")
	if err != nil {
		t.Fatalf("recall both: %v", err)
	}
	if !strings.Contains(out, "projA-m1") || !strings.Contains(out, "user-m1") {
		t.Fatalf("recall both lost the project's or the user's hit:\n%s", out)
	}
	if strings.Contains(out, "holds no store") {
		t.Fatalf("recall both carries the note of the explicit scope:\n%s", out)
	}
	// "both" with nothing matching anywhere stays the plain answer.
	var q toolParams
	q.Name, q.Arguments.Query, q.Arguments.Scope = "recall_memory", "nothingmatchesthis", "both"
	if out, err := s.run(&q); err != nil || out != "no memories match" {
		t.Fatalf("recall both without hits = %q, %v; want the plain answer", out, err)
	}
	// A group whose store exists and holds no match gets the plain answer too.
	var r toolParams
	r.Name, r.Arguments.Text, r.Arguments.Scope = "remember", "a gadget fact of g2", "group:g2"
	if _, err := s.run(&r); err != nil {
		t.Fatal(err)
	}
	q.Arguments.Scope = "group:g2"
	if out, err := s.run(&q); err != nil || out != "no memories match" {
		t.Fatalf("recall of an existing group store without hits = %q, %v; want the plain answer", out, err)
	}
	out, err = runTool(s, "review_memories", "group:g1")
	if err != nil || !strings.HasPrefix(out, "review queue is clear") {
		t.Fatalf("review group:g1 = %q, %v; want a clear queue", out, err)
	}
	var p toolParams
	p.Name = "get_design_doc"
	p.Arguments.Group = "g1"
	out, err = s.run(&p)
	if err != nil || !strings.HasPrefix(out, "no design document stored for group-g1") {
		t.Fatalf("get_design_doc g1 = %q, %v; want no design document", out, err)
	}

	// An undeclared group is still refused.
	if _, err := runTool(s, "recall_memory", "group:other"); err == nil || !strings.Contains(err.Error(), "has not declared group") {
		t.Fatalf("recall of an undeclared group: %v; want the refusal", err)
	}
	// No read created the group's store.
	if _, err := os.Stat(filepath.Join(root, "projects", "group-g1")); !os.IsNotExist(err) {
		t.Fatalf("a read created the group store: %v", err)
	}
}

// A group store that exists but cannot be opened is a storage fault, not
// an absent store: every read still fails.
func TestUnopenableGroupStoreStillFails(t *testing.T) {
	s, root := groupStoreSrv(t)
	// A file where the store's directory belongs: the stat succeeds and
	// the open fails, so the service answers without the absence code.
	if err := os.WriteFile(filepath.Join(root, "projects", "group-g2"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tool, scope string }{
		{"recall_memory", "group:g2"},
		{"recall_memory", "both"},
		{"review_memories", "group:g2"},
	} {
		if out, err := runTool(s, c.tool, c.scope); err == nil {
			t.Errorf("%s %s on an unopenable store = %q; want an error", c.tool, c.scope, out)
		}
	}
	var p toolParams
	p.Name = "get_design_doc"
	p.Arguments.Group = "g2"
	if out, err := s.run(&p); err == nil {
		t.Errorf("get_design_doc g2 on an unopenable store = %q; want an error", out)
	}
}

// The absence code is set by the service and nothing else: an error that
// carries the same words without the code stays an error.
func TestNoGroupStoreNeedsTheCode(t *testing.T) {
	s := &srv{project: "projA", groups: []string{"group-g1"}}
	plain := &apiError{msg: `no such project: "group-g1"`}
	coded := &apiError{msg: "anything", code: store.CodeNoSuchProject}
	if s.noGroupStore("group-g1", plain) {
		t.Error("the message alone counted as an absent store")
	}
	if !s.noGroupStore("group-g1", coded) {
		t.Error("the code did not count as an absent store")
	}
	if s.noGroupStore("projA", coded) {
		t.Error("a missing project store counted as empty; only a group's may")
	}
	if s.noGroupStore("group-other", coded) {
		t.Error("an undeclared group counted as empty")
	}
	// Hub mode declares nothing: a group the hub lacks stays an error.
	if (&srv{}).noGroupStore("group-g1", coded) {
		t.Error("hub mode read a missing group as empty")
	}
}
