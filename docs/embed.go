// Package docs embeds the documents the aimem binary serves to agents
// directly, from their canonical place in this directory: there is no second
// copy to keep in sync. internal/teamguide builds the team guidance unit
// from TeamGuidance and validates it; internal/writingrule serves the rule
// for kept text.
package docs

import "embed"

// TeamGuidance holds the team playbooks, the rule for kept text that every
// team role reads, and the request templates the playbooks link to, exactly
// as committed.
//
//go:embed TEAM-PLAYBOOKS.md WRITING-PERSISTED-TEXT.md examples/team/*.json
var TeamGuidance embed.FS

// WritingRuleFile is the rule for text agents keep (write-tool payloads and
// saved Markdown), as committed.
const WritingRuleFile = "WRITING-PERSISTED-TEXT.md"

// WritingRule is that file's content.
//
//go:embed WRITING-PERSISTED-TEXT.md
var WritingRule []byte
