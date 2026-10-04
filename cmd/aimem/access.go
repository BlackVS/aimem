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
	"sort"
	"strings"
	"time"
)

const accessUsage = `usage: aimem access list
       aimem access user-add --user-name NAME
       aimem access user-set --user-id ID --user-name NAME --state enabled|disabled
       aimem access group-add --group-name NAME
       aimem access member add|rm (--group-name NAME | --group-id ID) (--user-name NAME | --user-id ID)
       aimem access grant add|rm --project PROJECT (--user-name NAME | --user-id ID | --group-name NAME | --group-id ID)
       aimem access grant-rm-instance --instance ID (--user-name NAME | --user-id ID | --group-name NAME | --group-id ID)
       aimem access token-issue (--user-name NAME | --user-id ID) --label LABEL (--project PROJECT | --read-only)
                                --expires RFC3339 --output FILE|-
       aimem access token-issue-user (--user-name NAME | --user-id ID) --label LABEL --expires RFC3339 --output FILE|-
       aimem access token-revoke --token-id ID

Run on the aimem host with its local service running. A user or group is
named by exactly one of its name or its ID; an unknown name is refused with
the names that exist. user-add and group-add take the name only, since the
hub generates the ID; user-set takes the ID and the new name.

token-issue-user issues a token that follows the user's current project
grants; token-issue issues one scoped to a project, or a read-only one.
The secret is written once to --output: a new file only you can read, or -
for standard output into a pipe (refused on a terminal). Every other line
then goes to standard error. Admin tokens are still managed only by the
host-console 'aimem token' command.

The positional forms of earlier releases (for example
'aimem access grant add PROJECT user USER_ID') keep working for this
release and print the new form.

Examples:
  aimem access user-add --user-name pilot-worker
  aimem access grant add --project example --group-name reviewers
  aimem access token-issue-user --user-name pilot-worker --label pilot-worker --expires 2026-12-28T00:00:00Z --output worker.token`

// accessDirectory resolves user and group names to IDs; accessCmd reads it
// from the hub, tests supply one.
type accessDirectory interface {
	resolve(kind, name string) (string, error)
}

// accessCall is one parsed access command: the request, where an issued
// secret goes, and the new form to print when an old form was used.
type accessCall struct {
	method, path string
	body         any
	output       string
	newForm      string
}

func accessCmd(args []string) error {
	call, err := accessRequest(args, &hubDirectory{})
	if err != nil {
		return err
	}
	if call.newForm != "" {
		legacyForm(call.newForm)
	}
	var sink *secretOutput
	if call.output != "" {
		if sink, err = reserveSecretOutput("--output", call.output); err != nil {
			return err
		}
	}
	delivered := false
	defer func() {
		if sink != nil && !delivered {
			sink.abandon()
		}
	}()
	data, err := accessDo(call.method, call.path, call.body)
	if err != nil {
		return err
	}
	out := io.Writer(os.Stdout)
	if sink != nil {
		var resp map[string]json.RawMessage
		var secret string
		if json.Unmarshal(data, &resp) != nil || json.Unmarshal(resp["secret"], &secret) != nil || secret == "" {
			return fmt.Errorf("the hub's answer carried no token secret; check with 'aimem access list' and revoke any token you did not receive")
		}
		if err := sink.write(secret); err != nil {
			var tok struct {
				ID string `json:"id"`
			}
			json.Unmarshal(resp["token"], &tok)
			return fmt.Errorf("token %s was issued but its secret could not be written to %s (%v); revoke it with: aimem access token-revoke --token-id %s", tok.ID, sink.where(), err, tok.ID)
		}
		delivered = true
		delete(resp, "secret")
		if data, err = json.Marshal(resp); err != nil {
			return err
		}
		if sink.toStdout() {
			out = noticeOut
		}
		defer fmt.Fprintf(out, "the secret was written once to %s\n", sink.where())
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return err
	}
	fmt.Fprintln(out, pretty.String())
	return nil
}

// accessDo sends one request to the local service and returns a 200 answer.
func accessDo(method, path string, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, "http://aimem"+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w (is the local aimem service running?)", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, data)
	}
	return data, nil
}

// hubDirectory reads the users and groups once, on the first name lookup.
type hubDirectory struct {
	loaded bool
	names  map[string]map[string][]string // kind -> name -> IDs
}

func (d *hubDirectory) resolve(kind, name string) (string, error) {
	if !d.loaded {
		data, err := accessDo("GET", "/v1/access", nil)
		if err != nil {
			return "", fmt.Errorf("cannot read the users and groups to resolve %s name %q: %w", kind, name, err)
		}
		var snap struct {
			Users  []struct{ ID, Name string } `json:"users"`
			Groups []struct{ ID, Name string } `json:"groups"`
		}
		if err := json.Unmarshal(data, &snap); err != nil {
			return "", err
		}
		d.names = map[string]map[string][]string{"user": {}, "group": {}}
		for _, u := range snap.Users {
			d.names["user"][u.Name] = append(d.names["user"][u.Name], u.ID)
		}
		for _, g := range snap.Groups {
			d.names["group"][g.Name] = append(d.names["group"][g.Name], g.ID)
		}
		d.loaded = true
	}
	return lookupName(kind, name, d.names[kind])
}

// lookupName applies the selector rule to one directory: exactly one entity
// of that name, or a refusal that lists what exists.
func lookupName(kind, name string, byName map[string][]string) (string, error) {
	switch ids := byName[name]; len(ids) {
	case 1:
		return ids[0], nil
	case 0:
		known := make([]string, 0, len(byName))
		for n := range byName {
			known = append(known, n)
		}
		sort.Strings(known)
		if len(known) == 0 {
			return "", fmt.Errorf("unknown %s name %q: the hub has no %ss", kind, name, kind)
		}
		return "", fmt.Errorf("unknown %s name %q; the %ss are: %s", kind, name, kind, strings.Join(known, ", "))
	default:
		return "", fmt.Errorf("%s name %q is held by %d %ss (%s); name one with --%s-id", kind, name, len(ids), kind, strings.Join(ids, ", "), kind)
	}
}

// accessFlags is the named-flag form's parser for one subcommand.
type accessFlags struct {
	fs                                *flag.FlagSet
	userName, userID, groupName, grID *string
}

func newAccessFlags(name string) *accessFlags {
	fs := flag.NewFlagSet("aimem access "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return &accessFlags{fs: fs,
		userName: fs.String("user-name", "", ""), userID: fs.String("user-id", "", ""),
		groupName: fs.String("group-name", "", ""), grID: fs.String("group-id", "", "")}
}

// subject resolves the one user or group the flags name; allowGroup false
// means only a user may be named.
func (f *accessFlags) subject(dir accessDirectory, allowUser, allowGroup bool) (kind, id string, err error) {
	set := 0
	for _, v := range []*string{f.userName, f.userID, f.groupName, f.grID} {
		if *v != "" {
			set++
		}
	}
	var want []string
	if allowUser {
		want = append(want, "--user-name", "--user-id")
	}
	if allowGroup {
		want = append(want, "--group-name", "--group-id")
	}
	if set != 1 || (!allowUser && (*f.userName != "" || *f.userID != "")) || (!allowGroup && (*f.groupName != "" || *f.grID != "")) {
		return "", "", fmt.Errorf("name exactly one of %s", strings.Join(want, ", "))
	}
	switch {
	case *f.userID != "":
		return "user", *f.userID, nil
	case *f.grID != "":
		return "group", *f.grID, nil
	case *f.userName != "":
		id, err := dir.resolve("user", *f.userName)
		return "user", id, err
	default:
		id, err := dir.resolve("group", *f.groupName)
		return "group", id, err
	}
}

func accessRequest(args []string, dir accessDirectory) (accessCall, error) {
	usage := errors.New(accessUsage)
	if len(args) == 0 {
		return accessCall{}, usage
	}
	cmd := args[0]
	rest := args[1:]
	verb := ""
	if cmd == "member" || cmd == "grant" {
		if len(rest) == 0 || (rest[0] != "add" && rest[0] != "rm") {
			return accessCall{}, usage
		}
		verb, rest = rest[0], rest[1:]
	}
	if cmd == "list" {
		if len(rest) == 0 {
			return accessCall{method: "GET", path: "/v1/access"}, nil
		}
		return accessCall{}, usage
	}
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		return legacyAccessRequest(cmd, verb, rest)
	}
	call, err := namedAccessRequest(cmd, verb, rest, dir)
	if errors.Is(err, flag.ErrHelp) {
		return accessCall{}, usage
	}
	if err != nil && !errors.Is(err, errAccessUsage) {
		return accessCall{}, fmt.Errorf("aimem access %s: %w", strings.TrimSpace(cmd+" "+verb), err)
	}
	if err != nil {
		return accessCall{}, usage
	}
	return call, nil
}

var errAccessUsage = errors.New("usage")

func namedAccessRequest(cmd, verb string, args []string, dir accessDirectory) (accessCall, error) {
	q := url.PathEscape
	f := newAccessFlags(cmd)
	project := f.fs.String("project", "", "")
	f.fs.StringVar(project, "p", "", "")
	state := f.fs.String("state", "", "")
	instance := f.fs.String("instance", "", "")
	label := f.fs.String("label", "", "")
	readOnly := f.fs.Bool("read-only", false, "")
	expires := f.fs.String("expires", "", "")
	output := f.fs.String("output", "", "")
	tokenID := f.fs.String("token-id", "", "")
	if err := f.fs.Parse(args); err != nil {
		return accessCall{}, err
	}
	if f.fs.NArg() != 0 {
		return accessCall{}, fmt.Errorf("unexpected argument %q", f.fs.Arg(0))
	}
	// Each command accepts only its own flags.
	allowed := map[string]string{
		"user-add":          "user-name",
		"user-set":          "user-id user-name state",
		"group-add":         "group-name",
		"member":            "user-name user-id group-name group-id",
		"grant":             "project p user-name user-id group-name group-id",
		"grant-rm-instance": "instance user-name user-id group-name group-id",
		"token-issue":       "user-name user-id label project p read-only expires output",
		"token-issue-user":  "user-name user-id label expires output",
		"token-revoke":      "token-id",
	}[cmd]
	if allowed == "" {
		return accessCall{}, errAccessUsage
	}
	var stray error
	f.fs.Visit(func(fl *flag.Flag) {
		if stray == nil && !strings.Contains(" "+allowed+" ", " "+fl.Name+" ") {
			stray = fmt.Errorf("--%s does not apply here", fl.Name)
		}
	})
	if stray != nil {
		return accessCall{}, stray
	}
	expiry := func() (time.Time, error) {
		t, err := time.Parse(time.RFC3339, *expires)
		if err != nil {
			return t, fmt.Errorf("--expires must be an RFC 3339 time such as 2026-12-28T00:00:00Z")
		}
		return t, nil
	}
	switch cmd {
	case "user-add":
		if *f.userName == "" {
			return accessCall{}, errors.New("--user-name is required")
		}
		return accessCall{method: "POST", path: "/v1/access/users", body: map[string]any{"name": *f.userName}}, nil
	case "user-set":
		if *f.userID == "" || *f.userName == "" || (*state != "enabled" && *state != "disabled") {
			return accessCall{}, errors.New("needs --user-id, the new --user-name and --state enabled|disabled")
		}
		return accessCall{method: "PUT", path: "/v1/access/users/" + q(*f.userID), body: map[string]any{"name": *f.userName, "disabled": *state == "disabled"}}, nil
	case "group-add":
		if *f.groupName == "" {
			return accessCall{}, errors.New("--group-name is required")
		}
		return accessCall{method: "POST", path: "/v1/access/groups", body: map[string]any{"name": *f.groupName}}, nil
	case "member":
		user := &accessFlags{userName: f.userName, userID: f.userID, groupName: new(string), grID: new(string)}
		group := &accessFlags{userName: new(string), userID: new(string), groupName: f.groupName, grID: f.grID}
		_, uid, err := user.subject(dir, true, false)
		if err != nil {
			return accessCall{}, err
		}
		_, gid, err := group.subject(dir, false, true)
		if err != nil {
			return accessCall{}, err
		}
		return accessCall{method: map[string]string{"add": "PUT", "rm": "DELETE"}[verb], path: "/v1/access/groups/" + q(gid) + "/members/" + q(uid)}, nil
	case "grant":
		if *project == "" {
			return accessCall{}, errors.New("--project is required")
		}
		kind, id, err := f.subject(dir, true, true)
		if err != nil {
			return accessCall{}, err
		}
		return accessCall{method: map[string]string{"add": "PUT", "rm": "DELETE"}[verb], path: "/v1/projects/" + q(*project) + "/access/" + kind + "/" + q(id)}, nil
	case "grant-rm-instance":
		if *instance == "" {
			return accessCall{}, errors.New("--instance is required")
		}
		kind, id, err := f.subject(dir, true, true)
		if err != nil {
			return accessCall{}, err
		}
		return accessCall{method: "DELETE", path: "/v1/access/grants/" + q(*instance) + "/" + kind + "/" + q(id)}, nil
	case "token-issue", "token-issue-user":
		if *label == "" || *expires == "" || *output == "" {
			return accessCall{}, errors.New("needs --label, --expires and --output")
		}
		t, err := expiry()
		if err != nil {
			return accessCall{}, err
		}
		_, uid, err := f.subject(dir, true, false)
		if err != nil {
			return accessCall{}, err
		}
		body := map[string]any{"user_id": uid, "label": *label, "expires_at": t}
		if cmd == "token-issue-user" {
			body["scope"] = "user"
		} else {
			if (*project == "") == !*readOnly {
				return accessCall{}, errors.New("name exactly one of --project or --read-only")
			}
			body["project"] = *project
		}
		return accessCall{method: "POST", path: "/v1/access/tokens", body: body, output: *output}, nil
	case "token-revoke":
		if *tokenID == "" {
			return accessCall{}, errors.New("--token-id is required")
		}
		return accessCall{method: "DELETE", path: "/v1/access/tokens/" + q(*tokenID)}, nil
	}
	return accessCall{}, errAccessUsage
}

// legacyAccessRequest serves the positional forms of earlier releases for
// one more release, each with the new form it should be written as.
func legacyAccessRequest(cmd, verb string, args []string) (accessCall, error) {
	q := url.PathEscape
	fail := accessCall{}
	usage := errors.New(accessUsage)
	quote := func(s string) string { return shellArg(s, false) }
	switch cmd {
	case "user-add":
		if len(args) == 1 {
			return accessCall{method: "POST", path: "/v1/access/users", body: map[string]any{"name": args[0]},
				newForm: "aimem access user-add --user-name " + quote(args[0])}, nil
		}
	case "user-set":
		if len(args) == 3 && (args[2] == "enabled" || args[2] == "disabled") {
			return accessCall{method: "PUT", path: "/v1/access/users/" + q(args[0]), body: map[string]any{"name": args[1], "disabled": args[2] == "disabled"},
				newForm: "aimem access user-set --user-id " + quote(args[0]) + " --user-name " + quote(args[1]) + " --state " + args[2]}, nil
		}
	case "group-add":
		if len(args) == 1 {
			return accessCall{method: "POST", path: "/v1/access/groups", body: map[string]any{"name": args[0]},
				newForm: "aimem access group-add --group-name " + quote(args[0])}, nil
		}
	case "member":
		if len(args) == 2 {
			return accessCall{method: map[string]string{"add": "PUT", "rm": "DELETE"}[verb], path: "/v1/access/groups/" + q(args[0]) + "/members/" + q(args[1]),
				newForm: "aimem access member " + verb + " --group-id " + quote(args[0]) + " --user-id " + quote(args[1])}, nil
		}
	case "grant":
		if len(args) == 3 && (args[1] == "user" || args[1] == "group") {
			return accessCall{method: map[string]string{"add": "PUT", "rm": "DELETE"}[verb], path: "/v1/projects/" + q(args[0]) + "/access/" + args[1] + "/" + q(args[2]),
				newForm: "aimem access grant " + verb + " --project " + quote(args[0]) + " --" + args[1] + "-id " + quote(args[2])}, nil
		}
	case "grant-rm-instance":
		if len(args) == 3 && (args[1] == "user" || args[1] == "group") {
			return accessCall{method: "DELETE", path: "/v1/access/grants/" + q(args[0]) + "/" + args[1] + "/" + q(args[2]),
				newForm: "aimem access grant-rm-instance --instance " + quote(args[0]) + " --" + args[1] + "-id " + quote(args[2])}, nil
		}
	case "token-issue":
		if len(args) == 4 {
			expires, err := time.Parse(time.RFC3339, args[3])
			if err != nil {
				return fail, fmt.Errorf("expiry must be RFC3339: %w", err)
			}
			project, scope := args[2], "--project "+quote(args[2])
			if project == "-" {
				project, scope = "", "--read-only"
			}
			return accessCall{method: "POST", path: "/v1/access/tokens", body: map[string]any{"user_id": args[0], "label": args[1], "project": project, "expires_at": expires},
				newForm: "aimem access token-issue --user-id " + quote(args[0]) + " --label " + quote(args[1]) + " " + scope + " --expires " + args[3] + " --output FILE"}, nil
		}
	case "token-issue-user":
		if len(args) == 3 {
			expires, err := time.Parse(time.RFC3339, args[2])
			if err != nil {
				return fail, fmt.Errorf("expiry must be RFC3339: %w", err)
			}
			return accessCall{method: "POST", path: "/v1/access/tokens", body: map[string]any{"user_id": args[0], "label": args[1], "scope": "user", "expires_at": expires},
				newForm: "aimem access token-issue-user --user-id " + quote(args[0]) + " --label " + quote(args[1]) + " --expires " + args[2] + " --output FILE"}, nil
		}
	case "token-revoke":
		if len(args) == 1 {
			return accessCall{method: "DELETE", path: "/v1/access/tokens/" + q(args[0]),
				newForm: "aimem access token-revoke --token-id " + quote(args[0])}, nil
		}
	}
	return fail, usage
}
