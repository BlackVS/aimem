// Package skills is the hub-served skills set (docs/DESIGN-portable-team-context.md,
// increment 1e): a skills release, or an instance's own skills, imported
// into the hub by an admin on the hub host from an archive verified by its
// SHA256SUMS, stored hub-wide under the state root with its digest, and
// served to members under their project grant. A member installs the
// skills its project's process requires from the hub into the directories
// the agent clients read, and reads nothing from the internet. Two
// sources: the public release and the instance set; an instance skill of
// the same name overrides the public one.
package skills

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"aimem/internal/process"
)

// The two sources of a skills set, in precedence order from lowest to
// highest: an instance entry overrides a release entry of the same name.
const (
	SourceRelease  = "release"
	SourceInstance = "instance"
)

// Sources lists the sources in precedence order, lowest first.
var Sources = []string{SourceRelease, SourceInstance}

// ValidSource reports whether s names a source.
func ValidSource(s string) bool { return s == SourceRelease || s == SourceInstance }

// Bounds of one set: each file, all files together, and their number. A
// skills release is a few hundred KiB of Markdown; anything near these
// bounds is a mistake, not a skill.
const (
	MaxFileBytes = process.MaxFileBytes
	MaxSetBytes  = process.MaxSetBytes
	MaxSetFiles  = 1024
)

var (
	nameRE    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	versionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)
	digestRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// ValidName reports whether s is a skill name: a plain lowercase word.
func ValidName(s string) bool { return nameRE.MatchString(s) }

// ValidVersion reports whether s is a set version: a release's VERSION
// (1.33.13) or any plain word an instance chooses.
func ValidVersion(s string) bool { return versionRE.MatchString(s) }

// Set is one imported skills set: its files keyed "<skill>/<path>" (each
// skill a directory holding SKILL.md), the version the import named, and
// the source it was imported as.
type Set struct {
	Source  string            `json:"source"`
	Version string            `json:"version"`
	Files   map[string][]byte `json:"files"`
}

// Digest is the SHA-256 over the set's files, the same canonical encoding
// as a process set's (process.Digest).
func (s *Set) Digest() string { return process.Digest(s.Files) }

// Bytes is the size of the set's files together.
func (s *Set) Bytes() int {
	n := 0
	for _, f := range s.Files {
		n += len(f)
	}
	return n
}

// Names lists the set's skills, sorted.
func (s *Set) Names() []string { return names(s.Files) }

func names(files map[string][]byte) []string {
	seen := map[string]bool{}
	for p := range files {
		seen[strings.SplitN(p, "/", 2)[0]] = true
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Validate checks that the set is exactly a skills set: a source, a
// version, at least one skill, every file under a skill directory whose
// name is plain and which holds SKILL.md, clean relative paths, and the
// bounds. A set the hub accepts is one every member can install.
func (s *Set) Validate() error {
	if !ValidSource(s.Source) {
		return fmt.Errorf("source %q is not release or instance", s.Source)
	}
	if !ValidVersion(s.Version) {
		return errors.New("version must be one plain word of at most 64 characters (a release's VERSION, or the instance's own)")
	}
	if len(s.Files) == 0 {
		return errors.New("the set holds no files")
	}
	if len(s.Files) > MaxSetFiles {
		return fmt.Errorf("the set holds %d files, more than %d", len(s.Files), MaxSetFiles)
	}
	total := 0
	hasSkillMD := map[string]bool{}
	for p, data := range s.Files {
		if err := checkPath(p); err != nil {
			return fmt.Errorf("file %q: %w", p, err)
		}
		name, rest, ok := strings.Cut(p, "/")
		if !ok || !ValidName(name) {
			return fmt.Errorf("file %q is not inside a skill directory with a plain name", p)
		}
		if rest == "SKILL.md" {
			hasSkillMD[name] = true
		}
		if len(data) > MaxFileBytes {
			return fmt.Errorf("%s is larger than %d bytes", p, MaxFileBytes)
		}
		total += len(data)
	}
	if total > MaxSetBytes {
		return fmt.Errorf("the set is %d bytes, larger than %d", total, MaxSetBytes)
	}
	for _, n := range s.Names() {
		if !hasSkillMD[n] {
			return fmt.Errorf("skill %s has no SKILL.md", n)
		}
	}
	return nil
}

// checkPath accepts a clean relative slash path of bounded length.
func checkPath(p string) error {
	if p == "" || len(p) > 256 || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.ContainsAny(p, "\t\r\n") {
		return errors.New("must be a relative slash path (at most 256 bytes)")
	}
	if path.Clean(p) != p || p == "." || strings.HasPrefix(p, "../") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/") {
		return errors.New("must be a clean relative path")
	}
	return nil
}

// Skill is one skill of the effective set: where it comes from and which
// set version it is pinned to.
type Skill struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Source  string `json:"source"`
	Digest  string `json:"digest"` // the digest of the set it is served from
}

// Entry describes one imported set as the index records it.
type Entry struct {
	Version    string   `json:"version"`
	Digest     string   `json:"digest"`
	Skills     []string `json:"skills"`
	Bytes      int      `json:"bytes"`
	ImportedAt string   `json:"imported_at"`
	ImportedBy string   `json:"imported_by,omitempty"`
}

// Effective merges the current entries by precedence: a skill of the
// instance set replaces the release's of the same name. Sorted by name.
func Effective(current map[string]*Entry) []Skill {
	byName := map[string]Skill{}
	for _, src := range Sources { // lowest precedence first, so the last write wins
		e := current[src]
		if e == nil {
			continue
		}
		for _, n := range e.Skills {
			byName[n] = Skill{Name: n, Version: e.Version, Source: src, Digest: e.Digest}
		}
	}
	out := make([]Skill, 0, len(byName))
	for _, s := range byName {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// The archive form: a gzip-compressed tar holding skills/<name>/... and,
// at its root, VERSION (the aiskills release archive is exactly this, under
// one leading directory, beside its SHA256SUMS; `aimem skills export`
// writes the same shape for an instance's set).

// ReadArchive reads the skills of a tar.gz: the entries under skills/
// (below one leading directory when every entry shares it), each skill a
// directory, and VERSION at the root when the archive has one. version
// overrides the archive's; without either the read fails.
func ReadArchive(r io.Reader, source, version string) (*Set, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("archive: not gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	all := map[string][]byte{}
	total := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.TrimPrefix(path.Clean(strings.TrimPrefix(hdr.Name, "./")), "/")
		if name == "." || strings.HasPrefix(name, "../") || strings.Contains(name, "/../") {
			return nil, fmt.Errorf("archive: entry %q leaves the archive", hdr.Name)
		}
		if hdr.Size > MaxFileBytes {
			if _, err := io.CopyN(io.Discard, tr, hdr.Size); err != nil {
				return nil, fmt.Errorf("archive: %w", err)
			}
			all[name] = nil // not read, to bound memory; refused below if it is a skill's file
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, MaxFileBytes+1))
		if err != nil {
			return nil, fmt.Errorf("archive: %w", err)
		}
		total += len(data)
		if total > 16*MaxSetBytes {
			return nil, fmt.Errorf("archive: more than %d bytes of files", 16*MaxSetBytes)
		}
		all[name] = data
	}
	all = stripTopDir(all)
	set := &Set{Source: source, Version: version, Files: map[string][]byte{}}
	for p, data := range all {
		if rest, ok := strings.CutPrefix(p, "skills/"); ok {
			if data == nil {
				return nil, fmt.Errorf("archive: %s is larger than %d bytes; a skill is text, the set is refused whole", p, MaxFileBytes)
			}
			set.Files[rest] = data
		}
	}
	if v, ok := all["VERSION"]; ok && set.Version == "" {
		set.Version = strings.TrimSpace(string(v))
	}
	if set.Version == "" {
		return nil, errors.New("archive: no VERSION file at its root; name the version with --version")
	}
	if len(set.Files) == 0 {
		return nil, errors.New("archive: no skills/<name>/ directories")
	}
	if err := set.Validate(); err != nil {
		return nil, err
	}
	return set, nil
}

// stripTopDir removes the one leading directory every entry shares, as a
// forge's tarball and a release archive have.
func stripTopDir(all map[string][]byte) map[string][]byte {
	top := ""
	for p := range all {
		d, _, ok := strings.Cut(p, "/")
		if !ok || (top != "" && d != top) {
			return all
		}
		top = d
	}
	if top == "" || top == "skills" {
		return all
	}
	out := make(map[string][]byte, len(all))
	for p, data := range all {
		out[strings.TrimPrefix(p, top+"/")] = data
	}
	return out
}

// ReadArchiveFile reads and verifies an archive on disk: the SHA256SUMS
// file is checked first (no line for the archive, or another digest, is a
// refusal), then the archive is read.
func ReadArchiveFile(archivePath, sumsPath, source, version string) (*Set, error) {
	if err := process.VerifySums(archivePath, sumsPath); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(archivePath)
	if err != nil {
		return nil, err
	}
	return ReadArchive(bytes.NewReader(data), source, version)
}

// WriteArchive writes the set in the archive form ReadArchive reads:
// skills/<name>/... and VERSION, in path order with fixed metadata.
func WriteArchive(w io.Writer, s *Set) error {
	files := make(map[string][]byte, len(s.Files)+1)
	for p, data := range s.Files {
		files["skills/"+p] = data
	}
	files["VERSION"] = []byte(s.Version + "\n")
	return process.WriteArchive(w, &process.Bundle{Files: files})
}

// SumsLine is the SHA256SUMS line for the file at p.
func SumsLine(data []byte, p string) string { return process.SumsLine(data, p) }

// ValidDigest reports whether s is a set digest.
func ValidDigest(s string) bool { return digestRE.MatchString(s) }
