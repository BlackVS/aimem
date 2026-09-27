package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/adapter"
	"aimem/internal/introspect"
	"aimem/internal/introspect/introspecttest"
	"aimem/internal/privatefile"
	"aimem/internal/store"
	"aimem/internal/teamsession"
)

func TestReceiptSinkAcceptsOnlyAPipe(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := receiptSink(w); err != nil {
		t.Fatalf("a pipe was refused: %v", err)
	}
	file, err := os.Create(filepath.Join(t.TempDir(), "receipt"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := receiptSink(file); err == nil || !strings.Contains(err.Error(), "not a pipe") {
		t.Fatalf("a file was accepted: %v", err)
	}
	// Terminals are character devices; the null device is the portable one.
	dev, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	err = receiptSink(dev)
	if err == nil {
		t.Fatal("a character device was accepted")
	}
	if fi, _ := dev.Stat(); fi.Mode()&os.ModeCharDevice != 0 && !strings.Contains(err.Error(), "stdout is a terminal") {
		t.Fatalf("a character device got %v", err)
	}
}

// teamSessionRig is a real TLS hub with a fake aicrew as its peer, a team
// profile granted project alpha, alice's individual credential, and a client
// state root configured for that hub.
type teamSessionRig struct {
	*identityCLIRig
	fake    *introspecttest.Fake
	hubID   string
	state   string
	aliceID string
	aliceTk string
	secret  string
	taskID  string
	mu      sync.Mutex
	seen    []seenReq
}

type seenReq struct{ path, handle string }

var (
	handleA  = "acs1_" + strings.Repeat("A", 43)
	handleA2 = "acs1_" + strings.Repeat("a", 43)
	handleB  = "acs1_" + strings.Repeat("B", 43)
)

func newTeamSessionRig(t *testing.T) *teamSessionRig {
	t.Helper()
	g := &teamSessionRig{}
	g.identityCLIRig = newIdentityCLIRig(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			g.mu.Lock()
			g.seen = append(g.seen, seenReq{r.URL.Path, r.Header.Get(teamsession.Header)})
			g.mu.Unlock()
			h.ServeHTTP(w, r)
		})
	})
	db, err := access.Open(g.reg.Root())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if g.hubID, err = db.HubID(); err != nil {
		t.Fatal(err)
	}
	u, err := db.CreateUser("admin", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	tok, secret, err := db.IssueScoped("admin", u.ID, "agent", access.ScopeUser, "", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	g.aliceID, g.aliceTk, g.secret = u.ID, tok.ID, secret

	// The fake aicrew answers each handle as one of alice's sessions.
	g.fake = introspecttest.New(t, "aicrew-example", g.hubID)
	sessions := map[string]string{handleA: "sess-A", handleA2: "sess-A", handleB: "sess-B"}
	g.fake.SetAnswer(func(w http.ResponseWriter, got introspecttest.Request) {
		session, ok := sessions[got.Handle]
		if !ok {
			introspecttest.WriteJSON(w, introspecttest.InactiveReply(got.Nonce))
			return
		}
		m := introspecttest.ActiveReply(got.Nonce, "aicrew-example", g.hubID)
		m["identity"] = map[string]any{"user_id": g.aliceID, "token_id": g.aliceTk}
		m["session_id"] = session
		introspecttest.WriteJSON(w, m)
	})
	credFile := filepath.Join(t.TempDir(), "introspection.token")
	fh, err := privatefile.Create(credFile)
	if err != nil {
		t.Fatal(err)
	}
	fh.WriteString("aicrew_introspect_TESTSECRET_e5a\n")
	fh.Close()
	g.s.SetIntrospectionClient(&introspect.Client{TokenFile: credFile, RootCAs: g.fake.CA.Pool})
	g.mustRun(t, "peer", "register", "aicrew-example", "--endpoint", g.fake.Endpoint(), "--peer-trust-pin", g.fake.Pin)

	// Project alpha, with a task, granted to team-1.
	pdb, err := g.reg.Open("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := pdb.SetMeta(store.TasksMetaKey, "on"); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", g.ts.URL+"/v1/projects/alpha/tasks", strings.NewReader(`{"title":"team task"}`))
	req.Header.Set("Authorization", "Bearer "+identityAdminToken)
	req.Header.Set("Idempotency-Key", "e5a-task")
	resp, err := g.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var task struct {
		ID string `json:"id"`
	}
	json.NewDecoder(resp.Body).Decode(&task)
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Fatalf("create task: %d", resp.StatusCode)
	}
	g.taskID = task.ID
	g.mustRun(t, "team", "create", "aicrew-example", "team-1")
	g.mustRun(t, "team", "grant", "aicrew-example", "team-1", "alpha")

	// The agent machine's state root: the hub with alice's individual
	// credential and the hub's CA.
	g.state = t.TempDir()
	if err := adapter.SaveHubs(g.state, map[string]*adapter.HubConfig{
		"hub": {URL: g.ts.URL, TaskToken: secret, CAFile: g.caFile}}, "hub"); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *teamSessionRig) requestsSince(i int) []seenReq {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]seenReq(nil), g.seen[i:]...)
}

func (g *teamSessionRig) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.seen)
}

func TestIdentityProofWritesTheReceiptOnlyIntoAPipe(t *testing.T) {
	g := newTeamSessionRig(t)
	args := []string{"--peer", "aicrew-example", "--hub-id", g.hubID, "--challenge", "challenge-1"}
	// A file is refused before anything is asked of the hub.
	before := g.count()
	file, _ := os.Create(filepath.Join(t.TempDir(), "receipt"))
	if err := identityProofCmd(args, file, g.state); err == nil {
		t.Fatal("a receipt was written to a file")
	}
	file.Close()
	if raw, _ := os.ReadFile(file.Name()); len(raw) != 0 {
		t.Fatal("the file holds something")
	}
	if g.count() != before {
		t.Fatal("the hub was asked for a receipt that could not be delivered privately")
	}
	// A pipe gets exactly the receipt.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := identityProofCmd(args, w, g.state); err != nil {
		t.Fatalf("proof into a pipe: %v", err)
	}
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	receipt := strings.TrimSpace(string(out))
	if !strings.HasPrefix(receipt, "amr1_") || len(receipt) != 48 {
		t.Fatalf("pipe output %q", out)
	}
	// A real process whose stdout is a pipe gets it too; the receipt goes
	// nowhere but that stdout.
	stdout, stderr, err := runAimemEnv(t, t.TempDir(), g.state, "", nil, append([]string{"identity", "proof"}, args...)...)
	if err != nil || !strings.HasPrefix(strings.TrimSpace(stdout), "amr1_") || strings.Contains(stderr, "amr1_") {
		t.Fatalf("process proof: %v %q %q", err, stdout, stderr)
	}
}

func TestTeamSessionCommands(t *testing.T) {
	g := newTeamSessionRig(t)
	run := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := runAicrewSession(args, strings.NewReader(stdin), &out, g.state)
		return out.String(), err
	}
	if _, err := run("not a handle", "open", "--service", "aicrew-example", "--team", "team-1", "--session", "sess-A"); err == nil {
		t.Fatal("a non-handle was accepted")
	}
	// A handle for another session writes nothing.
	if _, err := run(handleB, "open", "--service", "aicrew-example", "--team", "team-1", "--session", "sess-A"); err == nil || !strings.Contains(err.Error(), "another service, team or session") {
		t.Fatalf("mismatched open: %v", err)
	}
	out, err := run(handleA+"\n", "open", "--service", "aicrew-example", "--team", "team-1", "--session", "sess-A")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	path := strings.TrimSpace(out)
	if path != teamsession.PathFor(g.state, "sess-A") {
		t.Fatalf("open printed %q", out)
	}
	f, err := teamsession.Load(path)
	if err != nil || f.UserID != g.aliceID || f.TokenID != g.aliceTk || f.Handle != handleA || f.Generation != "4" {
		t.Fatalf("session file: %+v %v", f, err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), g.secret) || strings.Contains(string(raw), "aicrew_introspect") {
		t.Fatal("the session file holds a credential")
	}
	if _, err := run(handleA, "open", "--service", "aicrew-example", "--team", "team-1", "--session", "sess-A"); err == nil || !strings.Contains(err.Error(), "already open") {
		t.Fatalf("second open: %v", err)
	}
	if out, err := run("", "status", "sess-A"); err != nil || strings.Contains(out, handleA) || !strings.Contains(out, `"session_id": "sess-A"`) {
		t.Fatalf("status: %v %s", err, out)
	}
	if _, err := run(handleA2, "refresh", "sess-A"); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if f, _ := teamsession.Load(path); f.Handle != handleA2 {
		t.Fatal("refresh kept the old handle")
	}
	if _, err := run(handleB, "refresh", "sess-A"); err == nil {
		t.Fatal("a handle for another session refreshed sess-A")
	}
	if f, _ := teamsession.Load(path); f.Handle != handleA2 {
		t.Fatal("a refused refresh changed the file")
	}
	if out, err := run("", "close", "sess-A"); err != nil || !strings.Contains(out, "closed") {
		t.Fatalf("close: %v %s", err, out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("close left the file")
	}
}

// TestTeamConversationsThroughRealMCPProcesses binds two conversations of
// one checkout to two sessions and drives two real aimem mcp processes.
func TestTeamConversationsThroughRealMCPProcesses(t *testing.T) {
	g := newTeamSessionRig(t)
	open := func(handle, session string) string {
		var out bytes.Buffer
		if err := runAicrewSession([]string{"open", "--service", "aicrew-example", "--team", "team-1", "--session", session},
			strings.NewReader(handle), &out, g.state); err != nil {
			t.Fatalf("open %s: %v", session, err)
		}
		return strings.TrimSpace(out.String())
	}
	pathA, pathB := open(handleA, "sess-A"), open(handleB, "sess-B")
	checkout := t.TempDir()
	stdin := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_tasks","arguments":{"project":"alpha"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"session_context","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"remember","arguments":{"text":"x"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"create_task","arguments":{"project":"alpha","title":"x"}}}`,
	}, "\n") + "\n"
	before := g.count()
	type result struct {
		out, errOut string
		err         error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i, path := range []string{pathA, pathB} {
		wg.Add(1)
		go func(i int, path string) {
			defer wg.Done()
			out, errOut, err := runAimemEnv(t, checkout, g.state, stdin, []string{teamsession.EnvVar + "=" + path}, "mcp", "-p", "alpha")
			results[i] = result{out, errOut, err}
		}(i, path)
	}
	wg.Wait()
	for i, want := range []string{"sess-A", "sess-B"} {
		res := results[i]
		if res.err != nil {
			t.Fatalf("mcp %s: %v %s", want, res.err, res.errOut)
		}
		byID := map[int]string{}
		sc := bufio.NewScanner(strings.NewReader(res.out))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var m struct {
				ID int `json:"id"`
			}
			json.Unmarshal(sc.Bytes(), &m)
			byID[m.ID] = sc.Text()
		}
		if strings.Contains(byID[2], `"remember"`) || strings.Contains(byID[2], `"create_task"`) || !strings.Contains(byID[2], `"session_context"`) {
			t.Errorf("%s tool list: %s", want, byID[2])
		}
		if !strings.Contains(byID[3], g.taskID) || strings.Contains(byID[3], `"isError":true`) {
			t.Errorf("%s list_tasks: %s", want, byID[3])
		}
		if !strings.Contains(byID[4], want) || !strings.Contains(byID[4], `\"knowledge\":\"unavailable\"`) && !strings.Contains(byID[4], "unavailable") {
			t.Errorf("%s session_context: %s", want, byID[4])
		}
		for _, id := range []int{5, 6} {
			if !strings.Contains(byID[id], "not available in a team conversation") {
				t.Errorf("%s call %d: %s", want, id, byID[id])
			}
		}
		for _, h := range []string{handleA, handleB} {
			if strings.Contains(res.out, h) || strings.Contains(res.errOut, h) {
				t.Errorf("%s printed a handle", want)
			}
		}
	}
	// Every hub request of the two conversations carried one of the two
	// handles; none went without.
	reqs := g.requestsSince(before)
	perHandle := map[string]int{}
	for _, r := range reqs {
		if r.handle != handleA && r.handle != handleB {
			t.Fatalf("a team conversation sent %q to %s", r.handle, r.path)
		}
		perHandle[r.handle]++
	}
	if perHandle[handleA] == 0 || perHandle[handleB] == 0 {
		t.Fatalf("per-handle requests: %v", perHandle)
	}
	// Personal mode in the same checkout is untouched: no team header.
	before = g.count()
	runAimemEnv(t, checkout, g.state, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n", nil, "mcp", "-p", "alpha")
	for _, r := range g.requestsSince(before) {
		if r.handle != "" {
			t.Fatalf("a personal process sent a team header to %s", r.path)
		}
	}
}

// runAimemEnv is runAimem with extra environment entries.
func runAimemEnv(t *testing.T, dir, state, stdin string, env []string, args ...string) (string, string, error) {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperMain$")
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "AIMEM_TEST_MAIN=1", "AIMEM_TEST_ARGS="+strings.Join(args, "\x1f"),
		"AIMEM_STATE_DIR="+state, "XDG_STATE_HOME="+state, "HOME="+home, "USERPROFILE="+home,
		"AIMEM_HUB_URL=", "AIMEM_HUB_TOKEN=", teamsession.EnvVar+"="), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), errOut.String(), err
	case <-time.After(90 * time.Second):
		cmd.Process.Kill()
		t.Errorf("aimem %v did not finish; stderr: %s", args, errOut.String())
		return "", "", nil
	}
}
