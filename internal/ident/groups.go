package ident

// Knowledge groups: named shared memory scopes between consenting projects.
// A project opts into groups via .aimem.json in its root:
//
//	{"groups": ["webstack", "platform"]}
//
// Each group is backed by its own reserved database (project id
// "group-<name>"), so sharing is explicit and physically scoped: projects
// that declare no groups share nothing. Journal events never cross
// projects — groups apply to curated memories only.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var groupNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// GroupProject converts a group name into its reserved backing project id.
func GroupProject(name string) (string, error) {
	if !groupNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid group name %q (want lowercase letters, digits, dashes)", name)
	}
	return "group-" + name, nil
}

type projectConfig struct {
	Project string   `json:"project"` // explicit stable project id (optional)
	Groups  []string `json:"groups"`
	Hub     string   `json:"hub"`  // named hub this project's data belongs to (optional)
	Docs    []string `json:"docs"` // extra shared-document files (docs/SESSION-STATE.md is bound by default)
	// SessionFacts opts into session-start knowledge injection: a token
	// budget of recalled facts appended to the handoff (0/absent = off).
	SessionFacts int `json:"session_facts"`
}

// SessionFactsBudget reads the opt-in session-start recall budget;
// 0 means the feature is off. Fail-open like all config reads.
func SessionFactsBudget(dir string) int {
	cfg, err := readConfig(dir)
	if err != nil || cfg.SessionFacts < 0 {
		return 0
	}
	return cfg.SessionFacts
}

func readConfig(dir string) (*projectConfig, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ".aimem.json"))
	if os.IsNotExist(err) {
		return &projectConfig{}, nil
	}
	if err != nil {
		return nil, err
	}
	// Windows editors and PowerShell's `-Encoding UTF8` prepend a UTF-8
	// BOM, and encoding/json rejects one. An unparseable file is treated
	// as "no config" (a malformed file must not block checkpoints,
	// CHANGELOG v0.1.77), so a BOM would silently void the project's hub
	// binding and groups and quietly send its data to the machine's
	// default hub.
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	var cfg projectConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		// Fail-open, but never silently: losing the pin, hub binding and
		// groups is exactly what the warning lets someone catch before
		// data lands under a derived id on the default hub.
		warnBadConfig.Do(func() {
			fmt.Fprintf(os.Stderr,
				"aimem: %s unreadable (%v) — treated as absent; project pin, hub binding and groups are inactive until the file is fixed\n",
				filepath.Join(dir, ".aimem.json"), err)
		})
		return &projectConfig{}, nil
	}
	return &cfg, nil
}

// warnBadConfig keeps the malformed-config warning to one line per
// process — the submit path reads the config several times per event.
var warnBadConfig sync.Once

// projectPin returns the explicit project id from .aimem.json, "" if none.
// A malformed name in a PARSEABLE config is an error: silently falling
// back to a derived id would split the journal across machines. (An
// unparseable file is a different case — readConfig treats it as absent
// with a warning, so checkpoints never block on a broken config.)
func projectPin(dir string) (string, error) {
	cfg, err := readConfig(dir)
	if err != nil {
		return "", err
	}
	if cfg.Project == "" {
		return "", nil
	}
	if !pinnedName.MatchString(cfg.Project) || cfg.Project == "user" ||
		strings.HasPrefix(cfg.Project, "group-") {
		return "", fmt.Errorf(".aimem.json: invalid project name %q (want letters/digits/._- , not \"user\" or \"group-*\")", cfg.Project)
	}
	return cfg.Project, nil
}

// ProjectHubName reads the project's optional hub binding: the name of a
// hub entry in the machine's hub.json that this project's data belongs
// to. "" means the default hub. A malformed name is an error — silently
// falling back to the default hub would leak data across hubs.
func ProjectHubName(dir string) (string, error) {
	cfg, err := readConfig(dir)
	if err != nil {
		return "", err
	}
	if cfg.Hub != "" && !groupNameRe.MatchString(cfg.Hub) {
		return "", fmt.Errorf(".aimem.json: invalid hub name %q (want lowercase letters, digits, dashes)", cfg.Hub)
	}
	return cfg.Hub, nil
}

// ProjectGroups reads the group memberships a project has declared. A
// missing config file means no groups; a malformed one is an error (silent
// misconfiguration would silently stop sharing).
func ProjectGroups(dir string) ([]string, error) {
	cfg, err := readConfig(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, g := range cfg.Groups {
		id, err := GroupProject(g)
		if err != nil {
			return nil, fmt.Errorf(".aimem.json: %w", err)
		}
		out = append(out, id)
	}
	return out, nil
}

// DefaultDocPath is bound as a shared document whenever the file exists:
// it is the file the whole handoff protocol already names, so it should
// not need declaring.
const DefaultDocPath = "docs/SESSION-STATE.md"

// ProjectDocs returns the project-relative paths of the files bound as
// shared documents: the default handoff when present, plus any declared in
// .aimem.json. Paths must stay inside the project — a binding is a name
// on the hub, and "../../etc/passwd" must never become one. Malformed
// config yields only the default binding (fail-open, like checkpoints).
func ProjectDocs(dir string) []string {
	cfg, err := readConfig(dir)
	if err != nil {
		cfg = &projectConfig{}
	}
	// An explicitly EMPTY list ("docs": []) opts out of all bindings,
	// default included — for checkouts that carry a handoff file which
	// must not become the shared one (an archived clone, a fork). An
	// absent field keeps the default.
	if cfg.Docs != nil && len(cfg.Docs) == 0 {
		return nil
	}
	var out []string
	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(DefaultDocPath))); err == nil {
		out = append(out, DefaultDocPath)
	}
	for _, p := range cfg.Docs {
		p = filepath.ToSlash(filepath.Clean(p))
		// Windows nuance: IsAbs("/x") is false (rooted, not absolute),
		// so reject leading slashes and volume names explicitly too.
		if p == "" || p == "." || strings.HasPrefix(p, "../") || strings.HasPrefix(p, "/") ||
			filepath.IsAbs(p) || filepath.VolumeName(p) != "" || p == DefaultDocPath {
			continue
		}
		out = append(out, p)
	}
	return out
}

// DocName derives the hub-side document name from a bound path: the base
// name without extension ("docs/SESSION-STATE.md" -> "SESSION-STATE").
func DocName(path string) string {
	base := filepath.Base(filepath.FromSlash(path))
	return strings.TrimSuffix(base, filepath.Ext(base))
}
