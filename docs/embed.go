// Package docs embeds the documents the aimem binary serves to agents
// directly, from their canonical place in this directory: there is no second
// copy to keep in sync. internal/teamguide builds the team guidance unit
// from TeamGuidance and validates it.
package docs

import "embed"

// TeamGuidance holds the team playbooks and the request templates they link
// to, exactly as committed.
//
//go:embed TEAM-PLAYBOOKS.md examples/team/*.json
var TeamGuidance embed.FS
