package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/adapter"
	"aimem/internal/introspect/introspecttest"
	"aimem/internal/privatefile"
	"aimem/internal/teamsession"
)

// aicrewSessions scripts the fake aicrew per handle: which session it names,
// under which of alice's tokens and generation, and whether it has ended.
type aicrewSessions struct {
	mu      sync.Mutex
	g       *teamSessionRig
	entries map[string]aicrewEntry
	holds   map[string]*aicrewHold
}

// aicrewHold pauses the fake's answer for one handle: entered closes when a
// request for it arrives, and the answer is written, from the entry as it
// is then, once release closes.
type aicrewHold struct{ entered, release chan struct{} }

// hold pauses the next answer for handle.
func (a *aicrewSessions) hold(handle string) *aicrewHold {
	h := &aicrewHold{entered: make(chan struct{}), release: make(chan struct{})}
	a.mu.Lock()
	a.holds[handle] = h
	a.mu.Unlock()
	return h
}

type aicrewEntry struct {
	session, token, generation string
	ended, expired             bool
}

func scriptAicrew(g *teamSessionRig) *aicrewSessions {
	a := &aicrewSessions{g: g, entries: map[string]aicrewEntry{}, holds: map[string]*aicrewHold{}}
	g.fake.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		a.mu.Lock()
		h := a.holds[got.Handle]
		delete(a.holds, got.Handle)
		a.mu.Unlock()
		if h != nil {
			close(h.entered)
			<-h.release
		}
		a.mu.Lock()
		e, ok := a.entries[got.Handle]
		a.mu.Unlock()
		if !ok || e.ended {
			introspecttest.WriteJSON(w, introspecttest.InactiveReply(got.Nonce))
			return
		}
		m := introspecttest.ActiveReply(got.Nonce, "aicrew-example", g.hubID)
		m["identity"] = map[string]any{"user_id": g.aliceID, "token_id": e.token}
		m["session_id"] = e.session
		m["generation"] = e.generation
		if e.expired {
			m["handle_expires_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		}
		introspecttest.WriteJSON(w, m)
	})
	return a
}

func (a *aicrewSessions) set(handle string, e aicrewEntry) {
	a.mu.Lock()
	a.entries[handle] = e
	a.mu.Unlock()
}

// mcpRun drives one real aimem mcp process bound to path through a list and
// one tool call, and returns the tool call's result line.
func (g *teamSessionRig) mcpRun(t *testing.T, path, tool, args string) string {
	t.Helper()
	stdin := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}` + "\n"
	out, errOut, err := runAimemEnv(t, t.TempDir(), g.state, stdin, []string{teamsession.EnvVar + "=" + path}, "mcp", "-p", "alpha")
	if err != nil {
		t.Fatalf("mcp: %v %s", err, errOut)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"id":2`) {
			return line
		}
	}
	t.Fatalf("no answer to the tool call: %s", out)
	return ""
}

func (g *teamSessionRig) openSession(t *testing.T, handle, session string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runAicrewSession([]string{"open", "--service", "aicrew-example", "--team", "team-1", "--session", session},
		strings.NewReader(handle), &out, g.state); err != nil {
		t.Fatalf("open %s: %v", session, err)
	}
	return strings.TrimSpace(out.String())
}

// TestTeamSessionRestartVerifiesBeforeReadiness: every new process restores
// only the session file and asks the hub first; stale, expired, ended and
// incomplete state stays blocked.
func TestTeamSessionRestartVerifiesBeforeReadiness(t *testing.T) {
	g := newTeamSessionRig(t)
	a := scriptAicrew(g)
	a.set(handleA, aicrewEntry{session: "sess-A", token: g.aliceTk, generation: "4"})
	path := g.openSession(t, handleA, "sess-A")

	for run := 0; run < 2; run++ { // a first start and a restart behave alike
		before := g.count()
		if line := g.mcpRun(t, path, "list_tasks", `{"project":"alpha"}`); !strings.Contains(line, g.taskID) {
			t.Fatalf("run %d: %s", run, line)
		}
		reqs := g.requestsSince(before)
		if len(reqs) < 2 || reqs[0].path != "/v1/access/identity" {
			t.Fatalf("run %d did not verify before its first tool: %+v", run, reqs)
		}
	}
	// aicrew reports the handle expired, then the session ended: blocked.
	a.set(handleA, aicrewEntry{session: "sess-A", token: g.aliceTk, generation: "4", expired: true})
	if line := g.mcpRun(t, path, "list_tasks", `{"project":"alpha"}`); !strings.Contains(line, "context_stale") || strings.Contains(line, g.taskID) {
		t.Fatalf("expired handle: %s", line)
	}
	a.set(handleA, aicrewEntry{session: "sess-A", token: g.aliceTk, generation: "4", ended: true})
	if line := g.mcpRun(t, path, "get_task", `{"id":"`+g.taskID+`"}`); !strings.Contains(line, "context_stale") {
		t.Fatalf("ended session: %s", line)
	}
	// An incomplete file never reaches the hub.
	incomplete := teamsession.PathFor(g.state, "sess-broken")
	os.MkdirAll(teamsession.Dir(g.state), 0o700)
	w, err := privatefile.Create(incomplete)
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(`{"version":1,"hub":"hub","url":"` + g.ts.URL + `","session_id":"sess-broken"}`)
	w.Close()
	before := g.count()
	if line := g.mcpRun(t, incomplete, "list_tasks", `{"project":"alpha"}`); !strings.Contains(line, "cannot be used") {
		t.Fatalf("incomplete file: %s", line)
	}
	if g.count() != before {
		t.Fatal("an incomplete session file reached the hub")
	}
}

// TestTeamSessionRotationBlocksUntilRefresh: after the individual token
// rotates, the old handle is stale; aicrew re-proves with the new token, the
// client refreshes, and the conversation reads again.
func TestTeamSessionRotationBlocksUntilRefresh(t *testing.T) {
	g := newTeamSessionRig(t)
	a := scriptAicrew(g)
	a.set(handleA, aicrewEntry{session: "sess-A", token: g.aliceTk, generation: "4"})
	path := g.openSession(t, handleA, "sess-A")
	if line := g.mcpRun(t, path, "list_tasks", `{"project":"alpha"}`); !strings.Contains(line, g.taskID) {
		t.Fatalf("before rotation: %s", line)
	}
	// Rotate: a new individual token, the old one revoked, hub.json updated.
	db, err := access.Open(g.reg.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tok2, secret2, err := db.IssueScoped("admin", g.aliceID, "agent", access.ScopeUser, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Revoke("admin", g.aliceTk); err != nil {
		t.Fatal(err)
	}
	if err := adapter.SaveHubs(g.state, map[string]*adapter.HubConfig{
		"hub": {URL: g.ts.URL, TaskToken: secret2, CAFile: g.caFile}}, "hub"); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"list_tasks", "session_context"} {
		args := `{"project":"alpha"}`
		if tool == "session_context" {
			args = `{}`
		}
		if line := g.mcpRun(t, path, tool, args); !strings.Contains(line, "context_stale") || strings.Contains(line, g.taskID) {
			t.Fatalf("%s after rotation, before re-proof: %s", tool, line)
		}
	}
	// aicrew re-proves the same session under the new token and advances
	// the generation; the client refreshes.
	a.set(handleA2, aicrewEntry{session: "sess-A", token: tok2.ID, generation: "5"})
	if err := runAicrewSession([]string{"refresh", "sess-A"}, strings.NewReader(handleA2), &bytes.Buffer{}, g.state); err != nil {
		t.Fatalf("refresh after re-proof: %v", err)
	}
	if f, err := teamsession.Load(path); err != nil || f.TokenID != tok2.ID || f.Generation != "5" {
		t.Fatalf("refreshed file: %+v %v", f, err)
	}
	if line := g.mcpRun(t, path, "list_tasks", `{"project":"alpha"}`); !strings.Contains(line, g.taskID) {
		t.Fatalf("after refresh: %s", line)
	}
	line := g.mcpRun(t, path, "session_context", `{}`)
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	json.Unmarshal([]byte(line), &resp)
	if len(resp.Result.Content) == 0 || !strings.Contains(resp.Result.Content[0].Text, `"generation":"5"`) {
		t.Fatalf("session_context after refresh: %s", line)
	}
}

// TestTeamSessionCloseKeepsActiveState: close drops the file only once the
// hub confirms the session has ended, and only its own file.
func TestTeamSessionCloseKeepsActiveState(t *testing.T) {
	g := newTeamSessionRig(t)
	a := scriptAicrew(g)
	a.set(handleA, aicrewEntry{session: "sess-A", token: g.aliceTk, generation: "4"})
	a.set(handleB, aicrewEntry{session: "sess-B", token: g.aliceTk, generation: "4"})
	pathA, pathB := g.openSession(t, handleA, "sess-A"), g.openSession(t, handleB, "sess-B")
	closeCmd := func(session string) (string, error) {
		var out bytes.Buffer
		err := runAicrewSession([]string{"close", session}, strings.NewReader(""), &out, g.state)
		return out.String(), err
	}
	if _, err := closeCmd("sess-A"); err == nil || !strings.Contains(err.Error(), "still reports it active") {
		t.Fatalf("close of an active session: %v", err)
	}
	if _, err := os.Stat(pathA); err != nil {
		t.Fatal("an active session's file was dropped")
	}
	a.set(handleA, aicrewEntry{session: "sess-A", token: g.aliceTk, generation: "4", ended: true})
	if out, err := closeCmd("sess-A"); err != nil || !strings.Contains(out, "closed") {
		t.Fatalf("close after aicrew ended it: %v %s", err, out)
	}
	if _, err := os.Stat(pathA); !os.IsNotExist(err) {
		t.Fatal("the ended session's file is still there")
	}
	if _, err := os.Stat(pathB); err != nil {
		t.Fatal("closing one session touched another")
	}
	if out, err := closeCmd("sess-A"); err != nil || !strings.Contains(out, "not open here") {
		t.Fatalf("second close: %v %s", err, out)
	}
	// A hub that cannot answer keeps the file.
	a.set(handleB, aicrewEntry{session: "sess-B", token: g.aliceTk, generation: "4", ended: true})
	g.ts.Close()
	if _, err := closeCmd("sess-B"); err == nil || !strings.Contains(err.Error(), "could not confirm") {
		t.Fatalf("close with the hub down: %v", err)
	}
	if _, err := os.Stat(pathB); err != nil {
		t.Fatal("the file was dropped while the hub could not answer")
	}
	// A file other accounts can read binds nothing and goes.
	if err := privatefile.Expose(pathB); err != nil {
		t.Fatal(err)
	}
	if _, err := closeCmd("sess-B"); err != nil {
		t.Fatalf("close of an exposed file: %v", err)
	}
	if _, err := os.Stat(pathB); !os.IsNotExist(err) {
		t.Fatal("an exposed session file was kept")
	}
}

// TestTeamSessionLifecycleCommandsNeverUndoEachOther interleaves lifecycle
// commands for one session deterministically: the fake aicrew holds one
// command's verification while another completes. Whatever the caller,
// no command removes or overwrites a file it did not start from.
func TestTeamSessionLifecycleCommandsNeverUndoEachOther(t *testing.T) {
	g := newTeamSessionRig(t)
	a := scriptAicrew(g)
	run := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := runAicrewSession(args, strings.NewReader(stdin), &out, g.state)
		return out.String(), err
	}
	type result struct {
		out string
		err error
	}
	background := func(stdin string, args ...string) chan result {
		done := make(chan result, 1)
		go func() {
			out, err := run(stdin, args...)
			done <- result{out, err}
		}()
		return done
	}
	path := teamsession.PathFor(g.state, "sess-A")
	holding := func(want string) {
		t.Helper()
		f, err := teamsession.Load(path)
		if err != nil || f.Handle != want {
			t.Fatalf("the session file does not hold the expected handle: %v", err)
		}
	}

	// Close waits on the hub for H1 while a refresh writes H2; the stale
	// answer for H1 must not remove H2.
	a.set(handleA, aicrewEntry{session: "sess-A", token: g.aliceTk, generation: "4"})
	g.openSession(t, handleA, "sess-A")
	h := a.hold(handleA)
	closing := background("", "close", "sess-A")
	<-h.entered
	a.set(handleA2, aicrewEntry{session: "sess-A", token: g.aliceTk, generation: "5"})
	if out, err := run(handleA2, "refresh", "sess-A"); err != nil {
		t.Fatalf("refresh during close: %v %s", err, out)
	}
	a.set(handleA, aicrewEntry{session: "sess-A", ended: true})
	close(h.release)
	if r := <-closing; r.err == nil || !strings.Contains(r.err.Error(), "was kept") {
		t.Fatalf("close removed a file a refresh wrote meanwhile: %v %s", r.err, r.out)
	}
	holding(handleA2)

	// A refresh waits on the hub for H3 while close removes the ended H2
	// file; the refresh must not bring the closed session back.
	const handleA3 = "acs1_" + "cccccccccccccccccccccccccccccccccccccccccc3"
	h = a.hold(handleA3)
	refreshing := background(handleA3, "refresh", "sess-A")
	<-h.entered
	a.set(handleA2, aicrewEntry{session: "sess-A", ended: true})
	if out, err := run("", "close", "sess-A"); err != nil || !strings.Contains(out, "closed") {
		t.Fatalf("close during refresh: %v %s", err, out)
	}
	a.set(handleA3, aicrewEntry{session: "sess-A", token: g.aliceTk, generation: "6"})
	close(h.release)
	if r := <-refreshing; r.err == nil || !strings.Contains(r.err.Error(), "nothing was written") {
		t.Fatalf("a refresh revived a closed session: %v %s", r.err, r.out)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the closed session's file is back: %v", err)
	}

	// Two opens for one session: the one verified later must not overwrite
	// the file the other wrote.
	a.set(handleA, aicrewEntry{session: "sess-A", token: g.aliceTk, generation: "7"})
	h = a.hold(handleA)
	opening := background(handleA, "open", "--service", "aicrew-example", "--team", "team-1", "--session", "sess-A")
	<-h.entered
	g.openSession(t, handleA3, "sess-A")
	close(h.release)
	if r := <-opening; r.err == nil || !strings.Contains(r.err.Error(), "nothing was written") {
		t.Fatalf("an open overwrote a session opened meanwhile: %v %s", r.err, r.out)
	}
	holding(handleA3)
}
