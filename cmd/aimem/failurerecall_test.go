package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"aimem/internal/store"
)

// The SSH failure an agent hit on a push: the identity needs the hardware
// key's interactive PIN.
const sshPINFailure = "Exit code 128\n" +
	"sign_and_send_pubkey: signing failed for ECDSA-SK \"/home/dev/.ssh/id_ecdsa_sk\" from agent: agent refused operation\n" +
	"Enter PIN for ECDSA-SK key /home/dev/.ssh/id_ecdsa_sk: \n" +
	"git@github.com: Permission denied (publickey).\n" +
	"fatal: Could not read from remote repository."

// fakeStore serves the local service's recall route on a socket with canned
// memories per scope, and records the queries it got.
type fakeStore struct {
	sock    string
	mu      sync.Mutex
	queries []string
}

// shortSocket is a socket path within every platform's limit: a test's
// temporary directory can exceed it (104 bytes on macOS).
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "afr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s")
}

func startFakeStore(t *testing.T, memories map[string][]store.Memory) *fakeStore {
	t.Helper()
	fs := &fakeStore{sock: shortSocket(t)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/projects/{p}/memories/recall", func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.queries = append(fs.queries, r.URL.Query().Get("q"))
		fs.mu.Unlock()
		mems := memories[r.PathValue("p")]
		if mems == nil {
			mems = []store.Memory{}
		}
		json.NewEncoder(w).Encode(map[string]any{"memories": mems})
	})
	ln, err := net.Listen("unix", fs.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return fs
}

func (f *fakeStore) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.queries...)
}

func failurePayload(event, tool, errText string) string {
	b, _ := json.Marshal(map[string]any{
		"hook_event_name": event, "tool_name": tool, "error": errText, "is_interrupt": false,
		"tool_input": map[string]any{"command": "git push"},
	})
	return string(b)
}

// runFailureRecall runs the real subcommand with the payload on stdin,
// talking to the service on sock, and returns its additional context (""
// when it printed nothing).
func runFailureRecall(t *testing.T, sock, payload string, env ...string) string {
	t.Helper()
	env = append([]string{"AIMEM_SOCKET=" + sock}, env...)
	out, errOut, err := runAimemEnv(t, t.TempDir(), t.TempDir(), payload, env, "failure-recall")
	if err != nil || errOut != "" {
		t.Fatalf("failure-recall: %v %s", err, errOut)
	}
	if strings.TrimSpace(out) == "" {
		return ""
	}
	var got struct {
		Out struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Out.Event != "PostToolUseFailure" || got.Out.Context == "" {
		t.Fatalf("not a PostToolUseFailure context (%v): %s", err, out)
	}
	return got.Out.Context
}

// A failed push whose error matches the stored SSH rule brings it back as
// context, with the remedy, and nothing unrelated.
func TestFailureRecallReturnsTheMatchingRemedy(t *testing.T) {
	f := startFakeStore(t, map[string][]store.Memory{store.UserScopeProject: {
		{ID: "m-ssh", Kind: "solution", Text: "ssh: always pass -o PKCS11Provider=none; the hardware key's PIN prompt hangs non-interactive ssh and git push (agent refused operation)."},
		{ID: "m-other", Kind: "fact", Text: "The release workflow builds from tags reachable from master."},
	}})
	ctx := runFailureRecall(t, f.sock, failurePayload("PostToolUseFailure", "Bash", sshPINFailure))
	if !strings.Contains(ctx, "PKCS11Provider=none") || !strings.Contains(ctx, "(memory m-ssh)") || strings.Contains(ctx, "m-other") {
		t.Fatalf("context:\n%s", ctx)
	}
	q := f.seen()
	if len(q) == 0 {
		t.Fatal("the store was not asked")
	}
	for _, want := range []string{"agent", "refused", "pin", "publickey"} {
		if !strings.Contains(" "+q[0]+" ", " "+want+" ") {
			t.Errorf("query %q lacks %q", q[0], want)
		}
	}
	for _, noise := range []string{"128", "home", "dev", "fatal", "exit", "code"} {
		if strings.Contains(" "+q[0]+" ", " "+noise+" ") {
			t.Errorf("query %q keeps the volatile or common word %q", q[0], noise)
		}
	}
}

// Silent cases: nothing relevant stored, a successful command's event,
// another tool, an interrupt, no service, a team conversation.
func TestFailureRecallIsSilentOtherwise(t *testing.T) {
	f := startFakeStore(t, map[string][]store.Memory{store.UserScopeProject: {
		{ID: "m-weak", Text: "The agent wrote the operation log."}, // two ordinary words of a long error
	}})
	if ctx := runFailureRecall(t, f.sock, failurePayload("PostToolUseFailure", "Bash", sshPINFailure)); ctx != "" {
		t.Errorf("a memory sharing two ordinary words of a long error came back:\n%s", ctx)
	}
	asked := len(f.seen())
	for _, payload := range []string{
		failurePayload("PostToolUse", "Bash", sshPINFailure),
		failurePayload("PostToolUseFailure", "Edit", sshPINFailure),
		`{"hook_event_name":"PostToolUseFailure","tool_name":"Bash","error":"Exit code 1","is_interrupt":false}`,
		strings.Replace(failurePayload("PostToolUseFailure", "Bash", sshPINFailure), `"is_interrupt":false`, `"is_interrupt":true`, 1),
		"not json",
	} {
		if ctx := runFailureRecall(t, f.sock, payload); ctx != "" {
			t.Errorf("payload %.60q produced context:\n%s", payload, ctx)
		}
	}
	if n := len(f.seen()); n != asked {
		t.Errorf("the store was asked %d more times for payloads that are not a failed Bash command", n-asked)
	}
	if ctx := runFailureRecall(t, f.sock, failurePayload("PostToolUseFailure", "Bash", sshPINFailure), "AIMEM_TEAM_SESSION="+filepath.Join(t.TempDir(), "team.json")); ctx != "" {
		t.Errorf("a team conversation got context:\n%s", ctx)
	}
	if ctx := runFailureRecall(t, shortSocket(t), failurePayload("PostToolUseFailure", "Bash", sshPINFailure)); ctx != "" {
		t.Errorf("with no service running, context:\n%s", ctx)
	}
}

// A secret in the error never reaches the store, and a secret in a stored
// memory never reaches the context: a token by its pattern, and a known
// secret value (SESSIOND_SECRET_ENV) by its literal, inner whitespace and
// all, which the context's flattening must not get to first.
func TestFailureRecallNeverCarriesASecret(t *testing.T) {
	secret := "gh" + "p_" + strings.Repeat("Zx9", 12)
	literal := "Qv7  Lm2\tRk9  Wp4"
	f := startFakeStore(t, map[string][]store.Memory{store.UserScopeProject: {
		{ID: "m-tok", Text: "push refused: agent refused operation; the PIN is needed. Old token " + secret + " was revoked; the passphrase is " + literal + "."},
	}})
	ctx := runFailureRecall(t, f.sock, failurePayload("PostToolUseFailure", "Bash", sshPINFailure+"\nusing token "+secret),
		"SESSIOND_SECRET_ENV=AIMEM_TEST_PASSPHRASE", "AIMEM_TEST_PASSPHRASE="+literal)
	if ctx == "" || strings.Contains(ctx, secret) || strings.Contains(ctx, "Lm2") || strings.Contains(ctx, "Rk9") {
		t.Fatalf("context has a secret or is empty:\n%s", ctx)
	}
	for _, q := range f.seen() {
		if strings.Contains(strings.ToLower(q), strings.ToLower(secret)) || strings.Contains(q, "zx9") {
			t.Fatalf("the query carried the secret: %q", q)
		}
	}
}

// At most three memories come back, each bounded, best overlap first, and
// the whole context stays within its byte bound.
func TestFailureRecallIsBounded(t *testing.T) {
	var mems []store.Memory
	for i := 0; i < 10; i++ {
		mems = append(mems, store.Memory{ID: fmt.Sprintf("m%d", i), Text: "agent refused operation publickey " + strings.Repeat("padding words ", 100)})
	}
	mems = append(mems, store.Memory{ID: "m-best", Text: "agent refused operation: the PIN for the ECDSA-SK key; pass PKCS11Provider=none, publickey then works for remote repository pushes."})
	f := startFakeStore(t, map[string][]store.Memory{store.UserScopeProject: mems})
	ctx := runFailureRecall(t, f.sock, failurePayload("PostToolUseFailure", "Bash", sshPINFailure))
	if n := strings.Count(ctx, "\n- "); n != failureRecallHits {
		t.Errorf("%d memories, want %d:\n%s", n, failureRecallHits, ctx)
	}
	if len(ctx) > failureRecallMaxBytes {
		t.Errorf("context is %d bytes, over %d", len(ctx), failureRecallMaxBytes)
	}
	if lines := strings.Split(ctx, "\n"); len(lines) < 2 || !strings.HasSuffix(lines[1], "(memory m-best)") {
		t.Errorf("the best match is not first:\n%s", ctx)
	}
	for _, line := range strings.Split(strings.TrimSpace(ctx), "\n")[1:] {
		if len(line) > failureRecallHitBytes+64 {
			t.Errorf("a memory line is %d bytes", len(line))
		}
	}
}

func TestFailureRecallOverlap(t *testing.T) {
	for terms, want := range map[int]int{0: 0, 1: 1, 2: 2, 7: 2, 8: 3, 20: 3} {
		if got := failureRecallOverlap(terms); got != want {
			t.Errorf("failureRecallOverlap(%d) = %d, want %d", terms, got, want)
		}
	}
}

func TestFailureQueryTerms(t *testing.T) {
	got := failureQueryTerms("Exit code 1\nerror: cannot open /var/lib/app/data-7f3a9c21e.db: permission denied (errno 13)\nsee https://user@example.org/path/x for more\n")
	want := []string{"open", "data-7f3a9c21e.db", "permission", "denied", "errno", "example.org"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("terms %q, want %q", got, want)
	}
	var many []string
	for i := 0; i < 30; i++ {
		many = append(many, fmt.Sprintf("word%c%c", 'a'+i%26, 'a'+i/26))
	}
	if got := failureQueryTerms("Exit code 2\n" + strings.Repeat("alpha beta gamma ", 3) + strings.Join(many, " ")); len(got) != failureQueryMaxTerms {
		t.Errorf("%d terms, want the cap %d: %q", len(got), failureQueryMaxTerms, got)
	}
	if got := failureQueryTerms("Exit code 1\nfatal: bad object deadbeef1234cafe in submodule"); strings.Join(got, " ") != "bad object submodule" {
		t.Errorf("a hash was kept: %q", got)
	}
	if got := failureQueryTerms("Exit code 1\n"); len(got) != 0 {
		t.Errorf("an error without text gave terms %q", got)
	}
}
