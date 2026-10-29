package processctx

// The process pin: what an offer names for the project's process (aicrew's
// process pin and instruction digest), read from the hub with the
// checkout's own credential under its project grant, or, in a team
// conversation (AIMEM_TEAM_SESSION), under the team's grant. It needs no
// Git and no route to the process repository, and stores nothing.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"aimem/internal/adapter"
	"aimem/internal/ident"
	"aimem/internal/process"
	"aimem/internal/teamsession"
)

// Pin is the project's current selection as an offer pins it.
type Pin struct {
	Project    string `json:"project"`
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	Manifest   string `json:"manifest"`
	// SetDigest is the digest of the whole stored set (process.Digest).
	SetDigest string `json:"set_digest"`
	// InstructionDigest is "sha256:" and the hex SHA-256 of the manifest
	// file's exact bytes at the commit.
	InstructionDigest string `json:"instruction_digest"`
}

// ReadPin reads the pin of the project's current selection. The
// credential is selected as Load selects it: a required local credential
// that is missing or refused ends the read. The set is read from the hub
// and must digest to the selection's digest before the manifest is hashed.
//
// With AIMEM_TEAM_SESSION set, it reads as that team conversation instead,
// as the member's MCP tools and the reservation commands do, and never
// falls back to the checkout's credential.
func ReadPin(dir, root, projectID string) (*Pin, error) {
	if path := os.Getenv(teamsession.EnvVar); path != "" {
		return readTeamPin(path, dir, root, projectID)
	}
	r := &Result{Project: projectID}
	hub, client, ok := r.connect(dir, root)
	if !ok {
		return nil, pinError(r.Detail, r.Fix)
	}
	return readPin(hub, client, r.Project, credentialFix)
}

// teamPinFix is the next step when the team's grant does not reach the
// project.
const teamPinFix = "the team's access profile needs a grant on the project (on the hub host: aimem identity team grant); no personal credential is substituted"

// readTeamPin reads the pin as the team session in path: the session's
// pinned hub, the installation's individual credential over verified TLS,
// the session's handle on every request, after the hub has verified the
// context and it matches the session's binding.
func readTeamPin(path, dir, root, projectID string) (*Pin, error) {
	const restart = "restart the conversation through aicrew"
	f, err := teamsession.Load(path)
	if err != nil {
		return nil, pinError("the team session file cannot be used ("+err.Error()+"); nothing was sent to any hub", restart)
	}
	name, h := adapter.ResolveHub(root, f.Hub)
	switch {
	case h == nil || name != f.Hub:
		return nil, pinError(fmt.Sprintf("the team session's hub %q is not configured on this machine", f.Hub), restart)
	case strings.TrimRight(h.URL, "/") != strings.TrimRight(f.URL, "/"):
		return nil, pinError("the hub's configured URL is not the team session's", restart)
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
		return nil, pinError("the team context is not verified: "+err.Error(), "")
	case !f.Binding().Matches(rep):
		return nil, pinError("the hub reports a different user, team or session than the team session file names", restart)
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
	return readPin(&adapter.HubConfig{URL: f.URL, Token: cred}, &team, id, teamPinFix)
}

// withHeader sets one header on every request it carries.
type withHeader struct {
	base        http.RoundTripper
	name, value string
}

func (t withHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(t.name, t.value)
	return t.base.RoundTrip(r)
}

// readPin reads the selection and its stored set with hub's bearer over
// client; fix is the next step when the hub refuses the read.
func readPin(hub *adapter.HubConfig, client *http.Client, id, fix string) (*Pin, error) {
	var sel struct {
		Current *process.Ref `json:"current"`
	}
	err := hubGetJSONClient(hub, client, "/v1/projects/"+url.PathEscape(id)+"/process", &sel)
	var refused *hubRefusal
	switch {
	case errors.As(err, &refused) && refused.denied():
		return nil, pinError("the hub refused to read project "+id+"'s process selection: "+err.Error(), fix)
	case errors.As(err, &refused) && refused.status == 404:
		return nil, pinError("no process is selected for project "+id, "an admin selects one on the hub host: aimem process select <repo> <tag> -p "+id)
	case err != nil:
		return nil, pinError("cannot read project "+id+"'s process selection: "+err.Error(), "retry when the hub is reachable")
	case sel.Current == nil:
		return nil, pinError("no process is selected for project "+id, "an admin selects one on the hub host: aimem process select <repo> <tag> -p "+id)
	}
	ref := *sel.Current
	if err := ref.Validate(); err != nil {
		return nil, pinError("the hub's process selection for project "+id+" is malformed: "+err.Error(), "")
	}
	if ref.Digest == "" {
		return nil, pinError("project "+id+"'s selection predates stored sets, so the hub cannot vouch for its manifest", "an admin re-selects it on the hub host (aimem process select …), which stores the set")
	}
	outcome, set, _, err := fetchHubSet(hub, client, id, &ref, false, false)
	switch outcome {
	case hubSetStored:
	case hubSetDenied:
		return nil, pinError("the hub refused to serve project "+id+"'s process set: "+err.Error(), fix)
	case hubSetUnsupported:
		return nil, pinError("the hub serves no stored set for project "+id+"'s selection: "+err.Error(), "an admin re-selects it on the hub host (aimem process select …), which stores the set; a hub that predates stored sets is upgraded first")
	default:
		return nil, pinError("cannot read project "+id+"'s process set: "+err.Error(), "")
	}
	manifest, ok := set.Files[ref.Manifest]
	if !ok {
		return nil, pinError("project "+id+"'s process set has no manifest at "+ref.Manifest, "")
	}
	sum := sha256.Sum256(manifest)
	return &Pin{Project: id, Repository: ref.Repo, Commit: ref.Commit, Manifest: ref.Manifest,
		SetDigest: ref.Digest, InstructionDigest: "sha256:" + hex.EncodeToString(sum[:])}, nil
}

func pinError(detail, fix string) error {
	if fix == "" {
		return errors.New(detail)
	}
	return fmt.Errorf("%s; %s", detail, fix)
}
