package main

// The readable command vocabulary (docs/DESIGN-AIFORGE-PILOT-1.md §5, §6):
// named entity flags, the old positional forms with a one-release notice,
// and --output <file|-> for every issued secret.

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// captureNotices collects the one-release notices for the test's duration.
func captureNotices(t *testing.T) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	saved := noticeOut
	noticeOut = &b
	t.Cleanup(func() { noticeOut = saved })
	return &b
}

func TestIdentityCLINamedForms(t *testing.T) {
	notices := captureNotices(t)
	g := newIdentityCLIRig(t, nil)
	g.mustRun(t, "peer", "register", "--peer", "aicrew-example", "--endpoint", "https://aicrew.example/v1/crew/introspect", "--peer-trust-dns")
	if _, err := g.reg.Open("alpha"); err != nil {
		t.Fatal(err)
	}
	if out := g.mustRun(t, "team", "create", "--peer", "aicrew-example", "--team-id", "team-1"); !strings.Contains(out, "team profile team-1 created") {
		t.Fatalf("create: %s", out)
	}
	if !strings.Contains(notices.String(), "team create is kept for this release") {
		t.Fatalf("team create notice: %q", notices)
	}
	notices.Reset()
	if out := g.mustRun(t, "team", "grant", "--peer", "aicrew-example", "--team-id", "team-1", "--project", "alpha"); !strings.Contains(out, "project alpha (instance ") {
		t.Fatalf("grant: %s", out)
	}
	if out := g.mustRun(t, "team", "revoke", "--peer", "aicrew-example", "--team-id", "team-1", "-p", "alpha"); !strings.Contains(out, "project alpha revoked") {
		t.Fatalf("revoke: %s", out)
	}
	// One entity keeps its one positional, without a notice.
	g.mustRun(t, "cred", "list", "aicrew-example")
	g.mustRun(t, "peer", "enable", "--peer", "aicrew-example")
	if notices.Len() != 0 {
		t.Fatalf("a named or single-positional form printed a notice: %s", notices)
	}
	// The positional form of several entities works and names its new form.
	g.mustRun(t, "team", "grant", "aicrew-example", "team-1", "alpha")
	if want := "use: aimem identity team grant --peer aicrew-example --team-id team-1 --project alpha [hub flags]"; !strings.Contains(notices.String(), want) {
		t.Fatalf("notice %q, want %q", notices, want)
	}
	before := g.requests.Load()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"team", "grant", "aicrew-example", "--peer", "aicrew-example", "--team-id", "team-1", "--project", "alpha"}, "given twice"},
		{[]string{"team", "grant", "--peer", "aicrew-example", "--project", "alpha"}, "needs --team-id"},
		{[]string{"cred", "list", "--peer", "aicrew-example", "--team-id", "team-1"}, "--team-id does not apply"},
		{[]string{"cred", "revoke", "--peer", "aicrew-example"}, "needs --credential"},
		{[]string{"cred", "issue", "--peer", "aicrew-example", "--expires", "30d"}, "needs --expires and --output"},
		{[]string{"cred", "issue", "--peer", "aicrew-example", "--expires", "30d", "--output", g.secretPath("a"), "--secret-file", g.secretPath("b")}, "old name of --output"},
	} {
		if _, err := g.run(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
	if g.requests.Load() != before {
		t.Fatal("a malformed command reached the hub")
	}
	g.assertNoSecrets(t)
}

// --output FILE writes the bearer once to a new owner-only file; --output -
// writes it to a pipe and every other line to standard error, and is
// refused before anything is issued when standard output is not a pipe or
// file but a character device (a terminal; the null device stands in).
func TestIdentityCLIOutput(t *testing.T) {
	notices := captureNotices(t)
	g := newIdentityCLIRig(t, nil)
	g.register(t)
	out := g.mustRun(t, "cred", "issue", "--peer", "aicrew-example", "--expires", "30d", "--output", g.secretPath("one.secret"))
	if raw, err := os.ReadFile(g.secretPath("one.secret")); err != nil || !strings.HasPrefix(string(raw), "aimem_peer_") || !strings.Contains(out, "written once to "+g.secretPath("one.secret")) {
		t.Fatalf("file output: %v %q", err, out)
	}
	if _, err := g.run(t, "cred", "issue", "--peer", "aicrew-example", "--expires", "30d", "--output", g.secretPath("one.secret")); err == nil || !strings.Contains(err.Error(), "never overwritten") {
		t.Fatalf("existing file: %v", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := secretStdout
	secretStdout = w
	defer func() { secretStdout = saved }()
	out = g.mustRun(t, "cred", "issue", "--peer", "aicrew-example", "--operation", "reservation.read", "--expires", "30d", "--output", "-")
	w.Close()
	piped, _ := io.ReadAll(r)
	if !strings.HasPrefix(string(piped), "aimem_peer_") || strings.Count(string(piped), "\n") != 1 {
		t.Fatalf("pipe got %d bytes", len(piped))
	}
	if out != "" || !strings.Contains(notices.String(), "written once to standard output") {
		t.Fatalf("with --output - the report goes to standard error: stdout %q, stderr %q", out, notices)
	}

	dev, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer dev.Close()
	secretStdout = dev
	n := len(g.creds(t))
	if _, err := g.run(t, "cred", "issue", "--peer", "aicrew-example", "--operation", "reservation.read", "--expires", "30d", "--output", "-"); err == nil || !strings.Contains(err.Error(), "standard output is a terminal") {
		t.Fatalf("terminal: %v", err)
	}
	if len(g.creds(t)) != n {
		t.Fatal("a credential was issued for a refused --output -")
	}
	// --secret-file keeps working for this release, with a notice.
	notices.Reset()
	g.mustRun(t, "cred", "rotate", "aicrew-example", "--expires", "30d", "--secret-file", g.secretPath("two.secret"))
	if !strings.Contains(notices.String(), "--output in place of --secret-file") {
		t.Fatalf("notice: %q", notices)
	}
	g.assertNoSecrets(t)
}

// The access token secret goes to --output only, never into the printed
// answer; the old positional form still prints it, with a notice.
func TestAccessTokenOutput(t *testing.T) {
	reg := projectService(t)
	if _, err := reg.Open("example"); err != nil {
		t.Fatal(err)
	}
	notices := captureNotices(t)
	run := func(args ...string) (string, error) {
		return stdoutOf(t, func() error { return accessCmd(args) })
	}
	if _, err := run("user-add", "--user-name", "pilot-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := run("grant", "add", "--project", "example", "--user-name", "pilot-worker"); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	dir := t.TempDir()
	file := filepath.Join(dir, "worker.token")
	out, err := run("token-issue-user", "--user-name", "pilot-worker", "--label", "worker", "--expires", expiry, "--output", file)
	raw, rerr := os.ReadFile(file)
	if err != nil || rerr != nil || !strings.HasPrefix(string(raw), "aimem_user_") || strings.Contains(out, "aimem_user_") || strings.Contains(out, `"secret"`) || !strings.Contains(out, `"scope": "user"`) {
		t.Fatalf("file output: %v %v %q", err, rerr, out)
	}
	if notices.Len() != 0 {
		t.Fatalf("named form printed a notice: %s", notices)
	}
	// An existing file is refused before anything is issued.
	before, _ := run("list")
	if _, err := run("token-issue-user", "--user-name", "pilot-worker", "--label", "again", "--expires", expiry, "--output", file); err == nil || !strings.Contains(err.Error(), "never overwritten") {
		t.Fatalf("existing file: %v", err)
	}
	if after, _ := run("list"); strings.Count(after, `"label"`) != strings.Count(before, `"label"`) {
		t.Fatal("a token was issued for a refused --output")
	}
	// Unknown names are refused with the names that exist.
	if _, err := run("grant", "add", "--project", "example", "--user-name", "nobody"); err == nil || !strings.Contains(err.Error(), "the users are: pilot-worker") {
		t.Fatalf("unknown name: %v", err)
	}
	// The old form prints the secret as before, and the new form to use.
	notices.Reset()
	list, _ := run("list")
	id := between(list, `"id": "`, `"`)
	out, err = run("token-issue-user", id, "legacy", expiry)
	if err != nil || !strings.Contains(out, "aimem_user_") || !strings.Contains(notices.String(), "use: aimem access token-issue-user --user-id "+id+" --label legacy --expires "+expiry+" --output FILE") {
		t.Fatalf("legacy: %v %q %q", err, out, notices)
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return s
}

// Teams that aicrewd registered by name are named by that name within the
// peer; every grant and revoke prints the name and the team ID it acted on.
func TestIdentityCLITeamNames(t *testing.T) {
	captureNotices(t)
	g := newIdentityCLIRig(t, nil)
	g.register(t)
	if _, err := g.reg.Open("alpha"); err != nil {
		t.Fatal(err)
	}
	// aicrewd's side: a team.register credential issued by the CLI, used on
	// the wire route.
	g.mustRun(t, "cred", "issue", "--peer", "aicrew-example", "--operation", "team.register", "--expires", "30d", "--output", g.secretPath("register.secret"))
	raw, err := os.ReadFile(g.secretPath("register.secret"))
	if err != nil {
		t.Fatal(err)
	}
	bearer := strings.TrimSpace(string(raw))
	const team = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	req, _ := http.NewRequest("PUT", g.ts.URL+"/v1/identity/peers/aicrew-example/team-registrations/"+team, strings.NewReader(`{"team_name":"pilot"}`))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("X-Aimem-Identity-Version", "1")
	resp, err := g.ts.Client().Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("register: %v %v", err, resp)
	}
	resp.Body.Close()

	out := g.mustRun(t, "team", "grant", "--peer", "aicrew-example", "--team-name", "pilot", "--project", "alpha")
	if !strings.Contains(out, "granted to team pilot ("+team+") of aicrew-example") {
		t.Fatalf("grant by name: %s", out)
	}
	if out := g.mustRun(t, "team", "list", "aicrew-example"); !strings.Contains(out, "pilot  "+team+"  enabled") {
		t.Fatalf("list: %s", out)
	}
	if out := g.mustRun(t, "team", "grants", "--peer", "aicrew-example", "--team-id", team); !strings.Contains(out, "pilot  "+team) || !strings.Contains(out, "grant alpha") {
		t.Fatalf("grants by ID: %s", out)
	}
	if out := g.mustRun(t, "team", "revoke", "--peer", "aicrew-example", "--team-name", "pilot", "--project", "alpha"); !strings.Contains(out, "revoked from team pilot ("+team+")") {
		t.Fatalf("revoke by name: %s", out)
	}
	before := g.requests.Load()
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"team", "grant", "--peer", "aicrew-example", "--team-name", "pilot", "--team-id", team, "--project", "alpha"}, "exactly one of --team-id or --team-name"},
		{[]string{"team", "create", "--peer", "aicrew-example", "--team-name", "pilot"}, "team create takes --team-id only"},
		{[]string{"cred", "list", "--peer", "aicrew-example", "--team-name", "pilot"}, "--team-name does not apply"},
		{[]string{"cred", "issue", "--peer", "aicrew-example", "--operation", "team.write", "--expires", "30d", "--output", g.secretPath("x")}, "--operation must be one of"},
	} {
		if _, err := g.run(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: %v, want %q", tc.args, err, tc.want)
		}
	}
	if g.requests.Load() != before {
		t.Fatal("a malformed command reached the hub")
	}
	if _, err := g.run(t, "team", "grant", "--peer", "aicrew-example", "--team-name", "nobody", "--project", "alpha"); err == nil || !strings.Contains(err.Error(), "unknown team name \"nobody\"; the teams of aicrew-example are: pilot") {
		t.Fatalf("unknown name: %v", err)
	}
	g.assertNoSecrets(t)
}

// Every legacy form's notice names a new form that runs: parsed by the
// operator shell and run without hub flags, it passes the command's own
// argument checks and stops only at the missing --hub. Before the fix the
// team revoke --instance notice dropped --instance and failed the
// exactly-one-of-project-or-instance check.
func TestIdentityLegacyNoticesRun(t *testing.T) {
	notices := captureNotices(t)
	const team = "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"
	for _, legacy := range [][]string{
		{"team", "revoke", "aicrew-example", team, "--instance", "01a0e1f2-0000-7000-8000-000000000002"},
		{"team", "revoke", "aicrew-example", team, "example"},
		{"team", "revoke", "aicrew-example", team, "--project", "example"},
		{"team", "grant", "aicrew-example", team, "example"},
		{"team", "grants", "aicrew-example", team},
		{"team", "enable", "aicrew-example", team},
		{"team", "disable", "aicrew-example", team},
		{"cred", "revoke", "aicrew-example", "01a0e1f2-0000-7000-8000-000000000004"},
		// A stray boolean flag survives the round trip as one argument.
		{"team", "grants", "aicrew-example", team, "--peer-trust-dns"},
	} {
		notices.Reset()
		if err := runIdentity(legacy, io.Discard); err == nil || !strings.Contains(err.Error(), "--hub must be") {
			t.Fatalf("%v: %v", legacy, err)
		}
		line := strings.TrimSpace(notices.String())
		cmd, ok := strings.CutPrefix(line, "aimem: this form is kept for one release; use: ")
		if !ok {
			t.Fatalf("%v: notice %q", legacy, line)
		}
		cmd = strings.TrimSuffix(cmd, " [hub flags]")
		args := shellParse(t, cmd)
		if strings.Count(cmd, "--instance") != strings.Count(strings.Join(legacy, " "), "--instance") {
			t.Fatalf("%v: the notice changed --instance: %s", legacy, cmd)
		}
		notices.Reset()
		if err := runIdentity(args[2:], io.Discard); err == nil || !strings.Contains(err.Error(), "--hub must be") {
			t.Fatalf("the printed new form %q does not pass its own checks: %v", cmd, err)
		}
		if notices.Len() != 0 {
			t.Fatalf("the printed new form %q is itself a legacy form: %s", cmd, notices)
		}
	}
}
