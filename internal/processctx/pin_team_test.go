package processctx

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/hubtest"
	"aimem/internal/teamsession"
)

const (
	pinTeamToken  = "aimem_user_" + "7777777777777777777777777777777777777777777777777777777777777777"
	pinTeamHandle = "acs1_" + "hhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh"
)

// teamPinHub serves the process routes in team mode only: every request
// must carry the session's handle and the individual credential. A project
// outside granted is refused with the team envelope, as the hub refuses a
// project its team profile is not granted; the rest is fakeHub's.
type teamPinHub struct {
	inner   *fakeHub
	granted map[string]bool
	userID  string // the user the context report names

	mu  sync.Mutex
	bad []string // requests without the handle or the individual credential
}

func (h *teamPinHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(teamsession.Header) != pinTeamHandle || r.Header.Get("Authorization") != "Bearer "+pinTeamToken {
		h.mu.Lock()
		h.bad = append(h.bad, r.URL.Path)
		h.mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.URL.Path == "/v1/access/identity" {
		h.mu.Lock()
		user := h.userID
		h.mu.Unlock()
		rep := map[string]any{"mode": "team", "user_id": user, "token_id": "tok-1",
			"team": map[string]string{"service_id": "aicrew-example", "team_id": "team-1", "session_id": "sess-1", "role": "coordinator"}}
		json.NewEncoder(w).Encode(rep)
		return
	}
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/projects/"), "/")[0]
	if !h.granted[p] {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"code": "grant_denied", "message": "the team access profile holds no grant on this project", "active_mode": "team"})
		return
	}
	h.inner.ServeHTTP(w, r)
}

// teamPinSetup returns a checkout whose hub entry holds the individual
// credential and the checkpoint token, and AIMEM_TEAM_SESSION naming a
// session on that hub.
func teamPinSetup(t *testing.T, h *teamPinHub) (dir, root string) {
	t.Helper()
	ts := hubtest.NewServer(h)
	t.Cleanup(ts.Close)
	root, dir = t.TempDir(), t.TempDir()
	if err := adapter.SaveHubs(root, map[string]*adapter.HubConfig{"hub": {URL: ts.URL, Pin: hubtest.Pin(ts), Token: "checkpoint-secret", TaskToken: pinTeamToken}}, "hub"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aimem.json"), []byte(`{"project":"alpha"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path := teamsession.PathFor(root, "sess-1")
	f := teamsession.File{Version: 1, Hub: "hub", URL: ts.URL, UserID: "user-1", TokenID: "tok-1", ServiceID: "aicrew-example",
		TeamID: "team-1", SessionID: "sess-1", Generation: "1", Handle: pinTeamHandle, HandleExpiresAt: "2026-12-01T00:00:00Z", UpdatedAt: time.Now().UTC()}
	if err := teamsession.Save(path, f); err != nil {
		t.Fatal(err)
	}
	t.Setenv(teamsession.EnvVar, path)
	return dir, root
}

// In a team conversation, process pin reads under the team's grant: the
// session's handle and the individual credential on every request, never
// the checkpoint token, and the same six fields as a personal read.
func TestReadPinUnderATeamSession(t *testing.T) {
	noGit(t)
	b := bundle("# Handbook\n")
	sel := selection
	sel.Digest = b.Digest()
	inner := &fakeHub{}
	inner.change(func(h *fakeHub) { h.ref, h.set = &sel, b })
	h := &teamPinHub{inner: inner, granted: map[string]bool{"alpha": true}, userID: "user-1"}
	dir, root := teamPinSetup(t, h)

	pin, err := ReadPin(dir, root, "")
	sum := sha256.Sum256(b.Files[selection.Manifest])
	want := Pin{Project: "alpha", Repository: selection.Repo, Commit: selection.Commit, Manifest: selection.Manifest,
		SetDigest: b.Digest(), InstructionDigest: "sha256:" + hex.EncodeToString(sum[:])}
	if err != nil || *pin != want {
		t.Fatalf("team pin: %+v %v\nwant %+v", pin, err, want)
	}

	// A project the team holds no grant on: refused with the hub's reason
	// and the team's next step, nothing substituted.
	_, err = ReadPin(dir, root, "beta")
	if err == nil || !strings.Contains(err.Error(), "the hub refused to read project beta's process selection: grant_denied") ||
		!strings.Contains(err.Error(), "aimem identity team grant") {
		t.Fatalf("grantless project: %v", err)
	}

	// A hub that reports another user than the session's: nothing is read.
	h.mu.Lock()
	h.userID = "user-2"
	h.mu.Unlock()
	if _, err := ReadPin(dir, root, ""); err == nil || !strings.Contains(err.Error(), "different user, team or session") {
		t.Fatalf("another user: %v", err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.bad) != 0 {
		t.Fatalf("requests without the session's handle or the individual credential: %v", h.bad)
	}
	if inner.bearers["Bearer checkpoint-secret"] {
		t.Fatal("the checkpoint token was sent in team mode")
	}
}

// A session file that cannot be used ends the read before anything is sent.
func TestReadPinRefusesAnUnusableTeamSession(t *testing.T) {
	h := &teamPinHub{inner: &fakeHub{}, granted: map[string]bool{"alpha": true}, userID: "user-1"}
	dir, root := teamPinSetup(t, h)
	t.Setenv(teamsession.EnvVar, filepath.Join(t.TempDir(), "missing.json"))
	if _, err := ReadPin(dir, root, ""); err == nil || !strings.Contains(err.Error(), "the team session file cannot be used") {
		t.Fatalf("missing session file: %v", err)
	}
	if n := h.inner.calls.Load(); n != 0 {
		t.Fatalf("%d requests reached the hub", n)
	}
}
