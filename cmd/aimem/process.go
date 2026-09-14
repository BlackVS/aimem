package main

// The process reference: which Git repository, commit and manifest hold
// the handbook, checklists and templates a project's agents follow
// (docs/DESIGN-kanban-docs.md). `aimem process select|clear` run on the
// hub host as the admin; `aimem process show` runs on any machine and
// prints exactly what the session-start hook injects, with the same
// availability notices, so a person can see what an agent saw.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
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
)

func processCmd(args []string) error {
	usage := `usage: aimem process select <repo> <commit> <manifest> [--ref <branch|tag>] [--expect <commit>] [-p <project>]
       aimem process clear --expect <commit> [-p <project>]
       aimem process show [--full] [--template <kind>] [-p <project>]

select/clear run on the hub host (admin); show runs anywhere and prints
what the session-start hook injects for this project.`
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	switch args[0] {
	case "select", "clear":
		fs := flag.NewFlagSet("process "+args[0], flag.ExitOnError)
		p := fs.String("p", "", "project id (default: current directory's project)")
		ref := fs.String("ref", "", "branch or tag to fetch when the server refuses fetch-by-hash")
		expect := fs.String("expect", "", "the commit currently selected (compare-and-swap); empty when none")
		var pos []string
		rest := args[1:]
		for len(rest) > 0 { // flags after the positionals too
			if strings.HasPrefix(rest[0], "-") {
				fs.Parse(rest)
				rest = fs.Args()
				continue
			}
			pos = append(pos, rest[0])
			rest = rest[1:]
		}
		proj, err := projectOrCurrent(*p)
		if err != nil {
			return err
		}
		body := map[string]any{"expected_commit": *expect}
		if args[0] == "clear" {
			body["clear"] = true
		} else {
			if len(pos) != 3 {
				return fmt.Errorf("%s", usage)
			}
			body["repo"], body["commit"], body["manifest"] = pos[0], pos[1], pos[2]
			if *ref != "" {
				body["ref"] = *ref
			}
		}
		raw, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPut, "http://aimem/v1/projects/"+url.PathEscape(proj)+"/process", bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client().Do(req)
		if err != nil {
			return fmt.Errorf("%w (is the local aimem service running?)", err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
		}
		fmt.Println(strings.TrimSpace(string(data)))
		return nil
	case "show":
		fs := flag.NewFlagSet("process show", flag.ExitOnError)
		p := fs.String("p", "", "project id (default: current directory's project)")
		tmpl := fs.String("template", "", "print this template from the process set instead of the bootstrap")
		full := fs.Bool("full", false, "print the whole unit regardless of the manifest's injection budget")
		fs.Parse(args[1:])
		// A named project is still resolved through this directory's hub
		// binding: the reference lives on the hub the project is bound to.
		text, set := processBootstrap(".", *p, *full)
		if *tmpl != "" {
			if set == nil {
				return errors.New("process set unavailable: " + strings.TrimSpace(text))
			}
			raw, ok := set.Templates[*tmpl]
			if !ok {
				return fmt.Errorf("no template %q in the process set", *tmpl)
			}
			os.Stdout.Write(raw)
			fmt.Println()
			return nil
		}
		fmt.Println(strings.TrimSpace(text))
		return nil
	}
	return fmt.Errorf("%s", usage)
}

func projectOrCurrent(p string) (string, error) {
	if p != "" {
		return p, nil
	}
	return ident.ProjectID(".")
}

// hubLookupTimeout bounds each hub contact at session start; the Git
// fetch has its own bound in the process package and is paid only on a
// cache miss.
const hubLookupTimeout = 1500 * time.Millisecond

// processBootstrap builds the session-start process context for the
// project in dir (or projectID when given): the enablement signal and
// the handbook when the hub says tasks are on and a process reference
// is selected, or an explicit availability notice otherwise. Every step
// is bounded and fails to a notice, never to a blocked session start.
// The returned Set is non-nil only when a process set was obtained. full
// skips the injection budget: the read path the over-budget notice names.
func processBootstrap(dir, projectID string, full bool) (string, *process.Set) {
	root := stateRoot()
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
	lastPath := filepath.Join(root, "process", "last-"+id+".json")
	// 1. Enablement and the selection, from the hub, bounded.
	var identity struct {
		TasksEnabled *bool `json:"tasks_enabled"`
	}
	ierr := hubGetJSON(hub, "/v1/access/identity?project="+url.QueryEscape(id), &identity)
	var sel struct {
		Current *process.Ref `json:"current"`
	}
	var serr error
	if ierr == nil {
		serr = hubGetJSON(hub, "/v1/projects/"+url.PathEscape(id)+"/process", &sel)
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

// hubGetJSON performs one bounded, authenticated GET against a hub with
// the checkpoint token (the identity route and the process reference read
// admit every credential class).
func hubGetJSON(hub *adapter.HubConfig, path string, into any) error {
	ctx, cancel := context.WithTimeout(context.Background(), hubLookupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(hub.URL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+hub.Token)
	resp, err := hub.HTTPClient().Do(req)
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

// processNotice is the session-start hook's slice: the bootstrap, or the
// availability notice, as its own block after the handoff.
func processNotice() string {
	text, _ := processBootstrap(".", "", false)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return "\n\n--- Process context (docs/DESIGN-kanban-docs.md) ---\n" + strings.TrimSpace(text)
}
