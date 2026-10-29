package main

// `aimem hub credential [<name>] [--json]`: whether this installation holds
// an individual credential for a hub, and what the hub says it is. It
// prints no part of any secret: only set or none, the credential's scope
// and state as the hub reports them, and the user and token IDs.
//
// The individual credential is the hub's task_token (`aimem hub
// task-token`). It is what `aimem identity proof` presents, and a client
// such as aicrew's bootstrap checks it here before it spends an invitation
// attempt on a proof that cannot succeed.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/taskcred"
	"aimem/internal/teamsession"
)

const hubCredentialUsage = "usage: aimem hub credential [<name>] [--json]"

// hubCredentialStatus is what `aimem hub credential` reports.
type hubCredentialStatus struct {
	Hub string `json:"hub"`
	// Credential is "set" when the hub has an individual (user-scoped
	// ordinary) credential stored, "none" when it has none, and "other"
	// when what is stored is not an individual credential.
	Credential string `json:"credential"`
	// State is the hub's answer for the stored credential: "active",
	// "refused" (the hub does not accept it: revoked, expired or unknown),
	// "unreachable" (no answer), "untrusted" (not sent: the entry is not
	// https, is --insecure, or its trust does not load), or "absent" when
	// nothing is stored.
	State   string `json:"state"`
	Scope   string `json:"scope,omitempty"` // user, project or read-only, as the hub reports it
	UserID  string `json:"user_id,omitempty"`
	TokenID string `json:"token_id,omitempty"`
	// UserName is the hub's name for the user, from a live answer only.
	UserName string `json:"user_name,omitempty"`
	// Label and ExpiresAt name the token: from the hub's answer when it
	// accepts the credential, else as recorded when it was stored.
	Label     string `json:"label,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	// Detail explains an unreachable or untrusted hub; it never carries a
	// secret.
	Detail string `json:"detail,omitempty"`
}

// hubCredentialCmd is `aimem hub credential`.
func hubCredentialCmd(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("aimem hub credential", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "")
	var name string
	rest := args
	for len(rest) > 0 {
		if strings.HasPrefix(rest[0], "-") {
			if err := fs.Parse(rest); err != nil {
				return errors.New(hubCredentialUsage)
			}
			rest = fs.Args()
			continue
		}
		if name != "" {
			return errors.New(hubCredentialUsage)
		}
		name, rest = rest[0], rest[1:]
	}
	resolved, h := adapter.ResolveHub(stateRoot(), name)
	if h == nil {
		if name == "" {
			return errors.New("no hub is configured on this machine (aimem hub add)")
		}
		return fmt.Errorf("hub %q is not configured on this machine", name)
	}
	st := probeHubCredential(resolved, h)
	if *asJSON {
		out := json.NewEncoder(stdout)
		out.SetIndent("", "  ")
		return out.Encode(st)
	}
	fmt.Fprintln(stdout, st.text())
	return nil
}

func probeHubCredential(name string, h *adapter.HubConfig) hubCredentialStatus {
	st := hubCredentialStatus{Hub: name, Credential: "none", State: "absent"}
	if h.TaskToken == "" {
		return st
	}
	cred, err := teamsession.Credential(h)
	if err != nil {
		st.Credential = "other"
		st.State = "absent"
		st.Detail = "the stored task credential is not an individual (user-scoped ordinary) credential"
		return st
	}
	st.Credential = "set"
	if m := h.TaskTokenMeta; m != nil {
		st.Label, st.ExpiresAt = m.Label, m.ExpiresAt
	}
	client, err := teamsession.HubClient(h)
	if err != nil {
		st.State, st.Detail = "unreachable", err.Error()
		if errors.Is(err, adapter.ErrUntrustedHub) {
			st.State = "untrusted"
		}
		return st
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(h.URL, "/")+"/v1/access/identity", nil)
	if err != nil {
		st.State, st.Detail = "unreachable", "invalid hub URL"
		return st
	}
	req.Header.Set("Authorization", "Bearer "+cred)
	resp, err := client.Do(req)
	if err != nil {
		st.State, st.Detail = "unreachable", redactCred(err.Error(), cred)
		return st
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		st.State = "refused"
		return st
	case resp.StatusCode != http.StatusOK:
		st.State, st.Detail = "unreachable", fmt.Sprintf("the hub answered HTTP %d", resp.StatusCode)
		return st
	}
	var id taskcred.Identity
	if json.Unmarshal(raw, &id) != nil {
		st.State, st.Detail = "unreachable", "the hub's answer is not an identity report"
		return st
	}
	st.State, st.Scope, st.UserID, st.TokenID, st.UserName = "active", id.Scope, id.UserID, id.TokenID, id.Name
	if id.Label != "" {
		st.Label, st.ExpiresAt = id.Label, id.ExpiresAt
	}
	return st
}

// hubTaskTokenMeta asks the hub what h's task credential is, to record
// beside it. It returns why when the hub could not say.
func hubTaskTokenMeta(name string, h *adapter.HubConfig) (meta *adapter.TokenMeta, why string) {
	if _, err := h.StrictClient(time.Second); err != nil {
		return nil, "" // nothing is sent there; hubTaskTokenWarn says why
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sel := &taskcred.Selection{Source: "user-hub", HubName: name, Hub: h, Token: h.TaskToken}
	id, err := sel.Identity(ctx)
	var rejected *taskcred.Rejected
	switch {
	case errors.As(err, &rejected):
		return nil, fmt.Sprintf("the hub refused the token, HTTP %d", rejected.Status)
	case err != nil:
		return nil, redactCred(err.Error(), h.TaskToken)
	case id.Label == "":
		return nil, "the hub does not report token labels; it runs an older aimem"
	}
	return id.Meta(), ""
}

// tokenName names the token by its label and expiry, after its ID when
// the ID is known.
func (st hubCredentialStatus) tokenName() string {
	name := "token"
	if st.Label != "" {
		name += fmt.Sprintf(" %q", st.Label)
	}
	if st.TokenID != "" {
		name += " " + st.TokenID
	}
	switch {
	case st.Label == "" && st.State == "refused":
		name += " (label unknown: not recorded when it was stored)"
	case st.ExpiresAt != "":
		name += ", expires " + st.ExpiresAt
	}
	return name
}

// redactCred removes the credential from an error text, in case a transport
// error ever quotes the request.
func redactCred(msg, cred string) string {
	if cred == "" {
		return msg
	}
	return strings.ReplaceAll(msg, cred, "[redacted]")
}

func (st hubCredentialStatus) text() string {
	switch {
	case st.Credential == "none":
		return fmt.Sprintf("hub %s: individual credential none (install one with: aimem hub task-token %s <token>)", st.Hub, st.Hub)
	case st.Credential == "other":
		return fmt.Sprintf("hub %s: the stored credential is not an individual credential (%s)", st.Hub, st.Detail)
	case st.State == "active":
		return fmt.Sprintf("hub %s: individual credential set, active, scope %s (user %s %s, %s)", st.Hub, st.Scope, st.UserName, st.UserID, st.tokenName())
	case st.State == "untrusted":
		return fmt.Sprintf("hub %s: individual credential set, not sent: aimem will not send it to this entry (%s)", st.Hub, st.Detail)
	case st.State == "refused":
		return fmt.Sprintf("hub %s: individual credential set, refused by the hub (revoked, expired or unknown): %s; an admin issues a new user token (aimem access token-issue-user), then: aimem hub task-token %s --token-file FILE", st.Hub, st.tokenName(), st.Hub)
	}
	return fmt.Sprintf("hub %s: individual credential set, state unknown: the hub is unreachable (%s)", st.Hub, st.Detail)
}
