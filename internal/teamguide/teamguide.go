// Package teamguide builds the team protocol guidance unit that the aimem
// binary serves through the team_context MCP tool and `aimem teams context`
// (docs/DESIGN-portable-team-context.md, decision 1): the playbook split
// into sections with stable ids, the request templates as sections of their
// own, the sections each role requires before work, and a digest over all
// of it. The unit is built from the files embedded by package docs and is
// either complete and valid or refused whole; nothing is truncated.
//
// Digest: SHA-256 over the manifest encoded as compact JSON (format, unit,
// team protocol, minimum hub, sections with their titles, sources, sizes
// and digests, the role sets and the reference map, in that order), then,
// for each section in manifest order, its id, a NUL, its length in decimal,
// a NUL and its bytes. The build version is not part of it: the same
// content has the same digest in every build.
package teamguide

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"aimem/docs"
)

const (
	// UnitID names the unit in every header and terminator.
	UnitID = "aimem-team-guidance"
	// Format is the version of this manifest shape.
	Format = 1
	// TeamProtocol is the session protocol_version the guidance describes;
	// MinHub is the first hub release that serves the whole loop it assumes.
	TeamProtocol = 1
	MinHub       = "0.7.0"

	MaxSectionBytes = 16 << 10
	MaxRoleBytes    = 32 << 10
	MaxUnitBytes    = 128 << 10

	playbookFile = "TEAM-PLAYBOOKS.md"
	examplesDir  = "examples/team"
	sourcePrefix = "docs/"
)

// Roles are the roles a required set exists for.
var Roles = []string{"coordinator", "worker"}

// playbookSections maps each heading of the playbook to its stable id, in
// document order; "" is the text before the first second-level heading. A
// heading that is added, renamed or removed without this table fails the
// build, so an id never silently changes meaning.
var playbookSections = []struct{ heading, id string }{
	{"", "common"},
	{"What the probe established about responsiveness", "responsiveness"},
	{"Coordinator playbook", "coordinator"},
	{"Worker playbook", "worker"},
	{"Questions, decisions and permissions", "questions"},
	{"Human escalation", "escalation"},
	{"Where the process rules live", "process-authority"},
}

// roleSections are the playbook sections each role needs before work. The
// templates those sections link to are added to the set by the build.
var roleSections = map[string][]string{
	"coordinator": {"common", "responsiveness", "coordinator", "questions", "escalation", "process-authority"},
	"worker":      {"common", "responsiveness", "worker", "questions", "escalation", "process-authority"},
}

// informative are the relative link targets the unit does not deliver, with
// what a reader should know instead. Any other relative target must resolve
// to a section.
var informative = map[string]string{
	"DESIGN-agent-teams.md":     "not delivered: the protocol design and its rationale, in the aimem repository",
	"TEAM-AGENT-QUICKSTART.md":  "not delivered: the mechanics; the MCP tool definitions carry the tool names and arguments",
	"AGENT-CAPABILITY-PROBE.md": "not delivered: the probe evidence behind the responsiveness rules",
	"examples/team/":            "the templates are delivered as the sections example/<name>",
}

// SectionInfo describes one section in the manifest.
type SectionInfo struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Source string `json:"source"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// RoleSet is the ordered list of sections a role requires.
type RoleSet struct {
	Role     string   `json:"role"`
	Required []string `json:"required"`
}

// Reference is one relative link found in a section: the section it
// resolves to, or why it is informative.
type Reference struct {
	From        string `json:"from"`
	Target      string `json:"target"`
	Section     string `json:"section,omitempty"`
	Informative string `json:"informative,omitempty"`
}

// Manifest is the digested description of the unit.
type Manifest struct {
	Format       int           `json:"format"`
	Unit         string        `json:"unit"`
	TeamProtocol int           `json:"team_protocol"`
	MinHub       string        `json:"min_hub"`
	Sections     []SectionInfo `json:"sections"`
	Roles        []RoleSet     `json:"roles"`
	References   []Reference   `json:"references"`
}

// Unit is a built, validated guidance unit.
type Unit struct {
	Manifest Manifest
	Digest   string // "sha256:<hex>"
	content  map[string][]byte
	index    map[string]int
}

var (
	// An inline link, with or without a quoted title, and a reference-style
	// link definition: every form a Markdown reader follows.
	linkRE    = regexp.MustCompile(`\[[^\]]*\]\(\s*<?([^)\s>]+)>?(?:\s+(?:"[^"]*"|'[^']*'))?\s*\)`)
	linkDefRE = regexp.MustCompile(`(?m)^ {0,3}\[[^\]]+\]:\s*<?([^\s>]+)>?`)
	idRE      = regexp.MustCompile(`^[a-z0-9][a-z0-9/_-]{0,63}$`)
	exampleRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}\.json$`)
)

var (
	embeddedOnce sync.Once
	embedded     *Unit
	embeddedErr  error
)

// Embedded returns the unit built from the files compiled into this
// binary. An invalid unit is an error on every call, never a partial unit;
// the package tests make that a failed build.
func Embedded() (*Unit, error) {
	embeddedOnce.Do(func() { embedded, embeddedErr = Build(docs.TeamGuidance) })
	return embedded, embeddedErr
}

// Build reads the playbook and the templates from fsys, splits and
// validates them, and computes the manifest and the digest.
func Build(fsys fs.FS) (*Unit, error) {
	raw, err := fs.ReadFile(fsys, playbookFile)
	if err != nil {
		return nil, fmt.Errorf("team guidance: %w", err)
	}
	u := &Unit{content: map[string][]byte{}, index: map[string]int{}}
	add := func(id, title, source string, b []byte) error {
		if _, dup := u.content[id]; dup {
			return fmt.Errorf("team guidance: section %s appears twice", id)
		}
		if len(b) > MaxSectionBytes {
			return fmt.Errorf("team guidance: section %s is %d bytes, over the %d-byte section limit", id, len(b), MaxSectionBytes)
		}
		sum := sha256.Sum256(b)
		u.index[id] = len(u.Manifest.Sections)
		u.Manifest.Sections = append(u.Manifest.Sections, SectionInfo{ID: id, Title: title, Source: source, Bytes: len(b), SHA256: hex.EncodeToString(sum[:])})
		u.content[id] = b
		return nil
	}
	parts, err := splitPlaybook(raw)
	if err != nil {
		return nil, err
	}
	for _, p := range parts {
		if err := add(p.id, p.title, sourcePrefix+playbookFile, p.body); err != nil {
			return nil, err
		}
	}
	entries, err := fs.ReadDir(fsys, examplesDir)
	if err != nil {
		return nil, fmt.Errorf("team guidance: %w", err)
	}
	for _, e := range entries { // ReadDir sorts by name
		if e.IsDir() || !exampleRE.MatchString(e.Name()) {
			return nil, fmt.Errorf("team guidance: unexpected template entry %q", e.Name())
		}
		b, err := fs.ReadFile(fsys, path.Join(examplesDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("team guidance: %w", err)
		}
		if !json.Valid(b) {
			return nil, fmt.Errorf("team guidance: template %s is not valid JSON", e.Name())
		}
		name := strings.TrimSuffix(e.Name(), ".json")
		if err := add("example/"+name, "Request template "+e.Name(), sourcePrefix+examplesDir+"/"+e.Name(), b); err != nil {
			return nil, err
		}
	}
	linked, err := u.resolveReferences(parts)
	if err != nil {
		return nil, err
	}
	for _, role := range Roles {
		set := append([]string{}, roleSections[role]...)
		var extra []string
		for _, id := range roleSections[role] {
			extra = append(extra, linked[id]...)
		}
		sort.Strings(extra)
		for _, id := range extra {
			if !slices.Contains(set, id) {
				set = append(set, id)
			}
		}
		u.Manifest.Roles = append(u.Manifest.Roles, RoleSet{Role: role, Required: set})
	}
	u.Manifest.Format, u.Manifest.Unit, u.Manifest.TeamProtocol, u.Manifest.MinHub = Format, UnitID, TeamProtocol, MinHub
	total := 0
	for _, s := range u.Manifest.Sections {
		total += s.Bytes
	}
	if total > MaxUnitBytes {
		return nil, fmt.Errorf("team guidance: unit is %d bytes, over the %d-byte unit limit", total, MaxUnitBytes)
	}
	if u.Digest, err = u.digest(); err != nil {
		return nil, err
	}
	for _, role := range Roles {
		// The rendered set is what a client receives; its size is the limit
		// that matters, headers and terminator included.
		text, err := u.Role(role, sizingVersion)
		if err != nil {
			return nil, err
		}
		if len(text) > MaxRoleBytes {
			return nil, fmt.Errorf("team guidance: role %s required set renders to %d bytes, over the %d-byte role limit", role, len(text), MaxRoleBytes)
		}
	}
	return u, nil
}

type part struct {
	id, title string
	body      []byte
}

// splitPlaybook cuts the playbook at its second-level headings and maps
// each piece to its stable id.
func splitPlaybook(raw []byte) ([]part, error) {
	if !bytes.HasPrefix(raw, []byte("# ")) {
		return nil, errors.New("team guidance: the playbook must start with its title heading")
	}
	var parts []part
	lines := strings.SplitAfter(string(raw), "\n")
	start, heading := 0, ""
	title := strings.TrimSpace(strings.TrimPrefix(lines[0], "# "))
	flush := func(end int) {
		parts = append(parts, part{title: heading, body: []byte(strings.Join(lines[start:end], ""))})
	}
	for i, l := range lines {
		if strings.HasPrefix(l, "## ") {
			flush(i)
			start, heading = i, strings.TrimSpace(strings.TrimPrefix(l, "## "))
		}
	}
	flush(len(lines))
	if len(parts) != len(playbookSections) {
		return nil, fmt.Errorf("team guidance: the playbook has %d sections, the id table %d; give every second-level heading a stable id", len(parts), len(playbookSections))
	}
	for i := range parts {
		if parts[i].title != playbookSections[i].heading {
			return nil, fmt.Errorf("team guidance: playbook section %d is %q, the id table expects %q; give every second-level heading a stable id", i, parts[i].title, playbookSections[i].heading)
		}
		parts[i].id = playbookSections[i].id
		if parts[i].title == "" {
			parts[i].title = title
		}
	}
	return parts, nil
}

// resolveReferences records every relative link of the playbook sections
// and returns, per section, the template sections it links to. A relative
// link that neither resolves to a section nor is listed as informative
// fails the build.
func (u *Unit) resolveReferences(parts []part) (map[string][]string, error) {
	anchors := map[string]string{}
	for _, p := range parts {
		anchors[slug(p.title)] = p.id
	}
	linked := map[string][]string{}
	u.Manifest.References = []Reference{}
	for _, p := range parts {
		for _, m := range append(linkRE.FindAllStringSubmatch(string(p.body), -1), linkDefRE.FindAllStringSubmatch(string(p.body), -1)...) {
			target := m[1]
			if strings.HasPrefix(target, "https://") || strings.HasPrefix(target, "http://") {
				continue // absolute: not part of the unit, and not a repository file
			}
			ref := Reference{From: p.id, Target: target}
			file, anchor, _ := strings.Cut(target, "#")
			switch {
			case file == "" && anchors[anchor] != "":
				ref.Section = anchors[anchor]
			case strings.HasPrefix(file, examplesDir+"/") && strings.HasSuffix(file, ".json") && anchor == "":
				id := "example/" + strings.TrimSuffix(strings.TrimPrefix(file, examplesDir+"/"), ".json")
				if _, ok := u.content[id]; !ok {
					return nil, fmt.Errorf("team guidance: section %s links %s, which is not in the unit", p.id, target)
				}
				ref.Section = id
				if !slices.Contains(linked[p.id], id) {
					linked[p.id] = append(linked[p.id], id)
				}
			case informative[file] != "":
				ref.Informative = informative[file]
			default:
				return nil, fmt.Errorf("team guidance: section %s links %s, which neither resolves to a section nor is listed as informative", p.id, target)
			}
			u.Manifest.References = append(u.Manifest.References, ref)
		}
	}
	return linked, nil
}

// slug is the GitHub-style anchor of a heading.
func slug(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	return b.String()
}

func (u *Unit) digest() (string, error) {
	mj, err := json.Marshal(u.Manifest)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(mj)
	for _, s := range u.Manifest.Sections {
		fmt.Fprintf(h, "%s\x00%d\x00", s.ID, s.Bytes)
		h.Write(u.content[s.ID])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// IDs returns every section id in manifest order.
func (u *Unit) IDs() []string {
	ids := make([]string, len(u.Manifest.Sections))
	for i, s := range u.Manifest.Sections {
		ids[i] = s.ID
	}
	return ids
}

// Required returns the sections a role requires, or an error naming the
// roles that exist.
func (u *Unit) Required(role string) ([]string, error) {
	for _, r := range u.Manifest.Roles {
		if r.Role == role {
			return r.Required, nil
		}
	}
	return nil, fmt.Errorf("unknown role %q: use %s", clipID(role), strings.Join(Roles, " or "))
}

// Section returns one section's bytes by id.
func (u *Unit) Section(id string) ([]byte, error) {
	if !idRE.MatchString(id) {
		return nil, fmt.Errorf("section must be a section id (lowercase letters, digits, '-', '_' and '/'); ids: %s", strings.Join(u.IDs(), ", "))
	}
	b, ok := u.content[id]
	if !ok {
		return nil, fmt.Errorf("unknown section %q; ids: %s", id, strings.Join(u.IDs(), ", "))
	}
	return b, nil
}

func clipID(s string) string {
	if len(s) > 64 {
		return s[:64] + "..."
	}
	return s
}
