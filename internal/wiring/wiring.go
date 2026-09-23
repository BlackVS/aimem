// Package wiring checks, and repairs where aimem owns the entry, the files
// that connect a project checkout to the agent clients: the SessionStart
// handoff hooks for Claude Code and Codex, the MCP registrations for Claude
// Code and OpenCode, the OpenCode instructions, and the handoff file. It
// writes the same shapes the installers write, adds only what is missing,
// never replaces an entry that differs, and never touches a key it does
// not own. It also reports which clients are on PATH and where each one
// finds a required skill.
package wiring

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Finding is one verified fact about one file or client. Level is ok,
// warn or fail; Repaired says the fact holds because this run made it so.
type Finding struct {
	File     string `json:"file"`
	Level    string `json:"level"`
	Detail   string `json:"detail"`
	Fix      string `json:"fix,omitempty"`
	Repaired bool   `json:"repaired,omitempty"`
}

// Client is an agent client found on PATH.
type Client struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Version string `json:"version"`
}

// SkillStatus is where one required skill was found for one client, or
// that it was not.
type SkillStatus struct {
	Skill  string `json:"skill"`
	Client string `json:"client"`
	Path   string `json:"path,omitempty"` // empty when not found
	Fix    string `json:"fix,omitempty"`
}

// Report is the outcome of one Check.
type Report struct {
	Findings []Finding     `json:"findings"`
	Clients  []Client      `json:"clients"`
	Skills   []SkillStatus `json:"skills,omitempty"`
}

// Failed reports whether any finding blocks.
func (r Report) Failed() bool {
	for _, f := range r.Findings {
		if f.Level == "fail" {
			return true
		}
	}
	return false
}

// Options control one Check.
type Options struct {
	Repair                bool     // add missing aimem entries; false reports only
	AllowProjectStopHooks bool     // do not fail on project-level Stop/StopFailure/PreCompact hooks
	RequiredSkills        []string // skills the selected process requires
	Home                  string   // user home for user-level skill locations; "" skips them
	ClientVersions        bool     // run `<client> --version` (bounded) for detected clients
}

// sessionStartMarkers are the two spellings the installers write for the
// handoff hook; either one satisfies the check.
var sessionStartMarkers = []string{"aimem session-start", "SESSION-STATE.md"}

// sessionStartCommand is the portable spelling (install.ps1 and Codex use
// it; it runs under cmd.exe and without a shell).
const sessionStartCommand = "aimem session-start"

// stopHooks at project level journal every turn twice (they belong to the
// user-level install).
var stopHooks = []string{"Stop", "StopFailure", "PreCompact"}

const handoffTemplate = `# Session State

Updated: (date) | branch: (branch) | HEAD: (sha) | by: (client/session)

## Objective

(current objective)

## Next actions (ready)

1. (first action)

## Pick up here

(one line)
`

// Check inspects the checkout at dir.
func Check(dir string, o Options) Report {
	var r Report
	r.Findings = append(r.Findings, checkHandoff(dir, o.Repair))
	r.Findings = append(r.Findings, checkHooks(filepath.Join(dir, ".claude", "settings.json"), "Claude Code", o)...)
	r.Findings = append(r.Findings, checkHooks(filepath.Join(dir, ".codex", "hooks.json"), "Codex", o)...)
	r.Findings = append(r.Findings, checkMCPJSON(filepath.Join(dir, ".mcp.json"), o.Repair))
	r.Findings = append(r.Findings, checkOpenCode(filepath.Join(dir, "opencode.json"), o.Repair)...)
	r.Clients = detectClients(o.ClientVersions)
	r.Skills = skillReport(dir, o.Home, r.Clients, o.RequiredSkills)
	return r
}

func checkHandoff(dir string, repair bool) Finding {
	path := filepath.Join(dir, "docs", "SESSION-STATE.md")
	rel := "docs/SESSION-STATE.md"
	if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
		return Finding{File: rel, Level: "ok", Detail: "handoff file present"}
	}
	if !repair {
		return Finding{File: rel, Level: "warn", Detail: "handoff file missing", Fix: "run without --no-repair to create the template"}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Finding{File: rel, Level: "fail", Detail: "cannot create docs/: " + err.Error()}
	}
	if err := writeAtomic(path, []byte(handoffTemplate), 0o644); err != nil {
		return Finding{File: rel, Level: "fail", Detail: "cannot write the template: " + err.Error()}
	}
	return Finding{File: rel, Level: "ok", Detail: "handoff template created", Repaired: true}
}

// object is one JSON object with its members kept as raw text, so a
// repair rewrites only the members it adds and leaves the rest as they
// were (member order follows the key order on output).
type object map[string]json.RawMessage

// readObject parses path as a JSON object. A missing file is an empty
// object; a BOM is tolerated; anything that is not an object is an error
// and is never overwritten.
func readObject(path string) (object, os.FileMode, bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return object{}, 0o644, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, 0, false, err
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(raw)) == 0 {
		return object{}, fi.Mode().Perm(), true, nil
	}
	var o object
	if err := json.Unmarshal(raw, &o); err != nil || o == nil {
		return nil, fi.Mode().Perm(), true, errors.New("not a JSON object")
	}
	return o, fi.Mode().Perm(), true, nil
}

func writeObject(path string, o object, mode os.FileMode) error {
	out, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, append(out, '\n'), mode)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".aimem-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// hookEntry is the shape both clients use inside hooks.<Event>.
type hookEntry struct {
	Hooks []struct {
		Command string `json:"command"`
	} `json:"hooks"`
}

// checkHooks verifies the SessionStart handoff hook in a Claude Code
// settings.json or a Codex hooks.json, and refuses project-level
// checkpoint hooks.
func checkHooks(path, client string, o Options) []Finding {
	rel := relName(path)
	obj, mode, _, err := readObject(path)
	if err != nil {
		return []Finding{{File: rel, Level: "fail", Detail: "unreadable: " + err.Error(), Fix: "repair the file by hand; nothing was written"}}
	}
	var hooks object
	if raw, ok := obj["hooks"]; ok {
		if json.Unmarshal(raw, &hooks) != nil || hooks == nil {
			return []Finding{{File: rel, Level: "fail", Detail: "hooks is not a JSON object", Fix: "repair the file by hand; nothing was written"}}
		}
	} else {
		hooks = object{}
	}
	var out []Finding
	var present []string
	for _, ev := range stopHooks {
		raw, ok := hooks[ev]
		if !ok {
			continue
		}
		// Decoded, not text-matched: `[ ]`, a multi-line empty array and
		// null hold no hook.
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) != nil || len(list) > 0 {
			present = append(present, ev)
		}
	}
	if len(present) > 0 {
		level := "fail"
		fix := "move these hooks to the user-level install (they are registered there already) or re-run with --allow-project-stop-hooks"
		if o.AllowProjectStopHooks {
			level, fix = "warn", ""
		}
		out = append(out, Finding{File: rel, Level: level, Detail: client + " project-level " + strings.Join(present, ", ") + " hook(s) would journal every turn twice", Fix: fix})
	}
	var entries []hookEntry
	if raw, ok := hooks["SessionStart"]; ok {
		if json.Unmarshal(raw, &entries) != nil {
			return append(out, Finding{File: rel, Level: "fail", Detail: "hooks.SessionStart is not an array", Fix: "repair the file by hand; nothing was written"})
		}
	}
	if hasHandoffHook(entries) {
		return append(out, Finding{File: rel, Level: "ok", Detail: client + " SessionStart handoff hook present"})
	}
	if !o.Repair {
		return append(out, Finding{File: rel, Level: "warn", Detail: client + " SessionStart handoff hook missing", Fix: "run without --no-repair to add it"})
	}
	var list []json.RawMessage
	if raw, ok := hooks["SessionStart"]; ok {
		json.Unmarshal(raw, &list)
	}
	entry, _ := json.Marshal(map[string]any{"hooks": []map[string]any{{"type": "command", "command": sessionStartCommand, "timeout": 10, "statusMessage": "Loading session handoff"}}})
	list = append(list, entry)
	hooks["SessionStart"], _ = json.Marshal(list)
	obj["hooks"], _ = json.Marshal(hooks)
	if err := writeObject(path, obj, mode); err != nil {
		return append(out, Finding{File: rel, Level: "fail", Detail: "cannot write: " + err.Error()})
	}
	return append(out, Finding{File: rel, Level: "ok", Detail: client + " SessionStart handoff hook added", Repaired: true})
}

// hasHandoffHook reports whether any hook command carries one of the
// installer spellings of the handoff hook.
func hasHandoffHook(entries []hookEntry) bool {
	for _, e := range entries {
		for _, h := range e.Hooks {
			if strings.Contains(h.Command, sessionStartMarkers[0]) || strings.Contains(h.Command, sessionStartMarkers[1]) {
				return true
			}
		}
	}
	return false
}

// checkMCPJSON verifies mcpServers.aimem in Claude Code's .mcp.json.
func checkMCPJSON(path string, repair bool) Finding {
	rel := relName(path)
	obj, mode, _, err := readObject(path)
	if err != nil {
		return Finding{File: rel, Level: "fail", Detail: "unreadable: " + err.Error(), Fix: "repair the file by hand; nothing was written"}
	}
	servers := object{}
	if raw, ok := obj["mcpServers"]; ok {
		if json.Unmarshal(raw, &servers) != nil || servers == nil {
			return Finding{File: rel, Level: "fail", Detail: "mcpServers is not a JSON object", Fix: "repair the file by hand; nothing was written"}
		}
	}
	want := map[string]any{"command": "aimem", "args": []string{"mcp"}}
	if raw, ok := servers["aimem"]; ok {
		if sameJSON(raw, want) {
			return Finding{File: rel, Level: "ok", Detail: "Claude Code MCP registration present"}
		}
		return Finding{File: rel, Level: "warn", Detail: "mcpServers.aimem differs from the installed shape; left as is", Fix: `expected {"command":"aimem","args":["mcp"]}`}
	}
	if !repair {
		return Finding{File: rel, Level: "warn", Detail: "Claude Code MCP registration missing", Fix: "run without --no-repair to add it"}
	}
	servers["aimem"], _ = json.Marshal(want)
	obj["mcpServers"], _ = json.Marshal(servers)
	if err := writeObject(path, obj, mode); err != nil {
		return Finding{File: rel, Level: "fail", Detail: "cannot write: " + err.Error()}
	}
	return Finding{File: rel, Level: "ok", Detail: "Claude Code MCP registration added", Repaired: true}
}

// checkOpenCode verifies opencode.json: the handoff in instructions and
// mcp.aimem. $schema is added with the registration when the file is new.
func checkOpenCode(path string, repair bool) []Finding {
	rel := relName(path)
	obj, mode, existed, err := readObject(path)
	if err != nil {
		return []Finding{{File: rel, Level: "fail", Detail: "unreadable: " + err.Error(), Fix: "repair the file by hand; nothing was written"}}
	}
	// Both members are validated before any finding is drafted, so a claim
	// of repair is made only for a write that then happens.
	var instructions []string
	if raw, ok := obj["instructions"]; ok {
		if json.Unmarshal(raw, &instructions) != nil {
			return []Finding{{File: rel, Level: "fail", Detail: "instructions is not an array of strings", Fix: "repair the file by hand; nothing was written"}}
		}
	}
	mcp := object{}
	if raw, ok := obj["mcp"]; ok {
		if json.Unmarshal(raw, &mcp) != nil || mcp == nil {
			return []Finding{{File: rel, Level: "fail", Detail: "mcp is not a JSON object", Fix: "repair the file by hand; nothing was written"}}
		}
	}
	var out []Finding
	changed := false
	hasHandoff := false
	for _, i := range instructions {
		if i == "docs/SESSION-STATE.md" {
			hasHandoff = true
		}
	}
	switch {
	case hasHandoff:
		out = append(out, Finding{File: rel, Level: "ok", Detail: "OpenCode handoff instruction present"})
	case !repair:
		out = append(out, Finding{File: rel, Level: "warn", Detail: "OpenCode handoff instruction missing", Fix: "run without --no-repair to add it"})
	default:
		instructions = append(instructions, "docs/SESSION-STATE.md")
		obj["instructions"], _ = json.Marshal(instructions)
		changed = true
		out = append(out, Finding{File: rel, Level: "ok", Detail: "OpenCode handoff instruction added", Repaired: true})
	}
	want := map[string]any{"type": "local", "command": []string{"aimem", "mcp"}, "enabled": true}
	switch raw, ok := mcp["aimem"]; {
	case ok && sameJSON(raw, want):
		out = append(out, Finding{File: rel, Level: "ok", Detail: "OpenCode MCP registration present"})
	case ok:
		out = append(out, Finding{File: rel, Level: "warn", Detail: "mcp.aimem differs from the installed shape; left as is", Fix: `expected {"type":"local","command":["aimem","mcp"],"enabled":true}`})
	case !repair:
		out = append(out, Finding{File: rel, Level: "warn", Detail: "OpenCode MCP registration missing", Fix: "run without --no-repair to add it"})
	default:
		mcp["aimem"], _ = json.Marshal(want)
		obj["mcp"], _ = json.Marshal(mcp)
		changed = true
		out = append(out, Finding{File: rel, Level: "ok", Detail: "OpenCode MCP registration added", Repaired: true})
	}
	if changed {
		if _, ok := obj["$schema"]; !ok && !existed {
			obj["$schema"], _ = json.Marshal("https://opencode.ai/config.json")
		}
		if err := writeObject(path, obj, mode); err != nil {
			// Nothing landed: no repair happened, whatever was drafted above.
			return []Finding{{File: rel, Level: "fail", Detail: "cannot write: " + err.Error(), Fix: "make the file writable and re-run; nothing was changed"}}
		}
	}
	return out
}

// sameJSON compares decoded JSON values structurally: a string "[mcp]" is
// not the array ["mcp"], and "true" is not true.
func sameJSON(raw json.RawMessage, want any) bool {
	var a, b any
	wb, _ := json.Marshal(want)
	return json.Unmarshal(raw, &a) == nil && json.Unmarshal(wb, &b) == nil && reflect.DeepEqual(a, b)
}

func relName(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	if len(parts) >= 2 && strings.HasPrefix(parts[len(parts)-2], ".") {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return parts[len(parts)-1]
}

// clientNames are the agent clients this checker knows, in report order.
var clientNames = []string{"claude", "codex", "opencode"}

var versionRE = regexp.MustCompile(`\d+\.\d+(\.\d+)?`)

func detectClients(versions bool) []Client {
	var out []Client
	for _, name := range clientNames {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		c := Client{Name: name, Path: p, Version: "unknown"}
		if versions {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			raw, err := exec.CommandContext(ctx, p, "--version").Output()
			cancel()
			if err == nil {
				if m := versionRE.FindString(string(raw)); m != "" {
					c.Version = m
				}
			}
		}
		out = append(out, c)
	}
	return out
}

// skillDirs lists, per client, where it reads skills from: project
// locations first, then user-level ones when home is known.
func skillDirs(dir, home, client string) []string {
	var dirs []string
	switch client {
	case "claude":
		dirs = append(dirs, filepath.Join(dir, ".claude", "skills"))
		if home != "" {
			dirs = append(dirs, filepath.Join(home, ".claude", "skills"))
		}
	case "codex":
		dirs = append(dirs, filepath.Join(dir, ".agents", "skills"))
		if home != "" {
			dirs = append(dirs, filepath.Join(home, ".agents", "skills"), filepath.Join(home, ".codex", "skills"))
		}
	case "opencode":
		dirs = append(dirs, filepath.Join(dir, ".claude", "skills"), filepath.Join(dir, ".opencode", "skills"), filepath.Join(dir, ".agents", "skills"))
		if home != "" {
			dirs = append(dirs, filepath.Join(home, ".config", "opencode", "skills"), filepath.Join(home, ".claude", "skills"))
		}
	}
	return dirs
}

var skillRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// Skills looks for each required skill (a directory holding SKILL.md) in
// every given client's locations; the caller decides the clients and the
// skills (the selected process names them after the hub is reached).
func Skills(dir, home string, clients []Client, required []string) []SkillStatus {
	return skillReport(dir, home, clients, required)
}

// skillReport looks for each required skill (a directory holding SKILL.md)
// in every detected client's locations.
func skillReport(dir, home string, clients []Client, required []string) []SkillStatus {
	var out []SkillStatus
	names := append([]string(nil), required...)
	sort.Strings(names)
	for _, skill := range names {
		for _, c := range clients {
			st := SkillStatus{Skill: skill, Client: c.Name}
			if skillRE.MatchString(skill) {
				for _, d := range skillDirs(dir, home, c.Name) {
					if fi, err := os.Stat(filepath.Join(d, skill, "SKILL.md")); err == nil && fi.Mode().IsRegular() {
						st.Path = filepath.Join(d, skill)
						break
					}
				}
			}
			if st.Path == "" {
				dirs := skillDirs(dir, home, c.Name)
				st.Fix = "install the skill directory (with SKILL.md) under " + strings.Join(dirs[:1], "") + " or another location " + c.Name + " reads"
			}
			out = append(out, st)
		}
	}
	return out
}
