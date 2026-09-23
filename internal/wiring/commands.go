package wiring

import (
	_ "embed"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Each entry point is one body, rendered once per client with that
// client's frontmatter and names. The bodies live in the binary so every
// checkout gets the same text and an upgrade refreshes it.
//
//go:embed assets/join_team.md
var joinTeamBody string

//go:embed assets/resume_team.md
var resumeTeamBody string

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

const resumeTeamDescription = "Continue the aimem team membership this checkout already holds after a client restart or compaction (aimem teams continue): verify or resume the session and take up the reserved attempt and unacknowledged inbox. Use only when the user asks to resume, continue or pick up team work; it never joins."

func render(body, frontmatter, platform, versionCmd string) string {
	body = strings.ReplaceAll(body, "{{PLATFORM}}", platform)
	body = strings.ReplaceAll(body, "{{VERSION_CMD}}", versionCmd)
	return frontmatter + "\n" + managedMarker + "\n\n" + body
}

// entryPoint describes one command in every client's terms.
type entryPoint struct {
	body                            string
	claudeName, codexName, fileName string // join_team / join-team / join_team
	description, argumentHint       string
	allowedTools                    string
}

func (e entryPoint) assets() []commandAsset {
	claude := "---\nname: " + e.claudeName + "\ndescription: " + e.description + "\nargument-hint: " + e.argumentHint + "\nallowed-tools: " + e.allowedTools + "\ndisable-model-invocation: true\n---\n"
	opencode := "---\ndescription: " + e.description + "\n---\n"
	codexSkill := "---\nname: " + e.codexName + "\ndescription: " + e.description + " Invoked as $" + e.codexName + " " + e.argumentHint + ".\n---\n"
	codexPrompt := "---\ndescription: " + e.description + "\nargument-hint: " + e.argumentHint + "\n---\n"
	return []commandAsset{
		{Rel: filepath.Join(".claude", "skills", e.claudeName, "SKILL.md"), Label: ".claude/skills/" + e.claudeName + "/SKILL.md (/" + e.claudeName + " in Claude Code)", Content: render(e.body, claude, "claude-code", "claude --version")},
		{Rel: filepath.Join(".opencode", "commands", e.fileName+".md"), Label: ".opencode/commands/" + e.fileName + ".md (/" + e.fileName + " in OpenCode)", Content: render(e.body, opencode, "opencode", "opencode --version")},
		{Rel: filepath.Join(".agents", "skills", e.codexName, "SKILL.md"), Label: ".agents/skills/" + e.codexName + "/SKILL.md ($" + e.codexName + " in Codex)", Content: render(e.body, codexSkill, "codex", "codex --version")},
		{Rel: filepath.Join(".codex", "prompts", e.fileName+".md"), Label: "~/.codex/prompts/" + e.fileName + ".md (/prompts:" + e.fileName + " in Codex)", User: true, Content: render(e.body, codexPrompt, "codex", "codex --version")},
	}
}

func commandAssets() []commandAsset {
	join := entryPoint{body: joinTeamBody, claudeName: "join_team", codexName: "join-team", fileName: "join_team", description: joinTeamDescription, argumentHint: "TEAM [worker|coordinator]", allowedTools: "Bash(aimem teams setup *) Bash(aimem teams mine *) Bash(claude --version)"}
	resume := entryPoint{body: resumeTeamBody, claudeName: "resume_team", codexName: "resume-team", fileName: "resume_team", description: resumeTeamDescription, argumentHint: "[TEAM]", allowedTools: "Bash(aimem teams continue *) Bash(git status *)"}
	return append(join.assets(), resume.assets()...)
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
