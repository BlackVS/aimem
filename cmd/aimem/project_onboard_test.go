package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aimem/internal/access"
	"aimem/internal/server"
	"aimem/internal/store"
)

// onboardState is what onboard changes on the hub for one project.
type onboardState struct {
	tasks            string
	commit, digest   string
	repo, ref        string
	direct           bool
	projectWriters   int // live project-scoped tokens of the user on the project
	tokens           int // all tokens on the hub
	writerExpiryDays int
}

func readOnboardState(t *testing.T, project, user string) onboardState {
	t.Helper()
	var st onboardState
	var meta struct {
		Value string `json:"value"`
	}
	if err := localJSON(http.MethodGet, "/v1/projects/"+project+"/meta/"+store.TasksMetaKey, nil, &meta); err != nil {
		t.Fatal(err)
	}
	st.tasks = meta.Value
	if sel, err := currentSelection(project); err != nil {
		t.Fatal(err)
	} else if sel != nil {
		st.commit, st.digest, st.repo, st.ref = sel.Commit, sel.Digest, sel.Repo, sel.Ref
	}
	data, err := accessDo("GET", "/v1/access", nil)
	if err != nil {
		t.Fatal(err)
	}
	var snap accessSnap
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatal(err)
	}
	v := newAccessView(snap, time.Now())
	uid := ""
	for _, u := range snap.Users {
		if u.Name == user {
			uid = u.ID
		}
	}
	inst := v.instance[project]
	st.direct = inst != "" && v.directGrant(inst, uid)
	st.tokens = len(snap.Tokens)
	for i, tk := range snap.Tokens {
		if tk.UserID == uid && tk.Scope == "project" && inst != "" && tk.Project == inst && v.liveToken(i) {
			st.projectWriters++
			st.writerExpiryDays = int(time.Until(tk.ExpiresAt).Hours()/24 + 0.5)
		}
	}
	return st
}

func TestProjectOnboard(t *testing.T) {
	reg := projectService(t)
	// bare and dflt are new to the hub: onboard creates them.
	for _, p := range []string{"fresh", "twin"} {
		if _, err := reg.Open(p); err != nil {
			t.Fatal(err)
		}
	}
	repo, _ := defaultRig(t)
	notices := captureNotices(t)
	run := func(args ...string) (string, error) {
		return stdoutOf(t, func() error { return projectNamespaceCmd(append([]string{"onboard"}, args...)) })
	}
	mustAccess := func(args ...string) {
		t.Helper()
		if out, err := stdoutOf(t, func() error { return accessCmd(args) }); err != nil {
			t.Fatalf("access %v: %v\n%s", args, err, out)
		}
	}
	mustAccess("user-add", "--user-name", "pilot-worker")
	dir := t.TempDir()
	file := func(name string) string { return filepath.Join(dir, name) }

	// Refusals change nothing.
	untouched := readOnboardState(t, "fresh", "pilot-worker")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"fresh", "--user", "nobody", "--output", file("x")}, "unknown user name \"nobody\"; the users are: pilot-worker"},
		{[]string{"fresh", "--user", "pilot-worker"}, "a token will be issued; name where its secret goes with --output"},
		{[]string{"fresh", "--user", "pilot-worker", "--expires", "soon", "--output", file("x")}, "expires"},
		{[]string{"fresh", "--user", "pilot-worker", "--process", repo, "v9", "--output", file("x")}, "no tag or branch \"v9\""},
	} {
		if out, err := run(tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: %v (want %q)\n%s", tc.args, err, tc.want, out)
		}
		if got := readOnboardState(t, "fresh", "pilot-worker"); got != untouched {
			t.Fatalf("%v changed the hub: %+v", tc.args, got)
		}
		if _, err := os.Stat(file("x")); err == nil {
			t.Fatalf("%v left a secret file", tc.args)
		}
	}

	// Onboard and the manual commands reach the same state.
	out, err := run("fresh", "--user", "pilot-worker", "--process", repo, "v1", "--output", file("fresh.token"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	secret, _ := os.ReadFile(file("fresh.token"))
	if len(strings.TrimSpace(string(secret))) < 20 || strings.Contains(out, strings.TrimSpace(string(secret))) {
		t.Fatalf("secret %d bytes; in output: %v", len(secret), strings.Contains(out, strings.TrimSpace(string(secret))))
	}
	for _, want := range []string{"  tasks: turned on\n", "  process: selected ", "  grant: granted user pilot-worker\n", "  token: issued ", "the secret was written once to " + file("fresh.token"), "readiness: ready for work\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if _, err := stdoutOf(t, func() error { return tasksCmd([]string{"on", "-p", "twin", "--no-process"}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := stdoutOf(t, func() error { return processCmd([]string{"select", repo, "v1", "-p", "twin"}) }); err != nil {
		t.Fatal(err)
	}
	mustAccess("grant", "add", "--project", "twin", "--user-name", "pilot-worker")
	mustAccess("token-issue", "--user-name", "pilot-worker", "--label", "twin", "--project", "twin", "--expires", "+90d", "--output", file("twin.token"))
	got, want := readOnboardState(t, "fresh", "pilot-worker"), readOnboardState(t, "twin", "pilot-worker")
	if got.tasks != "on" || got.commit == "" || got.digest == "" || !got.direct || got.projectWriters != 1 || got.writerExpiryDays != 90 {
		t.Fatalf("onboard state %+v", got)
	}
	if got.tasks != want.tasks || got.commit != want.commit || got.digest != want.digest || got.repo != want.repo || got.ref != want.ref ||
		got.direct != want.direct || got.projectWriters != want.projectWriters || got.writerExpiryDays != want.writerExpiryDays {
		t.Fatalf("onboard %+v, manual %+v", got, want)
	}

	// A second run changes nothing and issues nothing; it needs no --output.
	before := readOnboardState(t, "fresh", "pilot-worker")
	out, err = run("fresh", "--user", "pilot-worker", "--process", repo, "v1")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"  tasks: already on\n", "  process: already selected, ", "  grant: user pilot-worker already granted\n", "  token: none issued; user pilot-worker already has one that can write: fresh (project fresh, expires "} {
		if !strings.Contains(out, want) {
			t.Fatalf("rerun missing %q:\n%s", want, out)
		}
	}
	if after := readOnboardState(t, "fresh", "pilot-worker"); after != before {
		t.Fatalf("rerun changed %+v -> %+v", before, after)
	}
	// Another process is never selected over the current one.
	if _, err := run("fresh", "--user", "pilot-worker", "--process", repo, "v2"); err == nil || !strings.Contains(err.Error(), "--expect v1") {
		t.Fatalf("re-select: %v", err)
	}
	// --new-token issues another; with --output - the report goes to
	// standard error and only the secret to standard output.
	read := pipeSecretStdout(t)
	notices.Reset()
	out, err = run("fresh", "--user", "pilot-worker", "--new-token", "--label", "second", "--output", "-")
	piped := read()
	if err != nil || out != "" || len(strings.TrimSpace(piped)) < 20 || strings.Contains(notices.String(), strings.TrimSpace(piped)) || !strings.Contains(notices.String(), "  token: issued ") {
		t.Fatalf("new token: %v stdout %q notices %q", err, out, notices)
	}
	if after := readOnboardState(t, "fresh", "pilot-worker"); after.projectWriters != 2 || after.tokens != before.tokens+1 {
		t.Fatalf("new token: %+v", after)
	}

	// No --process and no default: the process step stays open, the rest
	// completes.
	out, err = run("bare", "--user", "pilot-worker", "--output", file("bare.token"))
	if err != nil || !strings.Contains(out, "  process: open, no --process and no hub default") || !strings.Contains(out, "readiness: 1 of 4 open\n") || !strings.Contains(out, "  token: issued ") {
		t.Fatalf("bare: %v\n%s", err, out)
	}
	if st := readOnboardState(t, "bare", "pilot-worker"); st.tasks != "on" || st.commit != "" || !st.direct || st.projectWriters != 1 {
		t.Fatalf("bare state %+v", st)
	}

	// With a hub default and no --process, the default is applied.
	if _, err := stdoutOf(t, func() error { return processCmd([]string{"default", "set", repo, "v2"}) }); err != nil {
		t.Fatal(err)
	}
	out, err = run("dflt", "--user", "pilot-worker", "--output", file("dflt.token"))
	if err != nil || !strings.Contains(out, "  process: applied the hub's default process: ") || !strings.Contains(out, "readiness: ready for work\n") {
		t.Fatalf("default: %v\n%s", err, out)
	}
}

func TestParseOnboard(t *testing.T) {
	a, err := parseOnboard([]string{"p", "--user", "u", "--process", "https://example.com/r.git", "v1", "--manifest", "m.json", "--new-token", "--output", "-"})
	if err != nil || a.project != "p" || a.user != "u" || a.repo != "https://example.com/r.git" || a.tag != "v1" || a.manifest != "m.json" || !a.newToken || a.output != "-" || a.label != "p" || a.expires != "+90d" {
		t.Fatalf("%+v %v", a, err)
	}
	for _, bad := range [][]string{
		{"--user", "u"},
		{"p", "q", "--user", "u"},
		{"p"},
		{"p", "--user", "u", "--process", "r"},
		{"p", "--user", "u", "--bogus", "x"},
	} {
		if _, err := parseOnboard(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	if _, err := parseOnboard([]string{"p", "--user", "u", "--manifest", "m"}); err == nil || !strings.Contains(err.Error(), "--manifest goes with --process") {
		t.Errorf("manifest alone: %v", err)
	}
}

// A team profile granted the project counts in the checklist's grant
// line; a disabled one does not. The route's answer is the real
// ProjectGrantee encoding.
func TestReadinessCountsTeamGrants(t *testing.T) {
	sockDir, err := os.MkdirTemp("", "atg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	t.Setenv("AIMEM_SOCKET", filepath.Join(sockDir, "s"))
	ln, err := net.Listen("unix", server.SocketPath(root))
	if err != nil {
		t.Fatal(err)
	}
	grants := []access.ProjectGrantee{
		{Kind: "user", ID: "u1", Name: "pilot-worker"},
		{Kind: "team_profile", ID: "t1", Name: "pilot", ServiceID: "aicrew-example", TeamID: "0190"},
		{Kind: "team_profile", ID: "t2", ServiceID: "aicrew-example", TeamID: "0191"},
		{Kind: "team_profile", ID: "t3", Name: "gone", Disabled: true},
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/alpha/grants" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"project": "alpha", "grants": grants})
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	teams := grantedTeams("alpha")
	if strings.Join(teams, ",") != "pilot,aicrew-example/0191" {
		t.Fatalf("teams %q", teams)
	}
	if grantedTeams("other") != nil {
		t.Fatal("an unreadable answer named teams")
	}
	list := readinessChecklist(readinessInput{project: "alpha", tasksOn: true, process: "p (v1)", view: newAccessView(accessSnap{}, time.Now()), teams: teams})
	if !strings.Contains(list, "  [x] granted: team pilot, team aicrew-example/0191\n") {
		t.Fatalf("checklist:\n%s", list)
	}
}
