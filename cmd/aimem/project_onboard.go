package main

// `aimem project onboard`: the hub side of a new project in one command,
// composed of the routes the separate commands use (tasks on, process
// select or the hub default, the grant, a task token). It reads and checks
// everything first, so a refusal changes nothing, and a second run reports
// what exists and issues nothing.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"aimem/internal/process"
	"aimem/internal/store"
)

const onboardUsage = `usage: aimem project onboard <project> --user <name> [--process <repo> <tag>] [--manifest <path>]
                             [--expires TIME|DATE|+Nd] [--label <label>] [--output FILE|-] [--new-token]`

// onboardArgs is one parsed onboard command.
type onboardArgs struct {
	project, user, repo, tag, manifest, expires, label, output string
	newToken                                                   bool
}

func parseOnboard(args []string) (onboardArgs, error) {
	var a onboardArgs
	usage := errors.New(onboardUsage)
	fs := flag.NewFlagSet("project onboard", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&a.user, "user", "", "")
	fs.StringVar(&a.manifest, "manifest", "", "")
	fs.StringVar(&a.expires, "expires", "+90d", "")
	fs.StringVar(&a.label, "label", "", "")
	fs.StringVar(&a.output, "output", "", "")
	fs.BoolVar(&a.newToken, "new-token", false, "")
	var pos, rest []string
	for i := 0; i < len(args); i++ { // --process takes two values; flags may follow the project
		switch arg := args[i]; {
		case arg == "--process" || arg == "-process":
			if i+2 >= len(args) || a.repo != "" {
				return a, usage
			}
			a.repo, a.tag, i = args[i+1], args[i+2], i+2
		case strings.HasPrefix(arg, "-"):
			rest = append(rest, arg)
			if !strings.Contains(arg, "=") && arg != "--new-token" && arg != "-new-token" && i+1 < len(args) {
				rest, i = append(rest, args[i+1]), i+1
			}
		default:
			pos = append(pos, arg)
		}
	}
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || len(pos) != 1 || a.user == "" {
		return a, usage
	}
	if a.manifest != "" && a.repo == "" {
		return a, errors.New("--manifest goes with --process")
	}
	a.project = pos[0]
	if a.label == "" {
		a.label = a.project
	}
	return a, nil
}

func projectOnboardCmd(args []string) error {
	a, err := parseOnboard(args)
	if err != nil {
		return err
	}
	return onboard(a, os.Stdout)
}

func onboard(a onboardArgs, stdout io.Writer) error {
	p := a.project
	expires, err := parseExpiry(a.expires, time.Now())
	if err != nil {
		return err
	}
	// Read and check everything before the first change.
	data, err := accessDo("GET", "/v1/access", nil)
	if err != nil {
		return err
	}
	var snap accessSnap
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}
	v := newAccessView(snap, time.Now())
	byName := map[string][]string{}
	for _, u := range snap.Users {
		byName[u.Name] = append(byName[u.Name], u.ID)
	}
	uid, err := lookupName("user", a.user, byName)
	if err != nil {
		return fmt.Errorf("nothing changed: %w (aimem access user-add --user-name NAME adds one)", err)
	}
	if v.disabled[uid] {
		return fmt.Errorf("nothing changed: user %q is disabled", a.user)
	}
	inst := v.instance[p]
	writer := -1
	for i, t := range snap.Tokens {
		if t.UserID == uid && v.liveToken(i) && (t.Scope == "user" || (t.Scope == "project" && inst != "" && t.Project == inst)) {
			writer = i
			break
		}
	}
	issue := writer < 0 || a.newToken
	if issue && a.output == "" {
		return errors.New("nothing changed: a token will be issued; name where its secret goes with --output FILE (or - for a pipe)")
	}
	current, err := currentSelection(p)
	if err != nil {
		return err
	}
	var selectBody map[string]any
	switch {
	case current != nil && a.repo != "" && !sameSelection(*current, a):
		return fmt.Errorf("nothing changed: project %s already has a process selected (%s); onboard never re-selects one. To change it: aimem process select %s %s -p %s --expect %s",
			p, describeSelection(*current), shellArg(a.repo, false), shellArg(a.tag, false), shellArg(p, false), selectionTag(*current))
	case current == nil && a.repo != "":
		if selectBody, err = selectRequest(p, a.repo, a.tag, a.manifest, "", "", "", "", false); err != nil {
			return fmt.Errorf("nothing changed: %w", err)
		}
	}
	var sink *secretOutput
	if issue {
		if sink, err = reserveSecretOutput("--output", a.output); err != nil {
			return err
		}
	}
	delivered := false
	defer func() {
		if sink != nil && !delivered {
			sink.abandon()
		}
	}()
	out := stdout
	if sink != nil && sink.toStdout() {
		out = noticeOut
	}

	// The changes, in the order the manual path takes them.
	fmt.Fprintf(out, "onboarding project %q for user %q\n", p, a.user)
	base := "/v1/projects/" + url.PathEscape(p)
	// The list, not the project's meta: a project the hub does not know
	// yet has no meta to read, and tasks on creates it.
	var on struct {
		Projects []string `json:"projects"`
	}
	if err := localJSON(http.MethodGet, "/v1/projects?tasks_enabled=true", nil, &on); err != nil {
		return err
	}
	if slices.Contains(on.Projects, p) {
		fmt.Fprintln(out, "  tasks: already on")
	} else {
		body, _ := json.Marshal(map[string]string{"value": "on"})
		if err := localJSON(http.MethodPut, base+"/meta/"+store.TasksMetaKey, body, new(json.RawMessage)); err != nil {
			return fmt.Errorf("turning tasks on: %w", err)
		}
		fmt.Fprintln(out, "  tasks: turned on")
	}

	switch {
	case current != nil:
		fmt.Fprintf(out, "  process: already selected, %s\n", describeSelection(*current))
	case selectBody != nil:
		raw, _ := json.Marshal(selectBody)
		var got struct {
			Current *process.Ref `json:"current"`
		}
		if err := localJSON(http.MethodPut, base+"/process", raw, &got); err != nil {
			return fmt.Errorf("selecting the process: %w", err)
		}
		if got.Current == nil {
			return errors.New("selecting the process: the hub answered no selection")
		}
		fmt.Fprintf(out, "  process: selected %s\n", describeSelection(*got.Current))
	default:
		applied, err := applyProcessDefault(p)
		switch {
		case err != nil:
			return fmt.Errorf("the hub's default process was not applied: %w", err)
		case applied != "":
			fmt.Fprint(out, "  process: "+applied)
		default:
			fmt.Fprintln(out, "  process: open, no --process and no hub default (the checklist below names the command)")
		}
	}

	switch {
	case inst != "" && v.directGrant(inst, uid):
		fmt.Fprintf(out, "  grant: user %s already granted\n", a.user)
	case inst != "" && v.grantedUsers(inst, false)[uid]:
		fmt.Fprintf(out, "  grant: user %s already granted through a group (%s)\n", a.user, strings.Join(v.groupsOf[uid], ", "))
	default:
		if _, err := accessDo("PUT", base+"/access/user/"+url.PathEscape(uid), nil); err != nil {
			return fmt.Errorf("granting user %s: %w", a.user, err)
		}
		fmt.Fprintf(out, "  grant: granted user %s\n", a.user)
	}

	if !issue {
		t := snap.Tokens[writer]
		fmt.Fprintf(out, "  token: none issued; user %s already has one that can write: %s (%s, expires %s); --new-token issues another\n",
			a.user, t.Label, v.scopeText(writer), expiryText(t.ExpiresAt))
	} else {
		data, err := accessDo("POST", "/v1/access/tokens", map[string]any{"user_id": uid, "label": a.label, "expires_at": expires, "project": p})
		if err != nil {
			return fmt.Errorf("issuing the token: %w", err)
		}
		var resp struct {
			Token struct {
				ID string `json:"id"`
			} `json:"token"`
			Secret string `json:"secret"`
		}
		if json.Unmarshal(data, &resp) != nil || resp.Secret == "" {
			return fmt.Errorf("the hub's answer carried no token secret; check with 'aimem access list --project %s' and revoke any token you did not receive", p)
		}
		if err := sink.write(resp.Secret); err != nil {
			return fmt.Errorf("token %s was issued but its secret could not be written to %s (%v); revoke it with: aimem access token-revoke --token-id %s", resp.Token.ID, sink.where(), err, resp.Token.ID)
		}
		delivered = true
		fmt.Fprintf(out, "  token: issued %s, label %s, project %s, expires %s; the secret was written once to %s\n",
			resp.Token.ID, a.label, p, expiryText(expires), sink.where())
	}
	printReadiness(out, p)
	return nil
}

// sameSelection reports whether --process names the selection the project
// already has: the same repository and manifest at the same tag or commit.
func sameSelection(cur process.Ref, a onboardArgs) bool {
	manifest := a.manifest
	if manifest == "" {
		manifest = process.DefaultManifest
	}
	return cur.Repo == a.repo && cur.Manifest == manifest && (cur.Ref == a.tag || cur.Commit == a.tag)
}

// selectionTag is what --expect takes for the current selection.
func selectionTag(r process.Ref) string {
	if r.Ref != "" {
		return r.Ref
	}
	return r.Commit
}

func describeSelection(r process.Ref) string {
	name := r.Name
	if name == "" {
		name = r.Manifest
	}
	if r.Ref == "" {
		return fmt.Sprintf("%s (commit %s)", name, r.Commit)
	}
	return fmt.Sprintf("%s (tag %s, commit %s)", name, r.Ref, r.Commit)
}

// directGrant reports whether the user holds a grant on the instance of
// its own, not only through a group.
func (v *accessView) directGrant(inst, uid string) bool {
	for _, g := range v.snap.Grants {
		if g.Project == inst && g.Kind == "user" && g.Subject == uid {
			return true
		}
	}
	return false
}
