package processctx

// The skills routes are read with the same connection the process context
// uses: the checkout's hub binding and its credential, selected strictly.

import (
	"aimem/internal/skills"
)

// SkillsClient connects for the project in dir (or projectID) and returns
// the client for the hub's skills routes. On failure the Result says why,
// in Load's states (unavailable, denied), and the client is nil.
func SkillsClient(dir, root, projectID string) (*skills.Client, *Result) {
	r := &Result{Project: projectID}
	hub, hubClient, ok := r.connect(dir, root)
	if !ok {
		return nil, r
	}
	return &skills.Client{BaseURL: hub.URL, Token: hub.Token, HTTP: hubClient, Project: r.Project}, r
}

// SkillsClient is the client for the connection this result was loaded
// with; nil when the hub was not reached.
func (r *Result) SkillsClient() *skills.Client {
	if r.hub == nil {
		return nil
	}
	return &skills.Client{BaseURL: r.hub.URL, Token: r.hub.Token, HTTP: r.hubClient, Project: r.Project}
}
