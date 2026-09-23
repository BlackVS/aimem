package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/store"
)

// fakeOperatorHub is the hub's local operator API as the provisioner uses
// it: the access listing, user creation, grants, team listing, creation
// and configuration, and token issuance; the state it keeps lets a test
// check exactly what one run created.
type fakeOperatorHub struct {
	users     []access.User
	grants    map[string]bool // instance/user
	teams     []store.Team
	tokens    []access.Token
	requests  []string
	forbidden bool // an ordinary caller
	conflicts int  // configure replies to refuse with 409 first
	secrets   int
}

func newFakeOperatorHub() *fakeOperatorHub {
	return &fakeOperatorHub{grants: map[string]bool{}, users: []access.User{{ID: "u-alice", Name: "alice"}, {ID: "u-bob", Name: "bob"}, {ID: "u-dup1", Name: "dup"}, {ID: "u-dup2", Name: "dup"}, {ID: "u-off", Name: "off", Disabled: true}}}
}

func (h *fakeOperatorHub) call(method, path string, body any) (int, []byte, error) {
	h.requests = append(h.requests, method+" "+path)
	reply := func(code int, v any) (int, []byte, error) {
		raw, _ := json.Marshal(v)
		return code, raw, nil
	}
	if h.forbidden {
		return reply(403, map[string]any{"error": "ordinary token is not authorized for this endpoint"})
	}
	raw, _ := json.Marshal(body)
	switch {
	case method == "GET" && path == "/v1/access":
		return reply(200, access.Snapshot{Users: h.users, Tokens: h.tokens})
	case method == "POST" && path == "/v1/access/users":
		var req struct{ Name string }
		json.Unmarshal(raw, &req)
		u := access.User{ID: "u-" + req.Name, Name: req.Name}
		h.users = append(h.users, u)
		return reply(200, u)
	case method == "PUT" && strings.HasPrefix(path, "/v1/projects/alpha/access/user/"):
		h.grants["inst-alpha/"+strings.TrimPrefix(path, "/v1/projects/alpha/access/user/")] = true
		return reply(200, map[string]any{"ok": true, "project_instance": "inst-alpha"})
	case method == "GET" && strings.HasPrefix(path, "/v1/projects/alpha/teams?"):
		return reply(200, map[string]any{"protocol_version": 1, "teams": h.teams, "next_cursor": ""})
	case method == "POST" && path == "/v1/projects/alpha/teams":
		var c store.TeamContent
		json.Unmarshal(raw, &c)
		for _, t := range h.teams {
			if t.Name == c.Name {
				return reply(409, map[string]any{"error": "team name already exists"})
			}
		}
		t := store.Team{ID: fmt.Sprintf("team-%d", len(h.teams)+1), Revision: 1, TeamContent: c}
		h.teams = append(h.teams, t)
		return reply(201, t)
	case method == "PUT" && strings.HasPrefix(path, "/v1/projects/alpha/teams/"):
		var req struct {
			store.TeamContent
			ExpectedRevision int64 `json:"expected_revision"`
		}
		json.Unmarshal(raw, &req)
		id := strings.TrimPrefix(path, "/v1/projects/alpha/teams/")
		for i := range h.teams {
			if h.teams[i].ID != id {
				continue
			}
			if h.conflicts > 0 {
				h.conflicts--
				h.teams[i].Revision++ // someone else changed it
				return reply(409, map[string]any{"error": "team changed since the expected revision"})
			}
			if req.ExpectedRevision != h.teams[i].Revision {
				return reply(409, map[string]any{"error": "team changed since the expected revision"})
			}
			h.teams[i].TeamContent = req.TeamContent
			h.teams[i].Revision++
			return reply(200, h.teams[i])
		}
		return reply(404, map[string]any{"error": "team or project not found"})
	case method == "POST" && path == "/v1/access/tokens":
		var req struct {
			UserID    string    `json:"user_id"`
			Label     string    `json:"label"`
			Project   string    `json:"project"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		json.Unmarshal(raw, &req)
		h.secrets++
		t := access.Token{ID: fmt.Sprintf("tok-%d", h.secrets), UserID: req.UserID, Label: req.Label, Scope: "project", Project: "inst-alpha", ExpiresAt: req.ExpiresAt}
		h.tokens = append(h.tokens, t)
		return reply(200, map[string]any{"token": t, "secret": fmt.Sprintf("aimem_user_%064d", h.secrets)})
	}
	return reply(404, map[string]any{"error": "unexpected " + method + " " + path})
}

func runProvision(t *testing.T, h *fakeOperatorHub, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runTeamProvision(args, h.call, &out)
	return out.String(), err
}

func TestTeamProvisionCreateThenAddMembersIdempotently(t *testing.T) {
	h := newFakeOperatorHub()
	exp := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	out, err := runProvision(t, h, "create", "alpha", "--team", "Pilot", "--coordinator", "alice", "--expiry", exp)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"user alice: existing (id u-alice)", "grant on project alpha: set", "team Pilot: created (id team-1, revision 1) with alice enrolled as coordinator", "token team-Pilot-alice: issued for alice (id tok-1", "one-time secret for alice", "aimem_user_" + fmt.Sprintf("%064d", 1), "/join_team Pilot coordinator"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if !h.grants["inst-alpha/u-alice"] || len(h.teams) != 1 || len(h.tokens) != 1 {
		t.Fatalf("state %+v %+v", h.grants, h.teams)
	}
	// Rerun: nothing duplicated, the token is not reissued, no secret shown.
	out, err = runProvision(t, h, "create", "alpha", "--team", "Pilot", "--coordinator", "alice", "--expiry", exp)
	if err != nil || !strings.Contains(out, "team Pilot: existing") || !strings.Contains(out, "alice already enrolled (coordinator: true)") || !strings.Contains(out, "token team-Pilot-alice: exists for alice since before this run") || strings.Contains(out, "one-time secret") || len(h.tokens) != 1 || len(h.teams) != 1 {
		t.Fatalf("rerun: %v tokens %d\n%s", err, len(h.tokens), out)
	}
	// Add a worker with a new user and a secret file.
	secret := filepath.Join(t.TempDir(), "worker.secret")
	out, err = runProvision(t, h, "add", "alpha", "--team", "Pilot", "--member", "worker-b", "--role", "worker", "--create-user", "--expiry", exp, "--secret-file", secret)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"user worker-b: created (id u-worker-b)", "enrollment: worker-b added as worker (revision 2)", "token team-Pilot-worker-b: issued", "secret written once to " + secret} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "aimem_user_") {
		t.Fatal("secret printed although --secret-file was given")
	}
	if b, err := os.ReadFile(secret); err != nil || !strings.HasPrefix(string(b), "aimem_user_") {
		t.Fatalf("secret file: %v %q", err, b)
	}
	if fi, _ := os.Stat(secret); fi == nil {
		t.Fatal("secret file missing")
	}
	if len(h.teams[0].Enrollment) != 2 || h.teams[0].Enrollment[1].UserID != "u-worker-b" || h.teams[0].Enrollment[1].Coordinator || h.teams[0].Enrollment[0].UserID != "u-alice" {
		t.Fatalf("enrollment %+v", h.teams[0].Enrollment)
	}
	// Existing file is never overwritten.
	if _, err := runProvision(t, h, "add", "alpha", "--team", "team-1", "--member", "bob", "--role", "worker", "--expiry", exp, "--secret-file", secret); err == nil || !strings.Contains(err.Error(), "never overwritten") {
		t.Fatalf("overwrite accepted: %v", err)
	}
	// Promote bob: enrolled as worker first, then coordinator flag set; a
	// worker request afterwards does not remove it.
	if out, err := runProvision(t, h, "add", "alpha", "--team", "team-1", "--member", "bob", "--role", "worker", "--no-token"); err != nil || !strings.Contains(out, "bob added as worker") || !strings.Contains(out, "token: skipped (--no-token)") {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := runProvision(t, h, "add", "alpha", "--team", "Pilot", "--member", "u-bob", "--role", "coordinator", "--no-token"); err != nil || !strings.Contains(out, "bob now coordinator-eligible") {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := runProvision(t, h, "add", "alpha", "--team", "Pilot", "--member", "bob", "--role", "worker", "--no-token"); err != nil || !strings.Contains(out, "a worker request does not remove that flag") {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(h.tokens) != 2 {
		t.Fatalf("tokens issued for --no-token runs: %d", len(h.tokens))
	}
}

func TestTeamProvisionConflictsAndRefusals(t *testing.T) {
	h := newFakeOperatorHub()
	exp := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if _, err := runProvision(t, h, "create", "alpha", "--team", "Pilot", "--coordinator", "alice", "--expiry", exp); err != nil {
		t.Fatal(err)
	}
	// A revision conflict is retried once against the re-read team.
	h.conflicts = 1
	out, err := runProvision(t, h, "add", "alpha", "--team", "Pilot", "--member", "bob", "--role", "worker", "--no-token")
	if err != nil || !strings.Contains(out, "team revision moved; re-reading") || !strings.Contains(out, "bob added as worker (revision 3)") {
		t.Fatalf("%v\n%s", err, out)
	}
	// Adding to a missing team is refused; an ambiguous name is refused; a
	// missing user without --create-user is refused; a disabled user is refused.
	for _, tc := range []struct{ args, want string }{
		{"add alpha --team Nope --member bob --role worker --no-token", `no team "Nope"`},
		{"add alpha --team Pilot --member dup --role worker --no-token", "is ambiguous (ids u-dup1, u-dup2)"},
		{"add alpha --team Pilot --member carol --role worker --no-token", `no user named "carol"; pass --create-user`},
		{"add alpha --team Pilot --member off --role worker --no-token", "is disabled"},
		{"add alpha --team Pilot --member u-off --role worker --no-token", "is disabled"},
		{"add alpha --team Pilot --member bob --role worker", "--expiry RFC3339 is required"},
		{"create alpha --team Pilot --coordinator alice --expiry 2020-01-01T00:00:00Z", "expiry must be in the future"},
		{"add alpha --team Pilot --member bob --role reviewer --no-token", "usage:"},
	} {
		if _, err := runProvision(t, h, strings.Fields(tc.args)...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: %v", tc.args, err)
		}
	}
	// The disabled user was refused before any mutation, by name and by ID.
	if h.grants["inst-alpha/u-off"] || len(h.teams[0].Enrollment) != 2 {
		t.Fatalf("disabled user provisioned: grants %v enrollment %+v", h.grants, h.teams[0].Enrollment)
	}
	// A live same-label token for another project is a collision, not a
	// usable credential: refused with the label advice, nothing issued and
	// nothing reported as reused. A user-scoped one authorizes any granted
	// project and counts as existing.
	exp2 := time.Now().Add(48 * time.Hour).UTC()
	h.tokens = append(h.tokens, access.Token{ID: "tok-beta", UserID: "u-bob", Label: "team-Pilot-bob", Scope: access.ScopeProject, Project: "inst-beta", ExpiresAt: exp2})
	before := len(h.tokens)
	out, err = runProvision(t, h, "add", "alpha", "--team", "Pilot", "--member", "bob", "--role", "worker", "--expiry", exp)
	if err == nil || !strings.Contains(err.Error(), "does not authorize project alpha (id tok-beta") || !strings.Contains(err.Error(), "pass --label with another name") || strings.Contains(out, "not reissued") || len(h.tokens) != before {
		t.Fatalf("other-project token accepted: %v tokens %d\n%s", err, len(h.tokens), out)
	}
	h.tokens = append(h.tokens, access.Token{ID: "tok-user", UserID: "u-bob", Label: "bob-user", Scope: access.ScopeUser, ExpiresAt: exp2})
	before = len(h.tokens)
	if out, err := runProvision(t, h, "add", "alpha", "--team", "Pilot", "--member", "bob", "--role", "worker", "--expiry", exp, "--label", "bob-user"); err != nil || !strings.Contains(out, "token bob-user: exists for bob since before this run (id tok-user") || len(h.tokens) != before {
		t.Fatalf("user-scoped token not reused: %v\n%s", err, out)
	}
	// A partial failure reports what was done; a rerun repeats idempotently.
	h.forbidden = true
	out, err = runProvision(t, h, "add", "alpha", "--team", "Pilot", "--member", "bob", "--role", "worker", "--expiry", exp)
	if err == nil || !strings.Contains(err.Error(), "local operator authority, never a member token") {
		t.Fatalf("%v\n%s", err, out)
	}
	h.forbidden = false
	if len(h.tokens) != before {
		t.Fatal("token issued on a refused run")
	}
}

func TestTeamProvisionArgsAndHTTPHelpers(t *testing.T) {
	for _, args := range [][]string{nil, {"create"}, {"remove", "alpha"}, {"create", "--team", "x"}, {"create", "alpha", "--team", "x"}, {"add", "alpha", "--team", "x", "--member", "y", "--no-token"}} {
		if _, err := parseTeamProvisionArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	o, err := parseTeamProvisionArgs([]string{"add", "alpha", "--team", "My Team", "--member", "bob", "--role", "worker", "--no-token"})
	if err != nil || o.team != "My Team" || o.role != "worker" || !o.noToken {
		t.Fatalf("%v %+v", err, o)
	}
	p := &provisioner{o: o, out: &bytes.Buffer{}}
	if err := p.hubError(http.StatusForbidden, []byte(`{"error":"nope"}`)); !strings.Contains(err.Error(), "operator authority") {
		t.Fatal(err)
	}
}
