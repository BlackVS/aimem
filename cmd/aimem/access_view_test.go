package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseExpiryAndSince(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 30, 15, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"2027-10-01":           time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC),
		"+90d":                 now.Add(90 * 24 * time.Hour),
		"2026-12-28T00:00:00Z": time.Date(2026, 12, 28, 0, 0, 0, 0, time.UTC),
	} {
		if got, err := parseExpiry(in, now); err != nil || !got.Equal(want) {
			t.Errorf("--expires %s: %v %v, want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"90d", "+90", "next week", "2027-13-01", "+d"} {
		if _, err := parseExpiry(bad, now); err == nil {
			t.Errorf("--expires %q accepted", bad)
		}
	}
	if got, err := parseSince("7d", now); err != nil || !got.Equal(now.Add(-7*24*time.Hour)) {
		t.Errorf("--since 7d: %v %v", got, err)
	}
	if _, err := parseSince("+7d", now); err == nil {
		t.Error("--since +7d accepted")
	}
}

// The checklist names every open item with the command that closes it,
// and says ready once tasks, the process, a grant and a live token that
// can write are all there.
func TestReadinessChecklist(t *testing.T) {
	reg := projectService(t)
	if _, err := reg.Open("example"); err != nil {
		t.Fatal(err)
	}
	show := func() string {
		out, err := stdoutOf(t, func() error { return projectNamespaceCmd([]string{"show", "-p", "example"}) })
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
	}
	access := func(args ...string) string {
		out, err := stdoutOf(t, func() error { return accessCmd(args) })
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	out := show()
	for _, want := range []string{
		"readiness: 4 of 4 open\n",
		"  [ ] tasks on\n      aimem tasks on -p example\n",
		"  [ ] process selected\n      aimem process select <repo> <tag> [<manifest>] -p example",
		"  [ ] a user or group granted\n      aimem access grant add --project example --user-name <user>\n",
		"  [ ] a live token that can write\n      aimem access token-issue-user --user-name <user> --label <user> --expires +90d --output FILE\n",
		"--project example --expires +90d --output FILE\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("nothing set: missing %q in\n%s", want, out)
		}
	}

	if out, err := stdoutOf(t, func() error { return tasksCmd([]string{"on", "-p", "example"}) }); err != nil || !strings.Contains(out, "readiness: 3 of 4 open\n  [x] tasks on\n") {
		t.Fatalf("tasks on: %v\n%s", err, out)
	}
	commit := strings.Repeat("ab", 20)
	archive, _ := exportFixture(t, "https://example.com/process.git", commit, "process/manifest.json")
	if _, err := stdoutOf(t, func() error {
		return processCmd([]string{"select", "https://example.com/process.git", commit, "process/manifest.json", "--commit", "--ref", "v1", "--from-archive", archive, "-p", "example"})
	}); err != nil {
		t.Fatal(err)
	}
	access("user-add", "--user-name", "pilot-worker")
	access("grant", "add", "--project", "example", "--user-name", "pilot-worker")
	out = show()
	// The granted user is filled into the token command.
	if !strings.Contains(out, "readiness: 1 of 4 open\n") || !strings.Contains(out, "  [x] process selected: process (v1)\n") ||
		!strings.Contains(out, "  [x] granted: pilot-worker\n") || !strings.Contains(out, "token-issue-user --user-name pilot-worker --label pilot-worker --expires +90d") {
		t.Fatalf("granted, no token:\n%s", out)
	}

	access("token-issue-user", "--user-name", "pilot-worker", "--label", "worker-laptop", "--expires", "+30d", "--output", filepath.Join(t.TempDir(), "w.token"))
	out = show()
	if !strings.Contains(out, "readiness: ready for work\n") || !strings.Contains(out, "  [x] a live token that can write: worker-laptop (pilot-worker, user, expires ") || strings.Contains(out, "[ ]") {
		t.Fatalf("ready:\n%s", out)
	}
	// A project token whose user loses the grant no longer counts, even
	// with another user still granted: the hub refuses its writes.
	access("user-add", "--user-name", "second")
	access("grant", "add", "--project", "example", "--user-name", "second")
	access("token-issue", "--user-name", "pilot-worker", "--label", "example-only", "--project", "example", "--expires", "+30d", "--output", filepath.Join(t.TempDir(), "p.token"))
	access("grant", "rm", "--project", "example", "--user-name", "pilot-worker")
	if out = show(); !strings.Contains(out, "readiness: 1 of 4 open\n") || !strings.Contains(out, "  [ ] a live token that can write\n") || strings.Contains(out, "example-only (") {
		t.Fatalf("grant removed, project token left:\n%s", out)
	}
	if table := access("list", "--project", "example"); strings.Contains(table, "example-only") {
		t.Fatalf("the table lists a token whose user has no grant:\n%s", table)
	}
	if table := access("list", "--project", "example", "--all"); !strings.Contains(table, "example-only [no grant]") {
		t.Fatalf("--all does not mark the ungranted token:\n%s", table)
	}
	access("grant", "add", "--project", "example", "--user-name", "pilot-worker")
	// A disabled user's token no longer counts.
	var snap accessSnap
	json.Unmarshal([]byte(access("list", "--json")), &snap)
	access("user-set", "--user-id", snap.Users[0].ID, "--user-name", "pilot-worker", "--state", "disabled")
	if out = show(); !strings.Contains(out, "readiness: 1 of 4 open\n") || !strings.Contains(out, "  [ ] a live token that can write\n") {
		t.Fatalf("user disabled:\n%s", out)
	}
}

// access list is a table: disabled users and revoked tokens only with
// --all, --project and --user filter it, and --json is the hub's whole
// document as before.
func TestAccessListTable(t *testing.T) {
	reg := projectService(t)
	for _, p := range []string{"alpha", "beta"} {
		if _, err := reg.Open(p); err != nil {
			t.Fatal(err)
		}
	}
	access := func(args ...string) string {
		out, err := stdoutOf(t, func() error { return accessCmd(args) })
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	dir := t.TempDir()
	access("user-add", "--user-name", "alice")
	access("user-add", "--user-name", "bob")
	access("group-add", "--group-name", "reviewers")
	access("member", "add", "--group-name", "reviewers", "--user-name", "bob")
	access("grant", "add", "--project", "alpha", "--user-name", "alice")
	access("grant", "add", "--project", "beta", "--group-name", "reviewers")
	issued := access("token-issue", "--user-name", "alice", "--label", "alpha-laptop", "--project", "alpha", "--expires", "+90d", "--output", filepath.Join(dir, "a"))
	if !strings.Contains(issued, `"expires_at": "`+time.Now().UTC().Add(90*24*time.Hour).Format("2006-01-02")) {
		t.Fatalf("+90d not resolved in the issue output:\n%s", issued)
	}
	access("token-issue-user", "--user-name", "bob", "--label", "bob-all", "--expires", time.Now().UTC().Add(200*24*time.Hour).Format("2006-01-02"), "--output", filepath.Join(dir, "b"))
	gone := access("token-issue-user", "--user-name", "alice", "--label", "old-one", "--expires", "+5d", "--output", filepath.Join(dir, "c"))
	revoked := between(gone, `"id": "`, `"`)
	access("token-revoke", "--token-id", revoked)
	var snap accessSnap
	json.Unmarshal([]byte(access("list", "--json")), &snap)
	for _, u := range snap.Users {
		if u.Name == "bob" {
			access("user-add", "--user-name", "carol")
			access("user-set", "--user-id", u.ID, "--user-name", "bob", "--state", "disabled")
		}
	}

	out := access("list")
	for _, want := range []string{
		"PROJECT  GRANTED",
		"alpha    alice",
		"alpha-laptop",
		"beta     group reviewers  (none that can write)",
		"carol",
		"--all shows them",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("table: missing %q in\n%s", want, out)
		}
	}
	for _, hidden := range []string{"old-one", "bob-all", "bob "} {
		if strings.Contains(out, hidden) {
			t.Fatalf("table shows %q without --all:\n%s", hidden, out)
		}
	}
	all := access("list", "--all")
	for _, want := range []string{"old-one [revoked]", "bob-all [user disabled]", "bob    disabled"} {
		if !strings.Contains(all, want) {
			t.Fatalf("--all: missing %q in\n%s", want, all)
		}
	}
	if p := access("list", "--project", "alpha"); !strings.Contains(p, "alpha-laptop") || strings.Contains(p, "beta") || strings.Contains(p, "STATE") {
		t.Fatalf("--project alpha:\n%s", p)
	}
	if u := access("list", "--user", "alice"); !strings.Contains(u, "alpha-laptop") || strings.Contains(u, "beta ") || strings.Contains(u, "carol") {
		t.Fatalf("--user alice:\n%s", u)
	}
	if _, err := stdoutOf(t, func() error { return accessCmd([]string{"list", "--user", "nobody"}) }); err == nil || !strings.Contains(err.Error(), "the users are: alice, bob, carol") {
		t.Fatalf("--user nobody: %v", err)
	}

	// --json is byte-equal to the document as it was always printed.
	data, err := accessDo("GET", "/v1/access", nil)
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	json.Indent(&pretty, data, "", "  ")
	if got := access("list", "--json"); got != pretty.String()+"\n" {
		t.Fatalf("--json differs:\n%s\nwant:\n%s", got, pretty.String())
	}
	if _, err := stdoutOf(t, func() error { return accessCmd([]string{"list", "--json", "--all"}) }); err == nil {
		t.Fatal("--json took a filter")
	}

	// The audit, filtered on the hub.
	if a := access("audit", "--action", "token.revoke"); !strings.Contains(a, "AT ") || !strings.Contains(a, "token.revoke  "+revoked) || strings.Contains(a, "user.create") {
		t.Fatalf("audit --action:\n%s", a)
	}
	if a := access("audit", "--since", "2099-01-01"); a != "no audit events match\n" {
		t.Fatalf("audit --since in the future:\n%s", a)
	}
	if a := access("audit", "--since", "1d", "--limit", "2"); strings.Count(a, "\n") != 3 {
		t.Fatalf("audit --limit 2:\n%s", a)
	}
	if _, err := stdoutOf(t, func() error { return accessCmd([]string{"audit", "--since", "soon"}) }); err == nil {
		t.Fatal("audit --since soon accepted")
	}
}

// An expired token reads as expired in the table and never counts as
// one that can write.
func TestAccessViewExpiredToken(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	var snap accessSnap
	if err := json.Unmarshal([]byte(`{
		"users": [{"id": "u1", "name": "alice", "disabled": false}],
		"groups": [], "members": [],
		"grants": [{"project_instance": "i1", "kind": "user", "subject": "u1"}],
		"tokens": [{"id": "t1", "user_id": "u1", "label": "stale", "scope": "user", "expires_at": "2026-10-01T00:00:00Z", "revoked": false}],
		"projects": {"i1": "alpha"}}`), &snap); err != nil {
		t.Fatal(err)
	}
	v := newAccessView(snap, now)
	if got := v.writeTokens("i1", false); len(got) != 0 {
		t.Fatalf("an expired token can write: %v", got)
	}
	var b bytes.Buffer
	if err := renderAccess(&b, v, "", "", true); err != nil || !strings.Contains(b.String(), "stale [expired]") {
		t.Fatalf("--all: %v\n%s", err, b.String())
	}
	if list := readinessChecklist(readinessInput{project: "alpha", tasksOn: true, process: "p (v1)", instance: "i1", view: v}); !strings.Contains(list, "readiness: 1 of 4 open") || !strings.Contains(list, "--user-name alice --label alice") {
		t.Fatalf("checklist:\n%s", list)
	}
}

// A project granted only to a team has its writer: the team writes with
// its peer credential. A granted user, alone or beside a team, still
// needs a live token of their own; with neither, the item stays open.
func TestReadinessTeamWriter(t *testing.T) {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	view := func(grants string) *accessView {
		var snap accessSnap
		if err := json.Unmarshal([]byte(`{
			"users": [{"id": "u1", "name": "alice", "disabled": false}, {"id": "u2", "name": "bob", "disabled": true}],
			"groups": [], "members": [],
			"grants": [`+grants+`],
			"tokens": [], "projects": {"i1": "alpha"}}`), &snap); err != nil {
			t.Fatal(err)
		}
		return newAccessView(snap, now)
	}
	check := func(name string, in readinessInput, want ...string) {
		t.Helper()
		in.project, in.tasksOn, in.process = "alpha", true, "p (v1)"
		list := readinessChecklist(in)
		for _, w := range want {
			if !strings.Contains(list, w) {
				t.Errorf("%s: missing %q in\n%s", name, w, list)
			}
		}
	}
	teams := []string{"pilot", "aicrew-example/0191"}
	check("teams only", readinessInput{view: view(""), teams: teams},
		"readiness: ready for work\n",
		"  [x] a writer: the team writes with its peer credential (team pilot, team aicrew-example/0191)\n")
	check("a disabled user and a team", readinessInput{instance: "i1", view: view(`{"project_instance": "i1", "kind": "user", "subject": "u2"}`), teams: teams[:1]},
		"readiness: ready for work\n",
		"  [x] a writer: the team writes with its peer credential (team pilot)\n")
	check("a user and a team", readinessInput{instance: "i1", view: view(`{"project_instance": "i1", "kind": "user", "subject": "u1"}`), teams: teams},
		"readiness: 1 of 4 open\n",
		"  [x] granted: alice, team pilot, team aicrew-example/0191\n",
		"  [ ] a live token that can write\n      aimem access token-issue-user --user-name alice --label alice")
	check("neither", readinessInput{view: view("")},
		"readiness: 2 of 4 open\n",
		"  [ ] a live token that can write\n      aimem access token-issue-user --user-name <user> --label <user>")
}
