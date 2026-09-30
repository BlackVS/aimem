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
	// "unreachable" (no answer), or "absent" when nothing is stored.
	State   string `json:"state"`
	Scope   string `json:"scope,omitempty"` // user, project or read-only, as the hub reports it
	UserID  string `json:"user_id,omitempty"`
	TokenID string `json:"token_id,omitempty"`
	// Detail explains an unreachable hub; it never carries a secret.
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
	client, err := teamsession.HubClient(h)
	if err != nil {
		st.State, st.Detail = "unreachable", err.Error()
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
	var id struct {
		Scope   string `json:"scope"`
		UserID  string `json:"user_id"`
		TokenID string `json:"token_id"`
	}
	if json.Unmarshal(raw, &id) != nil {
		st.State, st.Detail = "unreachable", "the hub's answer is not an identity report"
		return st
	}
	st.State, st.Scope, st.UserID, st.TokenID = "active", id.Scope, id.UserID, id.TokenID
	return st
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
		return fmt.Sprintf("hub %s: individual credential set, active, scope %s (user %s, token %s)", st.Hub, st.Scope, st.UserID, st.TokenID)
	case st.State == "refused":
		return fmt.Sprintf("hub %s: individual credential set, refused by the hub (revoked, expired or unknown); install a new one with: aimem hub task-token %s <token>", st.Hub, st.Hub)
	}
	return fmt.Sprintf("hub %s: individual credential set, state unknown: the hub is unreachable (%s)", st.Hub, st.Detail)
}
