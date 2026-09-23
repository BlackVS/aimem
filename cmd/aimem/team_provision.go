package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"aimem/internal/access"
	"aimem/internal/store"
	"aimem/internal/uuidv7"
)

const teamProvisionUsage = `usage: aimem teams provision create PROJECT --team NAME --coordinator USER --expiry RFC3339 [flags]
       aimem teams provision add    PROJECT --team NAME|ID --member USER --role worker|coordinator --expiry RFC3339 [flags]

Run on the hub host as its local operator (the service socket): the guided
path for the team an agent joins with /join_team. create makes the team
with its first coordinator; add enrolls one more member (or grants an
enrolled one the coordinator flag). Both resolve USER by exact name or ID,
grant the project, enroll, and issue that member one project-scoped token
labelled team-<team>-<user>, whose secret is shown once, at the end, on its
own line (or written to --secret-file, mode 0600, and not shown). Every
step is idempotent and reported: an existing user, grant, team or
enrollment is reused, never duplicated; a live token with the same label
is never reissued (choose another --label to issue a new one). A rerun
after a partial failure repeats the idempotent steps and says what already
existed. Creating an enrollment creates no session: the member joins with
/join_team under its own token.

  --create-user        create USER when no user of that exact name exists
  --label L            token label (default team-<team>-<user>)
  --no-token           enroll without issuing a token (the member has one)
  --secret-file PATH   write the secret there (new file, 0600) instead of stdout
  --description TEXT   team description (create only)
Ordinary member tokens cannot run this; agents get the operator handoff
from aimem teams setup instead.`

// operatorCall performs one request against the hub's local operator API.
type operatorCall func(method, path string, body any) (int, []byte, error)

func socketOperator(method, path string, body any) (int, []byte, error) {
	var raw []byte
	if body != nil {
		var err error
		if raw, err = json.Marshal(body); err != nil {
			return 0, nil, err
		}
	}
	req, err := http.NewRequest(method, "http://aimem"+path, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", uuidv7.New())
	}
	resp, err := client().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%w (run this on the hub host with its local aimem service running)", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, data, err
}

func teamProvisionCmd(args []string) error {
	return runTeamProvision(args, socketOperator, os.Stdout)
}

type provisionOptions struct {
	verb, project, team, user, role, label, secretFile, description string
	expiry                                                          time.Time
	createUser, noToken                                             bool
}

func parseTeamProvisionArgs(args []string) (*provisionOptions, error) {
	if len(args) < 2 || (args[0] != "create" && args[0] != "add") || strings.HasPrefix(args[1], "-") {
		return nil, errors.New(teamProvisionUsage)
	}
	o := &provisionOptions{verb: args[0], project: args[1]}
	fs := flag.NewFlagSet("teams provision", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var expiry string
	fs.StringVar(&o.team, "team", "", "")
	fs.StringVar(&o.user, "coordinator", "", "")
	fs.StringVar(&o.user, "member", "", "")
	fs.StringVar(&o.role, "role", "", "")
	fs.StringVar(&expiry, "expiry", "", "")
	fs.StringVar(&o.label, "label", "", "")
	fs.StringVar(&o.secretFile, "secret-file", "", "")
	fs.StringVar(&o.description, "description", "", "")
	fs.BoolVar(&o.createUser, "create-user", false, "")
	fs.BoolVar(&o.noToken, "no-token", false, "")
	if err := fs.Parse(args[2:]); err != nil || fs.NArg() != 0 {
		return nil, errors.New(teamProvisionUsage)
	}
	if o.verb == "create" {
		o.role = "coordinator"
	}
	if o.team == "" || o.user == "" || (o.role != "worker" && o.role != "coordinator") {
		return nil, errors.New(teamProvisionUsage)
	}
	if !o.noToken {
		if expiry == "" {
			return nil, errors.New("--expiry RFC3339 is required when a token is issued (or pass --no-token)")
		}
		t, err := time.Parse(time.RFC3339, expiry)
		if err != nil {
			return nil, fmt.Errorf("expiry must be RFC3339: %w", err)
		}
		if !t.After(time.Now()) {
			return nil, errors.New("expiry must be in the future")
		}
		o.expiry = t
	}
	if o.secretFile != "" {
		if _, err := os.Lstat(o.secretFile); err == nil {
			return nil, errors.New("--secret-file must name a new file; an existing one is never overwritten")
		}
	}
	return o, nil
}

func runTeamProvision(args []string, call operatorCall, stdout io.Writer) error {
	o, err := parseTeamProvisionArgs(args)
	if err != nil {
		return err
	}
	p := &provisioner{o: o, call: call, out: stdout}
	if err := p.run(); err != nil {
		if len(p.done) > 0 {
			fmt.Fprintln(stdout, "stopped after: "+strings.Join(p.done, "; ")+". Re-running repeats the idempotent steps and reports what already exists.")
		}
		return err
	}
	return nil
}

type provisioner struct {
	o    *provisionOptions
	call operatorCall
	out  io.Writer
	done []string
}

func (p *provisioner) say(format string, a ...any) {
	line := fmt.Sprintf(format, a...)
	p.done = append(p.done, line)
	fmt.Fprintln(p.out, "  "+line)
}

func (p *provisioner) hubError(status int, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	json.Unmarshal(body, &e)
	if status == http.StatusForbidden || status == http.StatusUnauthorized {
		return fmt.Errorf("HTTP %d: %s; provisioning needs the hub's local operator authority, never a member token", status, e.Error)
	}
	if e.Error != "" {
		return fmt.Errorf("HTTP %d: %s", status, e.Error)
	}
	return fmt.Errorf("HTTP %d", status)
}

func (p *provisioner) run() error {
	o := p.o
	fmt.Fprintf(p.out, "aimem teams provision %s: project %s, team %q, %s %s\n", o.verb, o.project, o.team, o.role, o.user)
	status, body, err := p.call(http.MethodGet, "/v1/access", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return p.hubError(status, body)
	}
	var snap access.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		return errors.New("unexpected access listing")
	}
	user, err := p.resolveUser(snap)
	if err != nil {
		return err
	}
	if err := p.grant(user); err != nil {
		return err
	}
	team, err := p.enroll(user)
	if err != nil {
		return err
	}
	if o.noToken {
		p.say("token: skipped (--no-token); the member installs its existing token with `aimem task-token set`")
		return nil
	}
	return p.token(snap, user, team)
}

// resolveUser finds USER by ID, then by exact name; two users of one exact
// name are an ambiguity the operator resolves by ID.
func (p *provisioner) resolveUser(snap access.Snapshot) (access.User, error) {
	var byName []access.User
	for _, u := range snap.Users {
		if u.ID == p.o.user {
			p.say("user %s: existing (id %s)", u.Name, u.ID)
			return u, nil
		}
		if u.Name == p.o.user {
			byName = append(byName, u)
		}
	}
	switch len(byName) {
	case 1:
		if byName[0].Disabled {
			return access.User{}, fmt.Errorf("user %s (id %s) is disabled; enable it first with aimem access user-set", byName[0].Name, byName[0].ID)
		}
		p.say("user %s: existing (id %s)", byName[0].Name, byName[0].ID)
		return byName[0], nil
	case 0:
	default:
		var ids []string
		for _, u := range byName {
			ids = append(ids, u.ID)
		}
		return access.User{}, fmt.Errorf("user name %q is ambiguous (ids %s); pass the ID", p.o.user, strings.Join(ids, ", "))
	}
	if !p.o.createUser {
		return access.User{}, fmt.Errorf("no user named %q; pass --create-user to create it, or the ID of an existing user", p.o.user)
	}
	status, body, err := p.call(http.MethodPost, "/v1/access/users", map[string]any{"name": p.o.user})
	if err != nil {
		return access.User{}, err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return access.User{}, p.hubError(status, body)
	}
	var u access.User
	if json.Unmarshal(body, &u) != nil || u.ID == "" {
		return access.User{}, errors.New("unexpected user creation reply")
	}
	p.say("user %s: created (id %s)", u.Name, u.ID)
	return u, nil
}

func (p *provisioner) grant(u access.User) error {
	status, body, err := p.call(http.MethodPut, "/v1/projects/"+url.PathEscape(p.o.project)+"/access/user/"+url.PathEscape(u.ID), nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return p.hubError(status, body)
	}
	p.say("grant on project %s: set for user %s (idempotent)", p.o.project, u.Name)
	return nil
}

// findTeam pages the admin listing for the team by ID or exact name.
func (p *provisioner) findTeam() (*store.Team, error) {
	after := ""
	for page := 0; page < 100; page++ {
		path := "/v1/projects/" + url.PathEscape(p.o.project) + "/teams?limit=100"
		if after != "" {
			path += "&after=" + url.QueryEscape(after)
		}
		status, body, err := p.call(http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, p.hubError(status, body)
		}
		var res struct {
			Teams      []store.Team `json:"teams"`
			NextCursor string       `json:"next_cursor"`
		}
		if json.Unmarshal(body, &res) != nil {
			return nil, errors.New("unexpected team listing")
		}
		for i := range res.Teams {
			if res.Teams[i].ID == p.o.team || res.Teams[i].Name == p.o.team {
				t := res.Teams[i]
				return &t, nil
			}
		}
		if res.NextCursor == "" {
			return nil, nil
		}
		after = res.NextCursor
	}
	return nil, errors.New("team listing did not end")
}

// enroll creates the team with the member (create) or adds the member to
// an existing team's enrollment at the read revision, once more on a
// revision conflict. An existing enrollment is reused; a worker request
// never removes an existing coordinator flag.
func (p *provisioner) enroll(u access.User) (*store.Team, error) {
	team, err := p.findTeam()
	if err != nil {
		return nil, err
	}
	wantCoordinator := p.o.role == "coordinator"
	if team == nil {
		if p.o.verb != "create" {
			return nil, fmt.Errorf("no team %q in project %s; create it with `aimem teams provision create`", p.o.team, p.o.project)
		}
		content := store.TeamContent{Name: p.o.team, Description: p.o.description, Enrollment: []store.TeamEnrollment{{UserID: u.ID, Coordinator: true}}}
		status, body, err := p.call(http.MethodPost, "/v1/projects/"+url.PathEscape(p.o.project)+"/teams", content)
		if err != nil {
			return nil, err
		}
		if status != http.StatusCreated && status != http.StatusOK {
			return nil, p.hubError(status, body)
		}
		var t store.Team
		if json.Unmarshal(body, &t) != nil || t.ID == "" {
			return nil, errors.New("unexpected team creation reply")
		}
		p.say("team %s: created (id %s, revision %d) with %s enrolled as coordinator", t.Name, t.ID, t.Revision, u.Name)
		return &t, nil
	}
	p.say("team %s: existing (id %s, revision %d)", team.Name, team.ID, team.Revision)
	for attempt := 0; attempt < 2; attempt++ {
		content := team.TeamContent
		changed := false
		found := false
		for i, e := range content.Enrollment {
			if e.UserID != u.ID {
				continue
			}
			found = true
			switch {
			case e.Coordinator == wantCoordinator:
				p.say("enrollment: %s already enrolled (coordinator: %t)", u.Name, e.Coordinator)
			case wantCoordinator:
				content.Enrollment[i].Coordinator = true
				changed = true
			default:
				p.say("enrollment: %s already enrolled as coordinator-eligible; a worker request does not remove that flag", u.Name)
			}
		}
		if !found {
			content.Enrollment = append(content.Enrollment, store.TeamEnrollment{UserID: u.ID, Coordinator: wantCoordinator})
			changed = true
		}
		if !changed {
			return team, nil
		}
		req := struct {
			store.TeamContent
			ExpectedRevision int64 `json:"expected_revision"`
		}{content, team.Revision}
		status, body, err := p.call(http.MethodPut, "/v1/projects/"+url.PathEscape(p.o.project)+"/teams/"+url.PathEscape(team.ID), req)
		if err != nil {
			return nil, err
		}
		if status == http.StatusConflict && attempt == 0 {
			p.say("enrollment: team revision moved; re-reading")
			if team, err = p.findTeam(); err != nil {
				return nil, err
			}
			if team == nil {
				return nil, fmt.Errorf("team %q disappeared while enrolling", p.o.team)
			}
			continue
		}
		if status != http.StatusOK {
			return nil, p.hubError(status, body)
		}
		var t store.Team
		if json.Unmarshal(body, &t) != nil || t.ID == "" {
			return nil, errors.New("unexpected team configuration reply")
		}
		if found {
			p.say("enrollment: %s now coordinator-eligible (revision %d)", u.Name, t.Revision)
		} else {
			p.say("enrollment: %s added as %s (revision %d)", u.Name, p.o.role, t.Revision)
		}
		return &t, nil
	}
	return nil, errors.New("enrollment did not settle after a retry; re-run")
}

// token issues the member's project-scoped token unless a live one with
// the same label exists; a lost secret is never replaced implicitly.
func (p *provisioner) token(snap access.Snapshot, u access.User, team *store.Team) error {
	label := p.o.label
	if label == "" {
		label = "team-" + team.Name + "-" + u.Name
	}
	for _, t := range snap.Tokens {
		if t.UserID == u.ID && t.Label == label && !t.Revoked && t.ExpiresAt.After(time.Now()) {
			p.say("token %s: exists for %s since before this run (id %s, expires %s); not reissued. Its secret was shown once when issued; to issue another, pass --label with a new name (and revoke the old one with aimem access token-revoke if it is lost)", label, u.Name, t.ID, t.ExpiresAt.UTC().Format(time.RFC3339))
			return nil
		}
	}
	status, body, err := p.call(http.MethodPost, "/v1/access/tokens", map[string]any{"user_id": u.ID, "label": label, "project": p.o.project, "expires_at": p.o.expiry})
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return p.hubError(status, body)
	}
	var res struct {
		Token  access.Token `json:"token"`
		Secret string       `json:"secret"`
	}
	if json.Unmarshal(body, &res) != nil || res.Secret == "" {
		return errors.New("unexpected token reply")
	}
	p.say("token %s: issued for %s (id %s, project-scoped, expires %s)", label, u.Name, res.Token.ID, res.Token.ExpiresAt.UTC().Format(time.RFC3339))
	install := "in the member's checkout: printf '%s' \"$SECRET\" | aimem task-token set   (then /join_team " + team.Name + " " + p.o.role + ")"
	if p.o.secretFile != "" {
		f, err := os.OpenFile(p.o.secretFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return fmt.Errorf("token issued but the secret could not be written to %s: %w; it is lost, revoke token %s and issue another with a new --label", p.o.secretFile, err, res.Token.ID)
		}
		if _, err := f.WriteString(res.Secret + "\n"); err != nil {
			f.Close()
			return fmt.Errorf("token issued but the secret could not be written to %s: %w; revoke token %s and issue another with a new --label", p.o.secretFile, err, res.Token.ID)
		}
		f.Close()
		fmt.Fprintf(p.out, "secret written once to %s (mode 0600); deliver it to the member and delete the file; install %s\n", p.o.secretFile, install)
		return nil
	}
	fmt.Fprintf(p.out, "one-time secret for %s (shown once, never again; install %s):\n%s\n", u.Name, install, res.Secret)
	return nil
}
