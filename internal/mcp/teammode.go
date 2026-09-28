package mcp

// Team mode of the stdio facade (task E5a). A process started with
// AIMEM_TEAM_SESSION serves exactly one verified aicrew team context:
//   - the binding (hub, user, service, team, session) is pinned at start;
//   - every hub call re-reads the session file, carries its handle and uses
//     only the installation's user-scoped credential over verified TLS;
//   - a changed binding or a missing file blocks the process for good;
//   - the context is verified online before the first tool and again after
//     any team refusal; until then every tool refuses;
//   - the tool list is the hub's team routes: the task and epic reads, the
//     member reservation tools (C6a), plus session_context.
// Nothing here ever makes a call without the handle, and nothing falls back
// to personal mode, the local socket or a checkout's credential.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/teamsession"
)

// teamReadTools are the task tools whose hub routes team mode serves
// (internal/server teamRoutes); session_context reports the verified context.
var teamReadTools = map[string]bool{
	"list_tasks": true, "get_task": true, "get_task_history": true, "list_task_comments": true,
	"get_task_comment": true, "list_epics": true, "get_epic": true,
}

const sessionContextTool = "session_context"

var sessionContextToolDef = map[string]any{
	"name": sessionContextTool,
	"description": "Report this conversation's verified aicrew team context: the team, your role, the session and generation, and the projects the team may read. " +
		"It is verified online with the hub on every call. Knowledge tools are unavailable in a team conversation.",
	"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
}

// teamToolList is the complete tool list of a team conversation.
func teamToolList() []map[string]any {
	var out []map[string]any
	for _, d := range taskToolDefs {
		if n := d["name"].(string); teamReadTools[n] || reservationTools[n] {
			out = append(out, d)
		}
	}
	return append(out, sessionContextToolDef)
}

type teamMode struct {
	path   string
	root   string
	pinned teamsession.Binding

	mu      sync.Mutex
	blocked error // permanent: the binding changed or the file is unusable
	ready   bool  // verified online since the last refusal
}

// newTeamMode loads the session file once and pins its binding. A file that
// cannot be loaded blocks the process from the start.
func newTeamMode(path, root string) *teamMode {
	tm := &teamMode{path: path, root: root}
	f, err := teamsession.Load(path)
	if err != nil {
		tm.blocked = fmt.Errorf("the team session file cannot be used (%v); nothing was sent to any hub. Restart the conversation through aicrew", err)
		return tm
	}
	tm.pinned = f.Binding()
	return tm
}

// current re-reads the session file and checks it still carries the pinned
// binding. Any failure blocks the process for good.
func (tm *teamMode) current() (teamsession.File, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.blocked != nil {
		return teamsession.File{}, tm.blocked
	}
	f, err := teamsession.Load(tm.path)
	// A refresh replaces the file by rename; on Windows a read that meets
	// the rename can fail for an instant. Retry briefly before deciding the
	// file is gone: a real deletion still blocks within about 100 ms.
	for i := 0; err != nil && i < 5; i++ {
		time.Sleep(20 * time.Millisecond)
		f, err = teamsession.Load(tm.path)
	}
	switch {
	case err != nil:
		tm.blocked = fmt.Errorf("the team session file is gone or unusable (%v); this conversation is blocked. Restart it through aicrew", err)
	case f.Binding() != tm.pinned:
		tm.blocked = errors.New("the team session file now names another hub, user, team or session; this conversation is blocked and never switches context. Restart it through aicrew")
	}
	if tm.blocked != nil {
		return teamsession.File{}, tm.blocked
	}
	return f, nil
}

// hub resolves the pinned hub's credential and verified-TLS client.
func (tm *teamMode) hub(f teamsession.File) (string, *http.Client, error) {
	name, h := adapter.ResolveHub(tm.root, f.Hub)
	if h == nil || name != f.Hub {
		return "", nil, fmt.Errorf("hub %q is not configured on this machine", f.Hub)
	}
	if strings.TrimRight(h.URL, "/") != strings.TrimRight(f.URL, "/") {
		tm.mu.Lock()
		tm.blocked = errors.New("the hub's configured URL changed since this conversation started; it is blocked. Restart it through aicrew")
		tm.mu.Unlock()
		return "", nil, tm.blocked
	}
	cred, err := teamsession.Credential(h)
	if err != nil {
		return "", nil, err
	}
	client, err := teamsession.HubClient(h)
	if err != nil {
		return "", nil, err
	}
	return cred, client, nil
}

// call is the team-mode TaskCallFunc: the pinned session's handle on every
// request, the individual credential, verified TLS.
func (tm *teamMode) call(ctx context.Context, method, path string, headers map[string]string, body []byte) (int, []byte, error) {
	f, err := tm.current()
	if err != nil {
		return 0, nil, err
	}
	cred, client, err := tm.hub(f)
	if err != nil {
		return 0, nil, err
	}
	h := map[string]string{teamsession.Header: f.Handle}
	for k, v := range headers {
		h[k] = v
	}
	status, resp, err := hubCaller(strings.TrimRight(f.URL, "/"), cred, client)(ctx, method, path, h, body)
	if err == nil && status/100 != 2 {
		if r := teamsession.ParseRefusal(status, resp); r != nil {
			tm.mu.Lock()
			tm.ready = false // any team refusal: verify again before the next tool
			tm.mu.Unlock()
		}
	}
	return status, resp, err
}

// verify asks the hub for the context report and checks it belongs to the
// pinned binding. On success the process is ready.
func (tm *teamMode) verify(ctx context.Context) ([]byte, error) {
	f, err := tm.current()
	if err != nil {
		return nil, err
	}
	cred, client, err := tm.hub(f)
	if err != nil {
		return nil, err
	}
	rep, body, err := teamsession.Verify(ctx, client, f.URL, cred, f.Handle)
	if err != nil {
		// A failed verification, from any tool, withdraws the last one.
		tm.mu.Lock()
		tm.ready = false
		tm.mu.Unlock()
		return nil, fmt.Errorf("the team context is not verified; team tools refuse until it is: %w", err)
	}
	if !tm.pinned.Matches(rep) {
		tm.mu.Lock()
		tm.blocked = errors.New("the hub reports a different user, team or session than this conversation is bound to; it is blocked. Restart it through aicrew")
		tm.mu.Unlock()
		return nil, tm.blocked
	}
	tm.mu.Lock()
	tm.ready = true
	tm.mu.Unlock()
	return body, nil
}

// ensureReady verifies online unless the last verification still stands.
func (tm *teamMode) ensureReady(ctx context.Context) error {
	tm.mu.Lock()
	blocked, ready := tm.blocked, tm.ready
	tm.mu.Unlock()
	if blocked != nil {
		return blocked
	}
	if ready {
		return nil
	}
	_, err := tm.verify(ctx)
	return err
}

// teamToolCall serves one tool call of a team conversation.
func (s *srv) teamToolCall(ctx context.Context, name string, raw []byte) (string, error) {
	switch {
	case name == sessionContextTool:
		body, err := s.team.verify(ctx)
		if err != nil {
			return "", err
		}
		return string(body), nil
	case teamReadTools[name], reservationTools[name]:
		if err := s.team.ensureReady(ctx); err != nil {
			return "", err
		}
		return s.taskTool(ctx, name, raw)
	}
	return "", fmt.Errorf("tool %q is not available in a team conversation: team mode serves only the team's task and epic reads, the reservation tools and session_context; knowledge and other write tools are off", name)
}
