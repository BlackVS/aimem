package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"aimem/internal/adapter"
)

// hubAddProbeTimeout bounds the identity call `aimem hub add` makes.
var hubAddProbeTimeout = 10 * time.Second

// hubAddWarn is what `aimem hub add` runs after storing an entry; the
// package's tests keep it off the network unless a test restores it.
var hubAddWarn = hubAddWarnings

// hubAddWarnings prints what is risky about a stored hub entry: a URL that
// is not https, --insecure, and a token that authenticates as role admin
// (one GET /v1/access/identity through the entry's own trust). It only
// warns: the entry is already stored, and an unreachable hub is a warning
// too.
func hubAddWarnings(w io.Writer, name string, h *adapter.HubConfig) {
	if !strings.HasPrefix(strings.ToLower(h.URL), "https://") {
		fmt.Fprintf(w, "WARNING: hub %s is not https: its token crosses the network in clear.\n", name)
	}
	if h.Insecure {
		fmt.Fprintf(w, "WARNING: --insecure skips certificate verification for hub %s: anyone on the path can pose as the hub and take its token. Prefer --pin or --ca-file.\n", name)
	}
	role, who, err := hubTokenRole(h)
	switch {
	case err != nil:
		fmt.Fprintf(w, "WARNING: could not check the role of hub %s's token: %s. The entry is stored.\n", name, redactCred(err.Error(), h.Token))
	case role != "admin":
	case who == "env":
		fmt.Fprintf(w, "WARNING: this is hub %s's env token (AIMEM_HTTP_TOKEN), its admin credential: it belongs on no client. On the hub, run `aimem token add <machine>` and add the hub again with that writer token.\n", name)
	default:
		fmt.Fprintf(w, "WARNING: the token for hub %s authenticates as role admin (token %q): this machine can change the hub's configuration. A client normally holds a writer token (`aimem token add <machine>` on the hub).\n", name, who)
	}
}

// hubTokenRole asks the hub who its token is. It follows no redirect, so
// the token goes only to the configured URL.
func hubTokenRole(h *adapter.HubConfig) (role, name string, err error) {
	client := h.Client(hubAddProbeTimeout)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(context.Background(), hubAddProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(h.URL, "/")+"/v1/access/identity", nil)
	if err != nil {
		return "", "", fmt.Errorf("invalid hub URL")
	}
	req.Header.Set("Authorization", "Bearer "+h.Token)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("the hub is unreachable (%v)", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return "", "", fmt.Errorf("the hub refused the token (HTTP %d)", resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return "", "", fmt.Errorf("the hub answered HTTP %d", resp.StatusCode)
	}
	var id struct {
		Name string `json:"name"`
		Role string `json:"role"`
	}
	if json.Unmarshal(raw, &id) != nil || id.Role == "" {
		return "", "", fmt.Errorf("the hub's answer is not an identity report")
	}
	return id.Role, id.Name, nil
}

// hubWarnOut receives `aimem hub task-token`'s warning; tests replace it.
var hubWarnOut io.Writer = os.Stderr

// hubTaskTokenWarn says when a task credential was stored on an entry the
// strict client refuses (not https, --insecure, or a trust that does not
// load): it is kept, but no path will send it there. Offline.
func hubTaskTokenWarn(w io.Writer, name string, h *adapter.HubConfig) {
	if _, err := h.StrictClient(time.Second); err != nil {
		fmt.Fprintf(w, "WARNING: hub %s: the task credential is stored, but aimem will not send it to this entry: %s\n", name, err)
	}
}
