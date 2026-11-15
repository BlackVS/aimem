package mcp

// The scope guard of the stdio docs, records and collections tools (security
// audit 2026-10, finding A-9). These tools reach the hub with the machine's
// checkpoint credential, a writer token the hub checks no project grant for,
// under the project, groups and hub that the checkout's .aimem.json names.
// Before that credential is used, the pinned project is confirmed with the
// member's own task credential, whose identity answer the hub gives per
// project. Without a task credential the call proceeds, marked as not
// confirmed; group spaces carry no grants, so a group is always named as
// declared by the repository. Every result names the hub and the credential,
// and the first use of a hub, project and groups from a checkout, or a change
// of any of them, is announced.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"aimem/internal/adapter"
	"aimem/internal/ident"
	"aimem/internal/taskcred"
)

// docScope is what one docs or records call addresses and with which
// credential.
type docScope struct {
	project string // the project or group space the call addresses
	group   bool   // project is a group space
	pinned  string // the checkout's pinned project ("" on a server without one)
	groups  []string
	hubName string
	hub     *adapter.HubConfig // the hub entry: carries the checkpoint credential
	// task is a copy of the hub entry carrying the task credential, set only
	// when that credential confirmed the pinned project.
	task      *adapter.HubConfig
	taskLabel string
	usedTask  bool // the call went out with the task credential
	// alsoRead are the group spaces a project-scoped call read beside the
	// project (the default list_docs), each named in the note as declared.
	alsoRead []string
	root     string
	checkout string
}

// docProject resolves a doc or record tool's scope and confirms it. Shared
// documents are hub-authoritative - the local service holds none - so unlike
// the other tools these talk to the project's hub directly and refuse
// plainly when offline: CAS needs the hub, and the bound file plus the
// checkpoint publisher already cover the offline case. write marks a tool
// that changes the hub.
func (s *srv) docProject(scope string, write bool) (*docScope, error) {
	d := &docScope{project: s.project, pinned: s.project, groups: s.groups, root: mcpStateRoot()}
	if strings.HasPrefix(scope, "group:") {
		id, err := ident.GroupProject(strings.TrimPrefix(scope, "group:"))
		if err != nil {
			return nil, err
		}
		if s.project != "" && !slices.Contains(s.groups, id) {
			return nil, fmt.Errorf("this project has not declared group %q in .aimem.json", strings.TrimPrefix(scope, "group:"))
		}
		d.project, d.group = id, true
	}
	if d.project == "" {
		return nil, fmt.Errorf("a project or group scope is required on this server")
	}
	hubName, err := ident.ProjectHubNameStrict(".")
	if err != nil {
		return nil, err // an invalid or unreadable binding fails loudly, never routes to the default hub
	}
	d.hubName, d.hub = adapter.ResolveHub(d.root, hubName)
	if d.hub == nil {
		return nil, fmt.Errorf("no hub configured - shared documents live on the project's hub; for a bound file, edit it directly and the checkpoint publisher delivers it when a hub is reachable")
	}
	if d.pinned == "" {
		return d, nil // no checkout pins a project here: nothing to confirm or announce
	}
	if abs, err := filepath.Abs(ident.ProjectDir(".")); err == nil {
		d.checkout = abs
	}
	if err := d.confirm(write && !d.group); err != nil {
		return nil, err
	}
	return d, nil
}

// confirm asks the hub, with the member's task credential, whether the pinned
// project is one that credential may read (and, for a write, write). Only a
// readable identity answer that names a user confirms; a 404 refuses, and
// any other answer (a refused credential, a server error, a timeout, a body
// that cannot be read) refuses too, since nothing confirmed the scope. The
// call proceeds unconfirmed only when this hub entry holds no task
// credential at all.
func (d *docScope) confirm(write bool) error {
	sel, err := taskcred.Resolve(".", d.root)
	if err != nil {
		if d.hub.TaskToken == "" && taskcred.Classify(err) == taskcred.ClassMissing {
			return nil // no task credential: proceed, marked not confirmed
		}
		return d.refuse(fmt.Sprintf("the task credential cannot be used (%v)", err))
	}
	if sel.Project != d.pinned || sel.HubName != d.hubName {
		return d.refuse(fmt.Sprintf("the task credential is bound to project %s on hub %q", sel.Project, sel.HubName))
	}
	ctx, cancel := context.WithTimeout(context.Background(), taskCheckTimeout)
	defer cancel()
	id, err := sel.Identity(ctx)
	switch {
	case errors.Is(err, taskcred.ErrProjectHidden):
		return d.refuse("your task credential may not read this project (no grant, or a token scoped to another project); an operator grants it with `aimem access grant add`")
	case err != nil:
		return d.refuse(fmt.Sprintf("the project could not be confirmed: %v", err))
	case id.UserID == "" || id.TokenID == "":
		return d.refuse("the hub's identity answer names no user, so it confirms nothing")
	case write && !id.TaskWrite:
		return d.refuse("your task credential may read this project but not write it (a read grant, or a token scoped to another project)")
	}
	t := *sel.Hub
	t.Token = sel.Token
	t.Strict = true // the task credential travels only over the strict client
	d.task, d.taskLabel = &t, id.Label
	return nil
}

func (d *docScope) refuse(why string) error {
	return fmt.Errorf("refused: project %s on hub %q was not confirmed for this checkout: %s. Nothing was sent with the machine's checkpoint credential", d.pinned, d.hubName, why)
}

// confirmed reports whether the task credential confirmed the pinned project.
func (d *docScope) confirmed() bool { return d.task != nil }

// readHub is the hub entry a read of the project's own documents goes out
// with: the task credential when it confirmed the project (the hub checks
// its grant on the list and read routes), else the checkpoint credential.
// A group space is never readable with a task credential, so a group read
// keeps the checkpoint credential.
func (d *docScope) readHub(scope string) *adapter.HubConfig {
	if scope != d.project && !slices.Contains(d.alsoRead, scope) {
		d.alsoRead = append(d.alsoRead, scope)
	}
	if d.task != nil && scope == d.pinned {
		d.usedTask = true
		return d.task
	}
	return d.hub
}

// note names the hub, the credential and how the scope was established.
func (d *docScope) note() string {
	cred := "the machine's checkpoint credential"
	if d.usedTask {
		cred = "your task credential"
		if d.taskLabel != "" {
			cred += fmt.Sprintf(" %q", d.taskLabel)
		}
		if len(d.alsoRead) > 0 {
			cred += " (group spaces: the machine's checkpoint credential)"
		}
	}
	var scope string
	switch {
	case d.group && d.confirmed():
		scope = fmt.Sprintf("group space %s as declared by this repository's .aimem.json, not confirmed (no grant covers a group); project %s confirmed", d.project, d.pinned)
	case d.group:
		scope = fmt.Sprintf("group space %s as declared by this repository's .aimem.json, not confirmed", d.project)
	case d.confirmed():
		scope = fmt.Sprintf("project %s, confirmed by your task credential", d.project)
	default:
		scope = fmt.Sprintf("project %s from this repository's .aimem.json; scope not confirmed (hub %q holds no task credential)", d.project, d.hubName)
	}
	if len(d.alsoRead) > 0 {
		scope += fmt.Sprintf("; group spaces %s as declared by this repository's .aimem.json, not confirmed", strings.Join(d.alsoRead, ", "))
	}
	return fmt.Sprintf("[aimem: hub %q (%s), %s; %s]", d.hubName, d.hub.URL, cred, scope)
}

// finish puts the first-use announcement before a successful result and the
// note after any result, and records what was announced.
func (d *docScope) finish(msg string, err error) (string, error) {
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, d.note())
	}
	return d.announce() + msg + "\n" + d.note(), nil
}

// scopeSeen is what a checkout last used, kept in the state root.
type scopeSeen struct {
	Hub     string   `json:"hub"`
	URL     string   `json:"url"`
	Project string   `json:"project"`
	Groups  []string `json:"groups,omitempty"`
}

const scopeSeenFile = "doc-scope-seen.json"

// announce returns the first-use line for this checkout, or a change line
// when its hub, project or groups differ from the last ones recorded, and
// records the current ones. It returns "" when nothing changed. Without a
// task credential this line is the only signal a person gets, so a failure
// to record repeats the announcement rather than dropping it.
func (d *docScope) announce() string {
	if d.checkout == "" {
		return ""
	}
	cur := scopeSeen{Hub: d.hubName, URL: d.hub.URL, Project: d.pinned, Groups: slices.Clone(d.groups)}
	slices.Sort(cur.Groups)
	path := filepath.Join(d.root, scopeSeenFile)
	var seen map[string]scopeSeen
	if raw, err := os.ReadFile(path); err != nil || json.Unmarshal(raw, &seen) != nil || seen == nil {
		seen = map[string]scopeSeen{} // a missing, unreadable or null record announces again
	}
	prev, had := seen[d.checkout]
	if had && prev.Hub == cur.Hub && prev.URL == cur.URL && prev.Project == cur.Project && slices.Equal(prev.Groups, cur.Groups) {
		return ""
	}
	seen[d.checkout] = cur
	if raw, err := json.MarshalIndent(seen, "", "  "); err == nil {
		tmp := path + ".tmp"
		if os.WriteFile(tmp, raw, 0o600) == nil {
			os.Rename(tmp, path)
		}
	}
	line := fmt.Sprintf("hub %q (%s), project %s", cur.Hub, cur.URL, orNone(cur.Project))
	line += ", groups " + orNone(strings.Join(cur.Groups, ", "))
	if had {
		was := fmt.Sprintf("hub %q (%s), project %s, groups %s", prev.Hub, prev.URL, orNone(prev.Project), orNone(strings.Join(prev.Groups, ", ")))
		return fmt.Sprintf("[aimem: this checkout's shared-docs scope CHANGED (from .aimem.json): now %s; was %s]\n", line, was)
	}
	return fmt.Sprintf("[aimem: first use of shared docs and records from this checkout (from .aimem.json): %s]\n", line)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
