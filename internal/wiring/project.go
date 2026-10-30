package wiring

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"aimem/internal/confine"
	"aimem/internal/ident"
	"aimem/templates"
)

//go:embed assets/aimem_analyze.md
var analyzeCommand string

// analyzeMarker is the start of the managed-file line in every version of
// the /aimem-analyze command (install.sh wrote the first ones).
const analyzeMarker = "<!-- managed by aimem"

const claudeStub = "See @AGENTS.md for the shared session handoff protocol.\n"

// ProjectOptions control one Wire: the .aimem.json keys to set and whether
// anything is written.
type ProjectOptions struct {
	Project   string   // pin this project id; "" leaves the key as it is
	Hub       string   // bind this named hub; "" leaves the key as it is
	Groups    []string // set the groups; nil leaves them as they are
	Check     bool     // report what is wired and what is missing; write nothing
	Home      string   // the user's home: never wired, and where the Codex prompt goes
	StateRoot string   // aimem's state root: never wired, nor anything inside it
}

// Wire wires the directory dir as a project, the project part of the
// installers: the handoff template, the SessionStart hooks, the MCP
// registrations, the OpenCode instructions, the /join_team and
// /aimem-analyze commands, AGENTS.md and CLAUDE.md, and .aimem.json. A file
// with the user's content is kept, a managed file is refreshed, and in
// .aimem.json only the keys o names are set. A linked worktree of a wired
// checkout gets everything but .aimem.json: the main checkout's governs it.
// A refused directory, or an option that would be refused later, returns
// an error before anything is written.
func Wire(dir string, o ProjectOptions) ([]Finding, error) {
	dir, err := canonical(dir)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	gov, linked, err := refuseDir(dir, o)
	if err != nil {
		return nil, err
	}
	if o.Project != "" {
		if err := ident.CheckProjectName(o.Project); err != nil {
			return nil, err
		}
	}
	if o.Hub != "" {
		if err := ident.CheckHubName(o.Hub); err != nil {
			return nil, err
		}
	}
	for _, g := range o.Groups {
		if _, err := ident.GroupProject(g); err != nil {
			return nil, err
		}
	}
	write := !o.Check
	r := Check(dir, Options{Repair: write, AllowProjectStopHooks: true, Home: o.Home, Commands: true})
	out := r.Findings
	for _, rel := range []string{filepath.Join(".claude", "commands", "aimem-analyze.md"), filepath.Join(".opencode", "command", "aimem-analyze.md")} {
		out = append(out, managedFile(dir, rel, analyzeCommand, write))
	}
	out = append(out, keptFile(dir, "AGENTS.md", string(templates.AgentsMD), write))
	out = append(out, keptFile(dir, "CLAUDE.md", claudeStub, write))
	if linked {
		out = append(out, governingConfig(gov))
	} else {
		out = append(out, projectConfig(dir, o, write))
	}
	if o.Check {
		for i, f := range out {
			if f.Level == "warn" && f.Fix != "" && (strings.Contains(f.Fix, "--no-repair") || strings.Contains(f.Fix, "teams commands")) {
				out[i].Fix = "run `aimem wire` here to write it"
			}
		}
	}
	return out, nil
}

func canonical(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if c, err := filepath.EvalSymlinks(abs); err == nil {
		abs = c
	}
	return abs, nil
}

func sameDir(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// within reports whether dir is root or below it.
func within(dir, root string) bool {
	rel, err := filepath.Rel(root, dir) // fails across Windows volumes
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// refuseDir refuses a directory that is not to be wired. A linked worktree
// of a wired checkout is wired without its own .aimem.json: linked is true
// and gov names the main checkout whose file governs it.
func refuseDir(dir string, o ProjectOptions) (gov string, linked bool, err error) {
	if o.Home != "" {
		if home, err := canonical(o.Home); err == nil && sameDir(dir, home) {
			return "", false, fmt.Errorf("refusing to wire the home directory %s as a project: Claude Code would read its CLAUDE.md in every session under it. Run this inside a project directory.", dir)
		}
	}
	if o.StateRoot != "" {
		if root, err := canonical(o.StateRoot); err == nil && within(dir, root) {
			return "", false, fmt.Errorf("refusing to wire %s as a project: it is inside aimem's state root %s. Run `aimem wire` inside a project directory.", dir, root)
		}
	}
	gov, viaWorktree := ident.ConfigDir(dir)
	if viaWorktree {
		if top := gitTop(dir); !sameDir(top, dir) {
			return "", false, fmt.Errorf("refusing to wire %s as a project: it is inside the linked worktree %s of the wired checkout %s. Next: run `aimem wire` in %s to give that worktree its hooks and registrations.", dir, top, gov, top)
		}
		if o.Project != "" || o.Hub != "" || o.Groups != nil {
			return "", false, fmt.Errorf("refusing to set the project, hub or groups in %s: it is a linked worktree of the wired checkout %s, whose .aimem.json governs it. Next: run `aimem wire` with those options in %s; run it here without them to write this worktree's hooks and registrations.", dir, gov, gov)
		}
		return gov, true, nil
	}
	if !sameDir(gov, dir) {
		return "", false, fmt.Errorf("refusing to wire %s as a project: it is inside the wired checkout %s, whose .aimem.json already governs it, so sessions here already belong to that project. Next: start sessions from %s; to refresh its wiring, run `aimem wire` there; to make this directory a project of its own, move it out of that checkout and run `aimem wire` in it.", dir, gov, gov)
	}
	return gov, false, nil
}

// gitTop is the nearest directory at or above dir that holds a .git entry,
// or dir when there is none.
func gitTop(dir string) string {
	for d := dir; ; {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		parent := filepath.Dir(d)
		if parent == d {
			return dir
		}
		d = parent
	}
}

// governingConfig reports the main checkout's .aimem.json, which governs a
// linked worktree; the worktree gets none of its own.
func governingConfig(gov string) Finding {
	const rel = ".aimem.json"
	path := filepath.Join(gov, rel)
	obj, _, _, err := readObject(gov, path)
	if err != nil {
		return Finding{File: rel, Level: "fail", Detail: "the governing " + path + " is unreadable: " + err.Error(), Fix: "repair it in the main checkout"}
	}
	return Finding{File: rel, Level: "ok", Detail: "not written here: this linked worktree is governed by " + path + describeConfig(obj)}
}

// managedFile writes content at rel unless the file there is current, or
// is not one aimem manages (then it is left alone and reported).
func managedFile(dir, rel, content string, write bool) Finding {
	name := filepath.ToSlash(rel)
	path := filepath.Join(dir, rel)
	if err := confine.Check(dir, path); err != nil {
		return Finding{File: name, Level: "fail", Detail: err.Error()}
	}
	current, err := os.ReadFile(path)
	switch {
	case err == nil && string(current) == content:
		return Finding{File: name, Level: "ok", Detail: "command present and current"}
	case err == nil && !strings.Contains(string(current), analyzeMarker):
		return Finding{File: name, Level: "warn", Detail: "a file of that name exists and is not managed by aimem; left as is"}
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return Finding{File: name, Level: "fail", Detail: "unreadable: " + err.Error()}
	}
	state := "missing"
	if err == nil {
		state = "outdated"
	}
	if !write {
		return Finding{File: name, Level: "warn", Detail: "command " + state, Fix: "run `aimem wire` here to write it"}
	}
	if err := writeAtomic(dir, path, []byte(content), 0o644); err != nil {
		return Finding{File: name, Level: "fail", Detail: "cannot write: " + err.Error()}
	}
	return Finding{File: name, Level: "ok", Detail: "command written (" + state + ")", Repaired: true}
}

// keptFile creates rel with content when it is missing; an existing file
// is the user's and is never rewritten.
func keptFile(dir, rel, content string, write bool) Finding {
	path := filepath.Join(dir, rel)
	if _, err := os.Lstat(path); err == nil {
		return Finding{File: rel, Level: "ok", Detail: "present (kept as is)"}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Finding{File: rel, Level: "fail", Detail: "unreadable: " + err.Error()}
	}
	if !write {
		return Finding{File: rel, Level: "warn", Detail: "missing", Fix: "run `aimem wire` here to create it"}
	}
	if err := writeAtomic(dir, path, []byte(content), 0o644); err != nil {
		return Finding{File: rel, Level: "fail", Detail: "cannot write: " + err.Error()}
	}
	return Finding{File: rel, Level: "ok", Detail: "created", Repaired: true}
}

// projectConfig creates .aimem.json, or sets in it only the keys o names;
// every other key keeps its value. It is written as UTF-8 without a BOM.
func projectConfig(dir string, o ProjectOptions, write bool) Finding {
	const rel = ".aimem.json"
	path := filepath.Join(dir, rel)
	// readObject takes an empty client config for an empty object, but a
	// session's reader (ident.loadConfig) refuses an empty .aimem.json, so
	// the project would run with no pin, hub binding or groups.
	if raw, err := confine.ReadFile(dir, path); err == nil && len(bytes.TrimSpace(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))) == 0 {
		return Finding{File: rel, Level: "fail", Detail: "empty: a session cannot read it, so the project has no pin, hub binding or groups",
			Fix: "write `{}` into it, or remove it, and run `aimem wire` again; nothing was written"}
	}
	obj, mode, existed, err := readObject(dir, path)
	if err != nil {
		return Finding{File: rel, Level: "fail", Detail: "unreadable: " + err.Error(), Fix: "repair the file by hand; nothing was written"}
	}
	want := map[string]any{}
	if o.Project != "" {
		want["project"] = o.Project
	}
	if o.Hub != "" {
		want["hub"] = o.Hub
	}
	if o.Groups != nil {
		want["groups"] = o.Groups
	} else if !existed {
		want["groups"] = []string{}
	}
	var changed []string
	for k, v := range want {
		if raw, ok := obj[k]; !ok || !sameJSON(raw, v) {
			changed = append(changed, k)
		}
	}
	if len(changed) == 0 {
		return Finding{File: rel, Level: "ok", Detail: "present" + describeConfig(obj)}
	}
	if !write {
		state := "missing"
		if existed {
			state = "differs in " + strings.Join(sortedKeys(changed), ", ")
		}
		return Finding{File: rel, Level: "warn", Detail: state, Fix: "run `aimem wire` here to write it"}
	}
	for _, k := range changed {
		obj[k] = rawPlain(want[k])
	}
	if err := writeObject(dir, path, obj, mode); err != nil {
		return Finding{File: rel, Level: "fail", Detail: "cannot write: " + err.Error()}
	}
	detail := "created"
	if existed {
		detail = "set " + strings.Join(sortedKeys(changed), ", ")
	}
	return Finding{File: rel, Level: "ok", Detail: detail + describeConfig(obj), Repaired: true}
}

func sortedKeys(keys []string) []string {
	out := append([]string(nil), keys...)
	sort.Strings(out)
	return out
}

// describeConfig names the project, hub and groups a config declares.
func describeConfig(obj object) string {
	var cfg struct {
		Project string   `json:"project"`
		Hub     string   `json:"hub"`
		Groups  []string `json:"groups"`
	}
	raw, _ := json.Marshal(obj)
	json.Unmarshal(raw, &cfg)
	project, hub, groups := cfg.Project, cfg.Hub, strings.Join(cfg.Groups, ",")
	if project == "" {
		project = "derived"
	}
	if hub == "" {
		hub = "default"
	}
	if groups == "" {
		groups = "none"
	}
	return fmt.Sprintf(" (project: %s, hub: %s, groups: %s)", project, hub, groups)
}
