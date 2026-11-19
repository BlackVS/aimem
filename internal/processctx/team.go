package processctx

// Reads as a team conversation (AIMEM_TEAM_SESSION): the session's pinned
// hub, the installation's individual credential over verified TLS and the
// session's handle on every request, once the hub has verified the context
// and it matches the session's binding. A member holds no personal grant on
// the team's project, and none is needed: the hub answers under the team
// profile's grant. Nothing here falls back to the checkout's credential.

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"aimem/internal/adapter"
	"aimem/internal/ident"
	"aimem/internal/process"
	"aimem/internal/skills"
	"aimem/internal/teamsession"
)

// teamRestart is the next step when the session itself cannot be used.
const teamRestart = "restart the conversation through aicrew"

// TeamGrantFix is the skills commands' next step when the hub refuses a
// team conversation's read for lack of the team's grant. Every request
// still carries the installation's individual credential with the
// conversation's handle; what never happens is a read in personal mode.
const TeamGrantFix = "the team's access profile needs a grant on the project (on the hub host: aimem identity team grant); nothing was read outside the team conversation"

// teamConn is one verified team conversation's connection to its hub.
type teamConn struct {
	hub     *adapter.HubConfig // the session's URL and the individual credential
	client  *http.Client       // verified TLS, the session's handle on every request
	project string
}

// teamName names the conversation of the session file at path for a
// message: its session and team when the file can be read, else the path.
func teamName(path string) string {
	if f, err := teamsession.Load(path); err == nil {
		return fmt.Sprintf("team conversation %s of team %s", f.SessionID, f.TeamID)
	}
	return "the team conversation in " + path
}

// teamConnect verifies the team session in path with its hub and returns
// the connection for projectID (or the project of the checkout in dir).
func teamConnect(path, dir, root, projectID string) (*teamConn, error) {
	f, err := teamsession.Load(path)
	if err != nil {
		return nil, pinError("the team session file cannot be used ("+err.Error()+"); nothing was sent to any hub", teamRestart)
	}
	name, h := adapter.ResolveHub(root, f.Hub)
	switch {
	case h == nil || name != f.Hub:
		return nil, pinError(fmt.Sprintf("the team session's hub %q is not configured on this machine", f.Hub), teamRestart)
	case strings.TrimRight(h.URL, "/") != strings.TrimRight(f.URL, "/"):
		return nil, pinError("the hub's configured URL is not the team session's", teamRestart)
	}
	cred, err := teamsession.Credential(h)
	if err != nil {
		return nil, pinError(err.Error(), "")
	}
	client, err := teamsession.HubClient(h)
	if err != nil {
		return nil, pinError(err.Error(), "")
	}
	ctx, cancel := context.WithTimeout(context.Background(), hubLookupTimeout)
	rep, _, err := teamsession.Verify(ctx, client, f.URL, cred, f.Handle)
	cancel()
	switch {
	case err != nil:
		return nil, pinError("the team context is not verified: "+err.Error(), teamRestart)
	case !f.Binding().Matches(rep):
		return nil, pinError("the hub reports a different user, team or session than the team session file names", teamRestart)
	}
	id := projectID
	if id == "" {
		if id, err = ident.ProjectID(dir); err != nil {
			return nil, pinError("not a project checkout: "+err.Error(), "name the project with -p")
		}
	}
	team := *client
	base := team.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	team.Transport = withHeader{base, teamsession.Header, f.Handle}
	return &teamConn{hub: &adapter.HubConfig{URL: f.URL, Token: cred}, client: &team, project: id}, nil
}

// TeamSkills reads, as the team conversation the session file at path
// names, what `aimem skills install` and `list` need: the skills client
// for the project's routes and, with withProcess, the project's selected
// process from the hub's stored set, verified against the selection's
// digest and cached by it in root; name names the conversation, for the
// caller's own messages. Every error names the conversation.
func TeamSkills(path, dir, root, projectID string, withProcess bool) (set *process.Set, c *skills.Client, name string, err error) {
	name = teamName(path)
	fail := func(err error) (*process.Set, *skills.Client, string, error) {
		return nil, nil, name, fmt.Errorf("%s: %w", name, err)
	}
	tc, err := teamConnect(path, dir, root, projectID)
	if err != nil {
		return fail(fmt.Errorf("%w; nothing was read outside the team conversation", err))
	}
	c = &skills.Client{BaseURL: tc.hub.URL, Token: tc.hub.Token, HTTP: tc.client, Project: tc.project}
	if !withProcess {
		return nil, c, name, nil
	}
	ref, bundle, err := readSelectedSet(tc.hub, tc.client, tc.project, TeamGrantFix)
	if err != nil {
		return fail(err)
	}
	if _, err := process.StoreSet(root, bundle, ref.Digest); err != nil {
		return fail(fmt.Errorf("project %s's process set does not validate: %w", tc.project, err))
	}
	if set, err = process.CachedSet(root, *ref); err != nil {
		return fail(fmt.Errorf("project %s's process set cannot be read back: %w", tc.project, err))
	}
	return set, c, name, nil
}
