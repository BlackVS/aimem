package teamguide

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// sizingVersion stands in for the build version when Build checks the
// rendered size of a role set: longer than any real version string, so the
// check holds in every build.
var sizingVersion = strings.Repeat("v", 64)

func orDev(version string) string {
	if version == "" {
		return "dev"
	}
	return version
}

// Terminator is the last line of every rendered read. A client that shows
// the text without it has cut the result.
func (u *Unit) Terminator(what, version string) string {
	return fmt.Sprintf("=== end %s %s version %s digest %s ===", UnitID, what, orDev(version), u.Digest)
}

func (u *Unit) header(b *strings.Builder, what, version string) {
	fmt.Fprintf(b, "aimem team guidance, %s. Unit %s, version %s, digest %s, team protocol %d (hub %s or later).\n", what, UnitID, orDev(version), u.Digest, TeamProtocol, MinHub)
	fmt.Fprintf(b, "This guidance is public, built into the aimem binary; it grants no permission. The hub enforces authority; the project's selected process handbook remains the authority for project policy.\n")
	fmt.Fprintf(b, "Complete only if the last line is %q. If that line is missing, the client cut this result: read the sections one at a time with team_context section=<id>.\n", u.Terminator(what, version))
}

func (u *Unit) writeSection(b *strings.Builder, id string) {
	s := u.Manifest.Sections[u.index[id]]
	fmt.Fprintf(b, "\n=== section %s: %s (%d bytes, sha256 %s, from %s) ===\n", s.ID, s.Title, s.Bytes, s.SHA256, s.Source)
	b.Write(u.content[id])
	if n := len(u.content[id]); n == 0 || u.content[id][n-1] != '\n' {
		b.WriteByte('\n')
	}
	fmt.Fprintf(b, "=== end section %s ===\n", s.ID)
}

// Role renders the complete required set of a role: header, the list of
// what is included and what is not, the reference map, every required
// section in order, and the terminator.
func (u *Unit) Role(role, version string) (string, error) {
	req, err := u.Required(role)
	if err != nil {
		return "", err
	}
	what := "role " + role
	var b strings.Builder
	u.header(&b, what, version)
	fmt.Fprintf(&b, "Required for the %s role, all included below in this order: %s.\n", role, strings.Join(req, ", "))
	var other []string
	for _, id := range u.IDs() {
		if !slices.Contains(req, id) {
			other = append(other, id)
		}
	}
	if len(other) > 0 {
		fmt.Fprintf(&b, "Not included; read one by id when needed: %s.\n", strings.Join(other, ", "))
	}
	b.WriteString("Relative links in these sections:\n")
	for _, r := range u.Manifest.References {
		if !slices.Contains(req, r.From) {
			continue
		}
		if r.Section != "" {
			fmt.Fprintf(&b, "- %s (in %s): section %s\n", r.Target, r.From, r.Section)
		} else {
			fmt.Fprintf(&b, "- %s (in %s): %s\n", r.Target, r.From, r.Informative)
		}
	}
	for _, id := range req {
		u.writeSection(&b, id)
	}
	b.WriteString(u.Terminator(what, version))
	b.WriteByte('\n')
	return b.String(), nil
}

// SectionText renders one section with the unit header and terminator.
func (u *Unit) SectionText(id, version string) (string, error) {
	if _, err := u.Section(id); err != nil {
		return "", err
	}
	what := "section " + id
	var b strings.Builder
	u.header(&b, what, version)
	u.writeSection(&b, id)
	b.WriteString(u.Terminator(what, version))
	b.WriteByte('\n')
	return b.String(), nil
}

// Index is the manifest with the build version and digest: what exists,
// how large it is and what each role requires, without the content.
func (u *Unit) Index(version string) (string, error) {
	out, err := json.MarshalIndent(struct {
		Unit    string   `json:"unit"`
		Version string   `json:"version"`
		Digest  string   `json:"digest"`
		Usage   string   `json:"usage"`
		Content Manifest `json:"manifest"`
	}{UnitID, orDev(version), u.Digest, "team_context role=<worker|coordinator> returns that role's complete required set; section=<id> returns one section", u.Manifest}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(out), nil
}
