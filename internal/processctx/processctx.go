// Package processctx builds the session-start process context: the
// enablement signal and the selected reference from the hub, the set from
// the cache or Git, rendered as the bootstrap the session-start hook
// injects. Every step is bounded and fails to a notice, never to a blocked
// session start. The CLI (`aimem session-start`, `aimem process show`,
// `aimem teams setup`) and the checkout-bound local MCP facade share it.
package processctx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/ident"
	"aimem/internal/process"
	"aimem/internal/taskcred"
)

// hubLookupTimeout bounds each hub contact at session start; the Git
// fetch has its own bound in the process package and is paid only on a
// cache miss.
const hubLookupTimeout = 1500 * time.Millisecond

// Bootstrap builds the session-start process context for the project in
// dir (or projectID when given), with root as the state root: the enablement signal and
// the handbook when the hub says tasks are on and a process reference
// is selected, or an explicit availability notice otherwise. Every step
// is bounded and fails to a notice, never to a blocked session start.
// The returned Set is non-nil only when a process set was obtained. full
// skips the injection budget: the read path the over-budget notice names.
func Bootstrap(dir, root, projectID string, full bool) (string, *process.Set) {
	id := projectID
	if id == "" {
		var err error
		if id, err = ident.ProjectID(dir); err != nil {
			return "", nil
		}
	}
	hubName, err := ident.ProjectHubNameStrict(dir)
	if err != nil {
		return "process context unavailable: " + err.Error(), nil
	}
	_, hub := adapter.ResolveHub(root, hubName)
	if hub == nil {
		return "", nil // no hub: no tasks anywhere; nothing to say
	}
	hubClient := hub.HTTPClient()
	local, err := taskcred.LocalRequired(dir)
	if err != nil {
		return "process context unavailable: " + err.Error(), nil
	}
	if local {
		selected, err := taskcred.Resolve(dir, root)
		if err != nil {
			return "process context unavailable: " + err.Error(), nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), hubLookupTimeout)
		err = selected.Validate(ctx)
		cancel()
		if err != nil {
			return "process context unavailable: " + err.Error(), nil
		}
		copyHub := *hub
		copyHub.Token = selected.Token
		hub = &copyHub
		hubClient = selected.Client()
	}
	lastPath := filepath.Join(root, "process", "last-"+id+".json")
	// 1. Enablement and the selection, from the hub, bounded.
	var identity struct {
		TasksEnabled *bool `json:"tasks_enabled"`
	}
	ierr := hubGetJSONClient(hub, hubClient, "/v1/access/identity?project="+url.QueryEscape(id), &identity)
	var sel struct {
		Current *process.Ref `json:"current"`
	}
	var serr error
	if ierr == nil {
		serr = hubGetJSONClient(hub, hubClient, "/v1/projects/"+url.PathEscape(id)+"/process", &sel)
	}
	var ref *process.Ref
	observed := ""
	switch {
	case ierr != nil:
		// Hub offline or unreachable: availability unknown. Use the last
		// observed selection, saying so, or report unavailable.
		b, rerr := os.ReadFile(lastPath)
		var last struct {
			Ref        process.Ref `json:"ref"`
			ObservedAt string      `json:"observed_at"`
		}
		if rerr != nil || json.Unmarshal(b, &last) != nil {
			return fmt.Sprintf("process context unavailable: hub unreachable (%v) and no last-observed selection on this machine; tasks availability unknown", ierr), nil
		}
		ref, observed = &last.Ref, last.ObservedAt
	case identity.TasksEnabled == nil || !*identity.TasksEnabled:
		return "", nil // tasks off (or an older hub): no process context, by design
	case serr != nil && strings.Contains(serr.Error(), "no process reference"):
		return "Tasks (Kanban) are ON for project " + id + ", but no process reference is selected: process context unavailable until an admin selects one (`aimem process select <repo> <commit> <manifest>` on the hub host).", nil
	case serr != nil:
		return "process context unavailable: " + serr.Error(), nil
	case sel.Current == nil:
		return "Tasks (Kanban) are ON for project " + id + ", but no process reference is selected: process context unavailable until an admin selects one.", nil
	default:
		ref = sel.Current
		if b, err := json.Marshal(map[string]any{"ref": ref, "observed_at": time.Now().UTC().Format(time.RFC3339)}); err == nil {
			os.MkdirAll(filepath.Dir(lastPath), 0o700)
			os.WriteFile(lastPath, b, 0o600)
		}
	}
	// 2. The files, from the cache or Git, bounded.
	ctx, cancel := context.WithTimeout(context.Background(), process.FetchTimeout+time.Second)
	defer cancel()
	res := process.Fetch(ctx, root, *ref)
	switch res.Status {
	case process.StatusDenied:
		return fmt.Sprintf("process context unavailable: access to the process repository was DENIED on this machine (%v); this machine needs read access to %s", res.Err, ref.Repo), nil
	case process.StatusUnavailable:
		return fmt.Sprintf("process context unavailable: %v (selection %s @ %s)", res.Err, ref.Repo, ref.Commit[:12]), nil
	}
	home, _ := os.UserHomeDir()
	var text string
	if full {
		text = process.BootstrapFull(res.Set, id, process.SkillInstalled(dir, home))
	} else {
		var err error
		if text, err = process.Bootstrap(res.Set, id, process.SkillInstalled(dir, home)); err != nil {
			return "process context unavailable: " + err.Error(), res.Set
		}
	}
	if observed != "" {
		text = fmt.Sprintf("NOTE: the hub is unreachable; the process selection below is as last observed at %s and tasks availability is unknown. Cached context never authorizes a task write.\n", observed) + text
	}
	return text, res.Set
}

// hubGetJSONClient uses the selected credential and redirect policy for a
// bounded identity/process read; both routes admit ordinary tokens.
func hubGetJSONClient(hub *adapter.HubConfig, client *http.Client, path string, into any) error {
	ctx, cancel := context.WithTimeout(context.Background(), hubLookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hub.URL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+hub.Token)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("hub unreachable: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("hub returned HTTP %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, into)
}
