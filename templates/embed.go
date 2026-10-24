// Package templates embeds the files `aimem wire` copies into a newly wired
// project, from their canonical place in this directory.
package templates

import _ "embed"

// AgentsMD is the session handoff protocol a wired project starts with.
//
//go:embed AGENTS.md
var AgentsMD []byte
