package wiring

import "strings"

// ProjectFile is one path Wire writes into a checkout, and whether a
// project commits it or keeps it out of Git.
type ProjectFile struct {
	Path  string // slash-separated, relative to the checkout
	Local bool   // kept out of Git: it belongs to one machine or one member
}

// ProjectFiles is every path Wire writes into a checkout. It is the one
// source for the file list in wire's closing line, in docs/INSTALL-CLIENT.md
// ("What wiring a project adds to it") and in docs/QUICKSTART-NEW-PROJECT.md
// (step 3.5): a test holds Wire to it, and another holds both documents.
var ProjectFiles = []ProjectFile{
	{Path: "AGENTS.md"},
	{Path: "CLAUDE.md"},
	{Path: ".mcp.json"},
	{Path: "opencode.json"},
	{Path: ".claude/settings.json"},
	{Path: ".codex/hooks.json"},
	{Path: ".claude/commands/aimem-analyze.md"},
	{Path: ".opencode/command/aimem-analyze.md"},
	{Path: ".claude/skills/join_team/SKILL.md"},
	{Path: ".claude/skills/resume_team/SKILL.md"},
	{Path: ".agents/skills/join-team/SKILL.md"},
	{Path: ".agents/skills/resume-team/SKILL.md"},
	{Path: ".opencode/commands/join_team.md"},
	{Path: ".opencode/commands/resume_team.md"},
	{Path: ".aimem.json", Local: true},
	{Path: "docs/SESSION-STATE.md", Local: true},
}

// GitignoreLines are the .gitignore lines that keep a wired project's
// local files out of Git: the local ProjectFiles, the handoff's archive,
// Claude Code's personal settings and the skills `aimem skills install`
// puts in place. The skills directories also hold the committed team entry
// points, which the ! lines keep.
var GitignoreLines = []string{
	".aimem.json",
	"docs/SESSION-STATE.md",
	"docs/SESSION-STATE-ARCHIVE.md",
	".claude/settings.local.json",
	".claude/skills/*",
	"!.claude/skills/join_team/",
	"!.claude/skills/resume_team/",
	".agents/skills/*",
	"!.agents/skills/join-team/",
	"!.agents/skills/resume-team/",
	".opencode/skills/",
	".openhands/skills/",
}

// WiredLine is wire's closing line: what to commit and what to keep local,
// in the terms of docs/QUICKSTART-NEW-PROJECT.md, step 3.5.
func WiredLine() string {
	var local []string
	for _, f := range ProjectFiles {
		if f.Local {
			local = append(local, f.Path)
		}
	}
	return "project wired. If the project is tracked, commit the files above except " +
		strings.Join(local, " and ") +
		", which stay local (the .gitignore lines: docs/QUICKSTART-NEW-PROJECT.md, step 3.5)."
}
