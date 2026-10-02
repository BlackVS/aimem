package mcp

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aimem/internal/adapter"
	"aimem/internal/server"
	"aimem/internal/store"
)

// keptParagraph is an invented, readable text with what a transport could
// damage: runs of spaces in code, a tab, blank lines, an indented block,
// non-ASCII letters and punctuation, and a final newline.
const keptParagraph = "The worker moved the cache key to include the tenant, so two tenants no longer share entries.\n" +
	"\n" +
	"Evidence:  `go test ./internal/cache/ -run TestTenantKey`  passed three times in a row.\n" +
	"\tIndented line kept as written; café, naïve, Zürich, 東京 and an em dash — all stay.\n" +
	"\n" +
	"    key := tenant + \"/\" + id   // two spaces before the comment\n" +
	"Next: the operator deploys after review.\n"

// The text an agent sends through each write tool is stored and read back
// byte for byte. A difference here is a transport or storage defect, never
// a generation one (docs/WRITING-PERSISTED-TEXT.md, task 01a0d996-616b).
func TestKeptTextRoundTripsByteForByte(t *testing.T) {
	// Task tools, through the hub's MCP facade with a user token.
	f := newHub(t)
	text, isErr := toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "create_task", "arguments": map[string]any{
		"project": "alpha", "title": "Readable text survives", "objective": keptParagraph, "idempotency_key": "kept-create"}}))
	if isErr {
		t.Fatalf("create_task: %s", text)
	}
	var task struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &task); err != nil || task.ID == "" {
		t.Fatalf("create_task result: %v %s", err, text)
	}
	if text, isErr := toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "add_task_comment", "arguments": map[string]any{
		"id": task.ID, "body": keptParagraph, "idempotency_key": "kept-comment"}})); isErr {
		t.Fatalf("add_task_comment: %s", text)
	}
	text, _ = toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "get_task", "arguments": map[string]any{"id": task.ID}}))
	var got struct {
		Objective string `json:"objective"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil || got.Objective != keptParagraph {
		t.Fatalf("task objective read back through get_task differs:\n%q\nwant\n%q (%v)", got.Objective, keptParagraph, err)
	}
	text, _ = toolText(f.rpc(t, f.alice, "tools/call", map[string]any{"name": "list_task_comments", "arguments": map[string]any{"id": task.ID}}))
	var page struct {
		Comments []struct {
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal([]byte(text), &page); err != nil || len(page.Comments) != 1 || page.Comments[0].Body != keptParagraph {
		t.Fatalf("comment read back through list_task_comments differs: %v %q", err, text)
	}

	// Memory, document and record writes, through the personal facade to
	// the service's own handler.
	reg, err := store.NewRegistry(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	db, err := reg.Open("alpha")
	if err != nil {
		t.Fatal(err)
	}
	svc := server.New(reg, slog.New(slog.NewTextHandler(new(strings.Builder), nil)))
	t.Cleanup(func() { svc.Close() })
	ts := httptest.NewServer(svc.Handler())
	t.Cleanup(ts.Close)
	// Documents and records go through the hub entry in the state root: an
	// isolated state root names only this test's server, so nothing can
	// reach a real hub.
	state := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", state)
	if err := adapter.SaveHubs(state, map[string]*adapter.HubConfig{"test": {URL: ts.URL, Token: "test-token"}}, "test"); err != nil {
		t.Fatal(err)
	}
	if mcpStateRoot() != state {
		t.Fatal("the state root is not the test's own; refusing to write")
	}
	t.Chdir(t.TempDir()) // no .aimem.json: update_doc touches no local file
	s := &srv{project: "alpha", api: &http.Client{Transport: &rewriteHost{to: strings.TrimPrefix(ts.URL, "http://"), rt: http.DefaultTransport}}}
	call := func(name string, args map[string]any) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
		var resp struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(s.handle(t.Context(), raw), &resp); err != nil {
			t.Fatal(err)
		}
		text, isErr := toolText(resp.Result)
		if isErr {
			t.Fatalf("%s: %s", name, text)
		}
		return text
	}

	call("update_doc", map[string]any{"name": "RUNBOOK", "body": keptParagraph, "base_rev": 0})
	doc, err := db.GetDoc("RUNBOOK", 0)
	if err != nil || doc.Body != keptParagraph {
		t.Fatalf("document stored differently:\n%q\nwant\n%q (%v)", doc.Body, keptParagraph, err)
	}

	body, _ := json.Marshal(map[string]string{"summary": keptParagraph})
	call("put_record", map[string]any{"collection": "notes", "id": "kept", "body": string(body), "base_rev": 0})
	rec, err := db.GetRecord("notes", "kept", 0)
	var fields struct {
		Summary string `json:"summary"`
	}
	if err != nil || json.Unmarshal(rec.Body, &fields) != nil || fields.Summary != keptParagraph {
		t.Fatalf("record field stored differently: %q (%v)", fields.Summary, err)
	}

	// A memory is one sentence; it keeps non-ASCII and inner spacing too.
	sentence := "The cache key in café-service  includes the tenant — checked in Zürich and 東京 on 2026-10-02."
	call("remember", map[string]any{"text": sentence})
	mems, err := db.Memories(true)
	if err != nil || len(mems) != 1 || mems[0].Text != sentence {
		t.Fatalf("memory stored differently: %v %+v", err, mems)
	}
}
