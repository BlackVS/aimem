package main

// The readable views of the access state: `aimem access list` as a table,
// `aimem access audit`, the project readiness checklist that `tasks on`
// and `project show` print, and the date forms --expires and --since take.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"aimem/internal/store"
)

var daysRE = regexp.MustCompile(`^\+?([0-9]{1,5})d$`)

// parseExpiry reads an --expires value: an RFC 3339 time, a date
// (midnight UTC that day) or +Nd (N days from now).
func parseExpiry(v string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	if m := daysRE.FindStringSubmatch(v); m != nil && strings.HasPrefix(v, "+") {
		n, _ := strconv.Atoi(m[1])
		return now.UTC().Add(time.Duration(n) * 24 * time.Hour).Truncate(time.Second), nil
	}
	return time.Time{}, errors.New("--expires must be an RFC 3339 time (2026-12-28T00:00:00Z), a date (2026-12-28, midnight UTC) or a number of days from now (+90d)")
}

// parseSince reads a --since value: an RFC 3339 time, a date (midnight
// UTC) or Nd (N days ago).
func parseSince(v string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t, nil
	}
	if m := daysRE.FindStringSubmatch(v); m != nil && !strings.HasPrefix(v, "+") {
		n, _ := strconv.Atoi(m[1])
		return now.UTC().Add(-time.Duration(n) * 24 * time.Hour).Truncate(time.Second), nil
	}
	return time.Time{}, errors.New("--since must be an RFC 3339 time, a date (2026-10-01) or a number of days ago (7d)")
}

// accessSnap is the part of GET /v1/access the views read.
type accessSnap struct {
	Users []struct {
		ID, Name string
		Disabled bool
	} `json:"users"`
	Groups  []struct{ ID, Name string } `json:"groups"`
	Members []struct {
		GroupID string `json:"group_id"`
		UserID  string `json:"user_id"`
	} `json:"members"`
	Grants []struct {
		Project string `json:"project_instance"`
		Kind    string `json:"kind"`
		Subject string `json:"subject"`
	} `json:"grants"`
	Tokens []struct {
		ID        string    `json:"id"`
		UserID    string    `json:"user_id"`
		Label     string    `json:"label"`
		Scope     string    `json:"scope"`
		Project   string    `json:"project_instance"`
		ExpiresAt time.Time `json:"expires_at"`
		Revoked   bool      `json:"revoked"`
	} `json:"tokens"`
	Projects map[string]string `json:"projects"` // access instance -> project
}

// accessView is the snapshot joined by ID, as the views need it.
type accessView struct {
	snap      accessSnap
	now       time.Time
	userName  map[string]string
	disabled  map[string]bool
	groupName map[string]string
	members   map[string][]string // group -> users
	groupsOf  map[string][]string // user -> group names
	instance  map[string]string   // project -> access instance
}

func newAccessView(snap accessSnap, now time.Time) *accessView {
	v := &accessView{snap: snap, now: now, userName: map[string]string{}, disabled: map[string]bool{},
		groupName: map[string]string{}, members: map[string][]string{}, groupsOf: map[string][]string{}, instance: map[string]string{}}
	for _, u := range snap.Users {
		v.userName[u.ID], v.disabled[u.ID] = u.Name, u.Disabled
	}
	for _, g := range snap.Groups {
		v.groupName[g.ID] = g.Name
	}
	for _, m := range snap.Members {
		v.members[m.GroupID] = append(v.members[m.GroupID], m.UserID)
		v.groupsOf[m.UserID] = append(v.groupsOf[m.UserID], v.groupName[m.GroupID])
	}
	for inst, p := range snap.Projects {
		v.instance[p] = inst
	}
	return v
}

// liveToken: not revoked, not expired, and its user enabled.
func (v *accessView) liveToken(i int) bool {
	t := v.snap.Tokens[i]
	return !t.Revoked && t.ExpiresAt.After(v.now) && !v.disabled[t.UserID]
}

func (v *accessView) tokenState(i int) string {
	t := v.snap.Tokens[i]
	switch {
	case t.Revoked:
		return " [revoked]"
	case !t.ExpiresAt.After(v.now):
		return " [expired]"
	case v.disabled[t.UserID]:
		return " [user disabled]"
	}
	return ""
}

// grantedUsers is every enabled user granted the instance, directly or
// through a group; all includes disabled ones.
func (v *accessView) grantedUsers(inst string, all bool) map[string]bool {
	out := map[string]bool{}
	for _, g := range v.snap.Grants {
		if g.Project != inst {
			continue
		}
		users := []string{g.Subject}
		if g.Kind == "group" {
			users = v.members[g.Subject]
		} else if g.Kind != "user" {
			continue
		}
		for _, u := range users {
			if all || !v.disabled[u] {
				out[u] = true
			}
		}
	}
	return out
}

// grantNames names the instance's grants: users by name, groups as
// "group NAME"; a disabled user only when all.
func (v *accessView) grantNames(inst string, all bool) []string {
	var out []string
	for _, g := range v.snap.Grants {
		if g.Project != inst {
			continue
		}
		switch g.Kind {
		case "user":
			if v.disabled[g.Subject] && !all {
				continue
			}
			n := v.userName[g.Subject]
			if v.disabled[g.Subject] {
				n += " [disabled]"
			}
			out = append(out, n)
		case "group":
			out = append(out, "group "+v.groupName[g.Subject])
		default:
			out = append(out, g.Kind+" "+g.Subject)
		}
	}
	sort.Strings(out)
	return out
}

// writeTokens are the indexes of the tokens that write to the instance,
// by the hub's own rule (access.Store.CanWriteToken): live, scoped to it
// (project-scoped to it, or user-scoped), and its user granted it directly
// or through a group. With all, also the dead ones and the project-scoped
// ones whose user has no grant; noGrant marks those.
func (v *accessView) writeTokens(inst string, all bool) []int {
	granted := v.grantedUsers(inst, all)
	var out []int
	for i, t := range v.snap.Tokens {
		if !all && !v.liveToken(i) {
			continue
		}
		scoped := (t.Scope == "project" && t.Project == inst) || t.Scope == "user"
		if scoped && (granted[t.UserID] || (all && t.Scope == "project")) {
			out = append(out, i)
		}
	}
	sort.Slice(out, func(a, b int) bool { return v.snap.Tokens[out[a]].Label < v.snap.Tokens[out[b]].Label })
	return out
}

// noGrant marks a token of the instance whose user has no current grant
// on it: the hub refuses its writes.
func (v *accessView) noGrant(inst string, i int) string {
	if v.grantedUsers(inst, true)[v.snap.Tokens[i].UserID] {
		return ""
	}
	return " [no grant]"
}

func expiryText(t time.Time) string { return t.UTC().Format("2006-01-02 15:04Z") }

// scopeText is a token's scope, with its project for a project token.
func (v *accessView) scopeText(i int) string {
	t := v.snap.Tokens[i]
	if t.Scope == "project" {
		return "project " + v.snap.Projects[t.Project]
	}
	return t.Scope
}

const accessListUsage = `usage: aimem access list [--project PROJECT] [--user NAME] [--all] [--json]
       aimem access audit [--since TIME|DATE|Nd] [--actor ACTOR] [--action ACTION] [--limit N] [--json]`

// accessListCmd prints the access state as tables: per project, who is
// granted and which live tokens can write to it; then the users with
// their groups and tokens. --json prints the hub's whole document.
func accessListCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("aimem access list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := fs.String("project", "", "")
	fs.StringVar(project, "p", "", "")
	user := fs.String("user", "", "")
	all := fs.Bool("all", false, "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New(accessListUsage)
	}
	data, err := accessDo("GET", "/v1/access", nil)
	if err != nil {
		return err
	}
	if *asJSON {
		if *project != "" || *user != "" || *all {
			return errors.New("--json prints the whole document; it takes no filter")
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, data, "", "  "); err != nil {
			return err
		}
		fmt.Fprintln(out, pretty.String())
		return nil
	}
	var snap accessSnap
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	return renderAccess(out, newAccessView(snap, time.Now()), *project, *user, *all)
}

func renderAccess(out io.Writer, v *accessView, project, user string, all bool) error {
	only := map[string]bool{}
	if user != "" {
		byName := map[string][]string{}
		for _, u := range v.snap.Users {
			byName[u.Name] = append(byName[u.Name], u.ID)
		}
		if _, err := lookupName("user", user, byName); err != nil && len(byName[user]) == 0 {
			return err
		}
		for _, id := range byName[user] {
			only[id] = true
		}
	}
	var projects []string
	for p := range v.instance {
		if project == "" || p == project {
			projects = append(projects, p)
		}
	}
	sort.Strings(projects)
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROJECT\tGRANTED\tTOKEN\tUSER\tSCOPE\tEXPIRES")
	rows := 0
	for _, p := range projects {
		inst := v.instance[p]
		if user != "" {
			involved := false
			for u := range v.grantedUsers(inst, all) {
				involved = involved || only[u]
			}
			for _, i := range v.writeTokens(inst, all) {
				involved = involved || only[v.snap.Tokens[i].UserID]
			}
			if !involved {
				continue
			}
		}
		granted := strings.Join(v.grantNames(inst, all), ", ")
		if granted == "" {
			granted = "(none)"
		}
		first := true
		for _, i := range v.writeTokens(inst, all) {
			t := v.snap.Tokens[i]
			if user != "" && !only[t.UserID] {
				continue
			}
			name, g := p, granted
			if !first {
				name, g = "", ""
			}
			fmt.Fprintf(tw, "%s\t%s\t%s%s%s\t%s\t%s\t%s\n", name, g, t.Label, v.tokenState(i), v.noGrant(inst, i), v.userName[t.UserID], v.scopeText(i), expiryText(t.ExpiresAt))
			first = false
		}
		if first {
			fmt.Fprintf(tw, "%s\t%s\t(none that can write)\t\t\t\n", p, granted)
		}
		rows++
	}
	if rows == 0 {
		fmt.Fprintln(tw, "(no project has grants or tokens)\t\t\t\t\t")
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if project != "" {
		return nil
	}
	fmt.Fprintln(out)
	tw = tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "USER\tSTATE\tGROUPS\tTOKENS")
	users := append(v.snap.Users[:0:0], v.snap.Users...)
	sort.Slice(users, func(a, b int) bool { return users[a].Name < users[b].Name })
	for _, u := range users {
		if (u.Disabled && !all) || (user != "" && !only[u.ID]) {
			continue
		}
		state := "enabled"
		if u.Disabled {
			state = "disabled"
		}
		groups := append([]string(nil), v.groupsOf[u.ID]...)
		sort.Strings(groups)
		g := strings.Join(groups, ", ")
		if g == "" {
			g = "-"
		}
		var toks []string
		for i, t := range v.snap.Tokens {
			if t.UserID != u.ID || (!all && !v.liveToken(i)) {
				continue
			}
			toks = append(toks, fmt.Sprintf("%s%s (%s, %s)", t.Label, v.tokenState(i), v.scopeText(i), expiryText(t.ExpiresAt)))
		}
		sort.Strings(toks)
		tk := strings.Join(toks, "; ")
		if tk == "" {
			tk = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", u.Name, state, g, tk)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if !all {
		fmt.Fprintln(out, "\ndisabled users and revoked or expired tokens are hidden; --all shows them, --json prints the whole document")
	}
	return nil
}

// accessAuditCmd prints the administration audit, newest first.
func accessAuditCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("aimem access audit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	since := fs.String("since", "", "")
	actor := fs.String("actor", "", "")
	action := fs.String("action", "", "")
	limit := fs.Int("limit", 100, "")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New(accessListUsage)
	}
	q := url.Values{}
	if *since != "" {
		t, err := parseSince(*since, time.Now())
		if err != nil {
			return err
		}
		q.Set("since", t.UTC().Format(time.RFC3339))
	}
	if *actor != "" {
		q.Set("actor", *actor)
	}
	if *action != "" {
		q.Set("action", *action)
	}
	q.Set("limit", strconv.Itoa(*limit))
	data, err := accessDo("GET", "/v1/access/audit?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	if *asJSON {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, data, "", "  "); err != nil {
			return err
		}
		fmt.Fprintln(out, pretty.String())
		return nil
	}
	var got struct {
		Events []struct{ At, Actor, Action, Subject string } `json:"events"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		return err
	}
	if len(got.Events) == 0 {
		fmt.Fprintln(out, "no audit events match")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AT\tACTOR\tACTION\tSUBJECT")
	for _, e := range got.Events {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.At, e.Actor, e.Action, e.Subject)
	}
	return tw.Flush()
}

// readinessInput is what the checklist is computed from.
type readinessInput struct {
	project  string
	tasksOn  bool
	process  string // the selection's name and tag, "" when none
	instance string // the project's access instance, "" when it has none
	view     *accessView
	teams    []string // the enabled team profiles granted the project
}

// readinessChecklist lists what an agent needs before it can write to the
// project's board, each open item with the command that closes it.
func readinessChecklist(in readinessInput) string {
	var b strings.Builder
	open := 0
	item := func(done bool, text string, fix ...string) {
		mark := "[x]"
		if !done {
			mark = "[ ]"
			open++
		}
		fmt.Fprintf(&b, "  %s %s\n", mark, text)
		if !done {
			for _, f := range fix {
				fmt.Fprintf(&b, "      %s\n", f)
			}
		}
	}
	p := shellArg(in.project, false)
	item(in.tasksOn, "tasks on", "aimem tasks on -p "+p)
	if in.process != "" {
		item(true, "process selected: "+in.process)
	} else {
		fix := "aimem process select <repo> <tag> [<manifest>] -p " + p
		if in.tasksOn {
			fix += " (the next step above names one)"
		}
		item(false, "process selected", fix)
	}
	var granted []string
	var tokens []string
	user := "<user>"
	if in.instance != "" {
		granted = in.view.grantNames(in.instance, false)
		for _, i := range in.view.writeTokens(in.instance, false) {
			t := in.view.snap.Tokens[i]
			tokens = append(tokens, fmt.Sprintf("%s (%s, %s, expires %s)", t.Label, in.view.userName[t.UserID], in.view.scopeText(i), expiryText(t.ExpiresAt)))
		}
		for u := range in.view.grantedUsers(in.instance, false) {
			if n := in.view.userName[u]; user == "<user>" || n < user {
				user = n
			}
		}
	}
	for _, t := range in.teams {
		granted = append(granted, "team "+t)
	}
	if len(granted) > 0 {
		item(true, "granted: "+strings.Join(granted, ", "))
	} else {
		item(false, "a user or group granted", "aimem access grant add --project "+p+" --user-name <user>")
	}
	if user != "<user>" {
		user = shellArg(user, false)
	}
	if len(tokens) > 0 {
		item(true, "a live token that can write: "+strings.Join(tokens, "; "))
	} else {
		item(false, "a live token that can write",
			"aimem access token-issue-user --user-name "+user+" --label "+user+" --expires +90d --output FILE",
			"or, for this project only: aimem access token-issue --user-name "+user+" --label LABEL --project "+p+" --expires +90d --output FILE")
	}
	head := "readiness: ready for work\n"
	if open > 0 {
		head = fmt.Sprintf("readiness: %d of 4 open\n", open)
	}
	return head + b.String()
}

// printReadiness prints the project's checklist after a blank line, or
// why it could not be read.
func printReadiness(w io.Writer, project string) {
	list, err := projectReadiness(project)
	if err != nil {
		fmt.Fprintf(w, "\ncould not read the project's readiness: %v\n", err)
		return
	}
	fmt.Fprint(w, "\n"+list)
}

// projectReadiness reads the checklist's inputs over the local socket.
func projectReadiness(project string) (string, error) {
	base := "/v1/projects/" + url.PathEscape(project)
	var tasks struct {
		Value string `json:"value"`
	}
	if err := localJSON(http.MethodGet, base+"/meta/"+store.TasksMetaKey, nil, &tasks); err != nil {
		return "", err
	}
	var proc struct {
		Current *struct {
			Name, Ref, Commit string
		} `json:"current"`
	}
	if err := localJSON(http.MethodGet, base+"/process?history=1", nil, &proc); err != nil {
		return "", err
	}
	in := readinessInput{project: project, tasksOn: tasks.Value == "on"}
	if c := proc.Current; c != nil {
		tag := c.Ref
		if tag == "" {
			tag = c.Commit[:min(12, len(c.Commit))]
		}
		in.process = strings.TrimSpace(c.Name + " (" + tag + ")")
	}
	data, err := accessDo("GET", "/v1/access", nil)
	if err != nil {
		return "", err
	}
	var snap accessSnap
	if err := json.Unmarshal(data, &snap); err != nil {
		return "", err
	}
	in.view = newAccessView(snap, time.Now())
	in.instance = in.view.instance[project]
	in.teams = grantedTeams(project)
	return readinessChecklist(in), nil
}

// grantedTeams names the enabled team profiles granted the project, by
// the name aicrewd registered or, before it registers, service/team. A
// hub that cannot answer leaves the line as the user and group grants.
func grantedTeams(project string) []string {
	var got struct {
		Grants []struct {
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			ServiceID string `json:"service_id"`
			TeamID    string `json:"team_id"`
			Disabled  bool   `json:"disabled"`
		} `json:"grants"`
	}
	if localJSON(http.MethodGet, "/v1/projects/"+url.PathEscape(project)+"/grants", nil, &got) != nil {
		return nil
	}
	var out []string
	for _, g := range got.Grants {
		if g.Kind != "team_profile" || g.Disabled {
			continue
		}
		name := g.Name
		if name == "" {
			name = g.ServiceID + "/" + g.TeamID
		}
		out = append(out, name)
	}
	return out
}
