package process

// A bundle is a process set as the hub stores and serves it: the manifest
// and every file it names, as bytes, with a digest over them
// (docs/DESIGN-portable-team-context.md, increment 1d). The admin's
// command on the hub host builds one from Git or from an archive, the hub
// validates and keeps it with the selection, and a member reads it from
// the hub, verifies the digest and caches it by that digest. A member
// therefore needs no Git access to the process repository, and the hub
// runs no Git at all.

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// MaxSetBytes bounds the files of one set together; MaxFileBytes bounds
// each. A set is a handbook, a few checklists and templates; anything near
// this bound is a mistake, not a process.
const MaxSetBytes = 4 << 20

// MaxSetFiles bounds how many files a manifest may name.
const MaxSetFiles = 64

var digestRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Bundle is one process set's content. Files is keyed by the repository
// path and holds every file the manifest names, plus the manifest itself.
type Bundle struct {
	Version  int               `json:"version"`
	Manifest string            `json:"manifest"`
	Files    map[string][]byte `json:"files"`
}

// Digest is the SHA-256 over the canonical encoding of files: for each
// path in byte order, the path's length as an unsigned varint, the path,
// the content's length as an unsigned varint, the content. The same files
// always give the same digest, whatever produced them; one changed byte,
// one renamed or extra file changes it.
func Digest(files map[string][]byte) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	var n [binary.MaxVarintLen64]byte
	for _, p := range paths {
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(p)))])
		io.WriteString(h, p)
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(files[p])))])
		h.Write(files[p])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Digest of the bundle's files.
func (b *Bundle) Digest() string { return Digest(b.Files) }

// Bytes is the size of the bundle's files together.
func (b *Bundle) Bytes() int {
	n := 0
	for _, f := range b.Files {
		n += len(f)
	}
	return n
}

// Validate checks that the bundle is exactly one process set: version 1,
// a manifest that parses, every file it names present and within bounds,
// and no file it does not name. The checklists and templates must parse
// too, so a set the hub accepts is a set every member can load.
func (b *Bundle) Validate() error {
	if b.Version != 1 {
		return fmt.Errorf("set version %d is not supported (want 1)", b.Version)
	}
	if err := checkRepoPath(b.Manifest); err != nil {
		return fmt.Errorf("manifest path: %w", err)
	}
	raw, ok := b.Files[b.Manifest]
	if !ok {
		return fmt.Errorf("the set lacks its manifest %s", b.Manifest)
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return err
	}
	named := map[string]bool{b.Manifest: true}
	for _, p := range m.files() {
		named[p] = true
	}
	if len(named) > MaxSetFiles {
		return fmt.Errorf("the manifest names %d files, more than %d", len(named), MaxSetFiles)
	}
	total := 0
	for p, data := range b.Files {
		if !named[p] {
			return fmt.Errorf("file %s is not named by the manifest", p)
		}
		if len(data) > MaxFileBytes {
			return fmt.Errorf("%s is larger than %d bytes", p, MaxFileBytes)
		}
		total += len(data)
	}
	if total > MaxSetBytes {
		return fmt.Errorf("the set is %d bytes, larger than %d", total, MaxSetBytes)
	}
	for p := range named {
		if _, ok := b.Files[p]; !ok {
			return fmt.Errorf("the set lacks %s, which the manifest names", p)
		}
	}
	// Everything a member would parse is parsed here once.
	_, err = loadSetFrom(func(p string) ([]byte, error) {
		data, ok := b.Files[p]
		if !ok {
			return nil, fmt.Errorf("%s missing", p)
		}
		return data, nil
	}, Ref{Manifest: b.Manifest})
	return err
}

// Set loads the bundle as the set for ref, which must name the bundle's
// manifest. It is loaded from memory; Dir is empty.
func (b *Bundle) Set(ref Ref) (*Set, error) {
	if ref.Manifest != b.Manifest {
		return nil, fmt.Errorf("the set was built for manifest %q, not %q", b.Manifest, ref.Manifest)
	}
	return loadSetFrom(func(p string) ([]byte, error) {
		data, ok := b.Files[p]
		if !ok {
			return nil, fmt.Errorf("set incomplete: %s missing", p)
		}
		return data, nil
	}, ref)
}

// BundleFromDir reads the set for ref from a complete cache entry (a
// finished fetch), as the admin's command builds the bundle it sends.
func BundleFromDir(dir string, ref Ref) (*Bundle, error) {
	set, err := loadSet(dir, ref)
	if err != nil {
		return nil, err
	}
	b := &Bundle{Version: 1, Manifest: ref.Manifest, Files: map[string][]byte{}}
	for _, p := range append([]string{ref.Manifest}, set.Manifest.files()...) {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
		if err != nil {
			return nil, err
		}
		b.Files[p] = data
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}

// SetName is how an agent is told which process it follows, in place of
// the repository URL: the manifest's directory ("aimem" for
// processes/aimem/manifest.json), or, for a manifest at the repository's
// root, the repository's last path element without ".git".
func SetName(repo, manifest string) string {
	if d := path.Dir(manifest); d != "." && d != "/" {
		return path.Base(d)
	}
	r := strings.TrimSuffix(strings.TrimRight(repo, "/"), ".git")
	if i := strings.LastIndexAny(r, "/:"); i >= 0 {
		r = r[i+1:]
	}
	if r == "" {
		return "process"
	}
	return r
}

// DisplayName is the set's name: the one the selection recorded, or the
// derived one.
func (r Ref) DisplayName() string {
	if r.Name != "" {
		return r.Name
	}
	return SetName(r.Repo, r.Manifest)
}

// SetDir is where a set read from the hub lives on this machine: keyed by
// its digest alone, because the digest is the content.
func SetDir(root, digest string) string {
	return filepath.Join(root, "process", "sets", digest)
}

// StoreSet writes the bundle under its digest as a complete cache entry,
// atomically: files first into a temporary directory, the marker last,
// then one rename. digest must be the bundle's own; a mismatch is refused
// before anything is written.
func StoreSet(root string, b *Bundle, digest string) (string, error) {
	if !digestRE.MatchString(digest) {
		return "", errors.New("digest must be 64 lowercase hex characters")
	}
	if got := b.Digest(); got != digest {
		return "", fmt.Errorf("the set's digest is %s, not the selection's %s", got[:12], digest[:12])
	}
	if err := b.Validate(); err != nil {
		return "", err
	}
	dir := SetDir(root, digest)
	if _, err := os.Stat(filepath.Join(dir, completeMarker)); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".tmp-"+digest[:12]+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	for p, data := range b.Files {
		fp := filepath.Join(tmp, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(fp), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(fp, data, 0o600); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, completeMarker), []byte(b.Manifest+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dir); err != nil {
		if _, err2 := os.Stat(filepath.Join(dir, completeMarker)); err2 != nil {
			return "", err
		}
	}
	return dir, nil
}

// CachedSet returns the set for ref from this machine's entry for the
// selection's digest, complete, and never contacts anything.
func CachedSet(root string, ref Ref) (*Set, error) {
	if !digestRE.MatchString(ref.Digest) {
		return nil, errors.New("the selection names no stored set")
	}
	set, err := loadSet(SetDir(root, ref.Digest), ref)
	if err != nil {
		return nil, fmt.Errorf("set %s is not complete in this machine's cache: %w", ref.Digest[:12], err)
	}
	set.FromCache = true
	return set, nil
}

// The archive form: a gzip-compressed tar of the set's files at their
// repository paths, which is also what `git archive` and a forge's source
// tarball produce for the whole repository (with one leading directory,
// which ReadArchive strips). Beside it, a SHA256SUMS file in the usual
// two-column form names the archive's digest.

// WriteArchive writes the bundle's files as a tar.gz, in path order, with
// fixed metadata, so the same bundle gives the same bytes.
func WriteArchive(w io.Writer, b *Bundle) error { return WriteArchiveAt(w, b, "") }

// WriteArchiveAt is WriteArchive recording commit first, as git archive
// does, in a pax global header whose comment ArchiveCommit reads; an
// empty commit records none.
func WriteArchiveAt(w io.Writer, b *Bundle, commit string) error {
	paths := make([]string, 0, len(b.Files))
	for p := range b.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if commit != "" {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header", PAXRecords: map[string]string{"comment": commit}, Format: tar.FormatPAX}); err != nil {
			return err
		}
	}
	for _, p := range paths {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: p, Mode: 0o644, Size: int64(len(b.Files[p])), Format: tar.FormatPAX}); err != nil {
			return err
		}
		if _, err := tw.Write(b.Files[p]); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// ReadArchive reads a tar.gz and returns the bundle for the manifest at
// manifestPath: the manifest and the files it names, nothing else. An
// archive whose entries all share one leading directory (a forge tarball)
// is read below that directory. Files are bounded as a fetch bounds them.
func ReadArchive(r io.Reader, manifestPath string) (*Bundle, error) {
	if err := checkRepoPath(manifestPath); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
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
			// Only files the manifest names must fit, but one this large is
			// never one of them; skipping it keeps memory bounded.
			if _, err := io.CopyN(io.Discard, tr, hdr.Size); err != nil {
				return nil, fmt.Errorf("archive: %w", err)
			}
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
	prefix := ""
	if _, ok := all[manifestPath]; !ok {
		// One leading directory for every entry: a forge tarball.
		var top string
		for name := range all {
			i := strings.IndexByte(name, '/')
			if i < 0 {
				top = ""
				break
			}
			if top == "" {
				top = name[:i+1]
			} else if name[:i+1] != top {
				top = ""
				break
			}
		}
		if top == "" {
			return nil, fmt.Errorf("archive: no %s in it", manifestPath)
		}
		if _, ok := all[top+manifestPath]; !ok {
			return nil, fmt.Errorf("archive: no %s in it (under %s)", manifestPath, top)
		}
		prefix = top
	}
	m, err := ParseManifest(all[prefix+manifestPath])
	if err != nil {
		return nil, err
	}
	b := &Bundle{Version: 1, Manifest: manifestPath, Files: map[string][]byte{}}
	for _, p := range append([]string{manifestPath}, m.files()...) {
		data, ok := all[prefix+p]
		if !ok {
			return nil, fmt.Errorf("archive: the manifest names %s, which is not in it", p)
		}
		b.Files[p] = data
	}
	if err := b.Validate(); err != nil {
		return nil, err
	}
	return b, nil
}

// SumsLine is the SHA256SUMS line for the file at p.
func SumsLine(data []byte, p string) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]) + "  " + filepath.Base(p) + "\n"
}

// VerifySums checks that the SHA256SUMS file at sumsPath names the file at
// archivePath (by its base name) with its actual SHA-256. No line for it,
// or another digest, is a refusal: an archive that no sums file vouches
// for is not imported.
func VerifySums(archivePath, sumsPath string) error {
	data, err := os.ReadFile(archivePath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	want := hex.EncodeToString(sum[:])
	f, err := os.Open(sumsPath)
	if err != nil {
		return fmt.Errorf("SHA256SUMS: %w", err)
	}
	defer f.Close()
	base := filepath.Base(archivePath)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == base {
			if strings.ToLower(fields[0]) == want {
				return nil
			}
			return fmt.Errorf("%s does not match its line in %s", base, sumsPath)
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("SHA256SUMS: %w", err)
	}
	return fmt.Errorf("%s has no line for %s", sumsPath, base)
}

// ReadArchiveFile reads and verifies an archive on disk: the sums file is
// checked first, then the archive is read for manifestPath.
func ReadArchiveFile(archivePath, sumsPath, manifestPath string) (*Bundle, error) {
	if err := VerifySums(archivePath, sumsPath); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(archivePath)
	if err != nil {
		return nil, err
	}
	return ReadArchive(bytes.NewReader(data), manifestPath)
}
