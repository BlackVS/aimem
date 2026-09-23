package wiring

import (
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// The /join_team entry point is one body, rendered once per client with
// that client's frontmatter and names. The body lives in the binary so
// every checkout gets the same text and an upgrade refreshes it.
//
//go:embed assets/join_team.md
var joinTeamBody string

// managedMarker heads every generated asset; a file carrying it is owned
// by aimem and is rewritten whenever the rendered content changes.
const managedMarker = "<!-- managed by aimem (`aimem teams commands`): regenerated from the aimem binary; change the source in the aimem repository, not this file -->"

const joinTeamDescription = "Join an aimem team from this checkout as worker or coordinator with verified onboarding (aimem teams setup). Use only when the user asks to join a team, become the coordinator or a worker, or onboard into the team pilot."

// commandAsset is one rendered entry point at one path. User is true for
// the one client whose native prompts live only under the home directory.
type commandAsset struct {
	Rel     string // path relative to the checkout, or to home when User
	Label   string // how the report names it
	User    bool
	Content string
}

func renderJoinTeam(frontmatter, platform, versionCmd string) string {
	body := strings.ReplaceAll(joinTeamBody, "{{PLATFORM}}", platform)
	body = strings.ReplaceAll(body, "{{VERSION_CMD}}", versionCmd)
	return frontmatter + "\n" + managedMarker + "\n\n" + body
}

func commandAssets() []commandAsset {
	claude := "---\nname: join_team\ndescription: " + joinTeamDescription + "\nargument-hint: TEAM [worker|coordinator]\nallowed-tools: Bash(aimem teams setup *) Bash(claude --version)\ndisable-model-invocation: true\n---\n"
	opencode := "---\ndescription: " + joinTeamDescription + "\n---\n"
	codexSkill := "---\nname: join-team\ndescription: " + joinTeamDescription + " Invoked as $join-team TEAM [worker|coordinator].\n---\n"
	codexPrompt := "---\ndescription: " + joinTeamDescription + "\nargument-hint: TEAM [worker|coordinator]\n---\n"
	return []commandAsset{
		{Rel: filepath.Join(".claude", "skills", "join_team", "SKILL.md"), Label: ".claude/skills/join_team/SKILL.md (/join_team in Claude Code)", Content: renderJoinTeam(claude, "claude-code", "claude --version")},
		{Rel: filepath.Join(".opencode", "commands", "join_team.md"), Label: ".opencode/commands/join_team.md (/join_team in OpenCode)", Content: renderJoinTeam(opencode, "opencode", "opencode --version")},
		{Rel: filepath.Join(".agents", "skills", "join-team", "SKILL.md"), Label: ".agents/skills/join-team/SKILL.md ($join-team in Codex)", Content: renderJoinTeam(codexSkill, "codex", "codex --version")},
		{Rel: filepath.Join(".codex", "prompts", "join_team.md"), Label: "~/.codex/prompts/join_team.md (/prompts:join_team in Codex)", User: true, Content: renderJoinTeam(codexPrompt, "codex", "codex --version")},
	}
}

// InstallCommands writes or refreshes the /join_team entry points for the
// checkout at dir: the Claude Code skill, the OpenCode command and the
// Codex skill inside the checkout, and the Codex custom prompt under home
// when a Codex home (`~/.codex`) exists there. A file is rewritten only
// when its rendered content differs; with write false the state is only
// reported. A file at one of these paths that does not carry the managed
// marker is left alone and reported.
func InstallCommands(dir, home string, write bool) []Finding {
	var out []Finding
	for _, a := range commandAssets() {
		base := dir
		if a.User {
			if home == "" {
				continue
			}
			if fi, err := os.Stat(filepath.Join(home, ".codex")); err != nil || !fi.IsDir() {
				out = append(out, Finding{File: a.Label, Level: "ok", Detail: "skipped: no Codex home directory on this machine"})
				continue
			}
			base = home
		}
		path := filepath.Join(base, a.Rel)
		current, err := os.ReadFile(path)
		switch {
		case err == nil && string(current) == a.Content:
			out = append(out, Finding{File: a.Label, Level: "ok", Detail: "command asset present and current"})
			continue
		case err == nil && !strings.Contains(string(current), managedMarker):
			out = append(out, Finding{File: a.Label, Level: "warn", Detail: "a file of that name exists and is not managed by aimem; left as is", Fix: "move it aside to receive the /join_team entry point"})
			continue
		case err != nil && !errors.Is(err, os.ErrNotExist):
			out = append(out, Finding{File: a.Label, Level: "fail", Detail: "unreadable: " + err.Error()})
			continue
		}
		state := "missing"
		if err == nil {
			state = "outdated"
		}
		if !write {
			out = append(out, Finding{File: a.Label, Level: "warn", Detail: "command asset " + state, Fix: "run `aimem teams commands` (or setup without --no-repair) to write it"})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			out = append(out, Finding{File: a.Label, Level: "fail", Detail: "cannot create the directory: " + err.Error()})
			continue
		}
		if err := writeAtomic(path, []byte(a.Content), 0o644); err != nil {
			out = append(out, Finding{File: a.Label, Level: "fail", Detail: "cannot write: " + err.Error()})
			continue
		}
		out = append(out, Finding{File: a.Label, Level: "ok", Detail: "command asset written (" + state + ")", Repaired: true})
	}
	return out
}
