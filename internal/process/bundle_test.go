package process

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureBundle() *Bundle {
	b := &Bundle{Version: 1, Manifest: "process/manifest.json", Files: map[string][]byte{}}
	for p, body := range fixtureFiles() {
		b.Files[p] = []byte(body)
	}
	return b
}

// The digest is a function of the files alone: the same files always give
// it, whatever produced them; a changed byte, a renamed or an extra file
// changes it.
func TestDigestIsContentAlone(t *testing.T) {
	a, b := fixtureBundle(), fixtureBundle()
	if a.Digest() != b.Digest() || !digestRE.MatchString(a.Digest()) {
		t.Fatalf("digest: %s %s", a.Digest(), b.Digest())
	}
	b.Files["process/HANDBOOK.md"] = append(b.Files["process/HANDBOOK.md"], '\n')
	if a.Digest() == b.Digest() {
		t.Fatal("a changed byte kept the digest")
	}
	c := fixtureBundle()
	c.Files["process/handbook.md"] = c.Files["process/HANDBOOK.md"]
	delete(c.Files, "process/HANDBOOK.md")
	if a.Digest() == c.Digest() {
		t.Fatal("a renamed file kept the digest")
	}
	// The length prefixes keep path and content apart: moving a byte from
	// the end of one name to the start of the next content is a change.
	x := Digest(map[string][]byte{"ab": []byte("c"), "d": []byte("")})
	y := Digest(map[string][]byte{"a": []byte("bc"), "d": []byte("")})
	if x == y {
		t.Fatal("digest does not separate path from content")
	}
}

// A bundle is exactly one set: the manifest and what it names, within
// bounds, every file parsing as a member would parse it.
func TestBundleValidateRefusesWhatAMemberCouldNotLoad(t *testing.T) {
	if err := fixtureBundle().Validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(b *Bundle){
		"version":        func(b *Bundle) { b.Version = 2 },
		"no manifest":    func(b *Bundle) { delete(b.Files, b.Manifest) },
		"manifest path":  func(b *Bundle) { b.Manifest = "../m.json" },
		"missing named":  func(b *Bundle) { delete(b.Files, "process/dod.json") },
		"unnamed extra":  func(b *Bundle) { b.Files["process/notes.md"] = []byte("x") },
		"bad checklist":  func(b *Bundle) { b.Files["process/dor.json"] = []byte(`{"state":"DONE","items":[]}`) },
		"bad manifest":   func(b *Bundle) { b.Files[b.Manifest] = []byte(`{"version":1}`) },
		"file too large": func(b *Bundle) { b.Files["process/HANDBOOK.md"] = bytes.Repeat([]byte("x"), MaxFileBytes+1) },
	}
	for name, mutate := range cases {
		b := fixtureBundle()
		mutate(b)
		if err := b.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	set, err := fixtureBundle().Set(Ref{Manifest: "process/manifest.json", Commit: strings.Repeat("a", 40)})
	if err != nil || set.Handbook == "" || len(set.Checklists) != 2 || len(set.Templates) != 1 {
		t.Fatalf("set from bundle: %+v %v", set, err)
	}
	if _, err := fixtureBundle().Set(Ref{Manifest: "other.json"}); err == nil {
		t.Fatal("a bundle served another manifest")
	}
}

func TestSetNameAndDisplayName(t *testing.T) {
	for _, c := range []struct{ repo, manifest, want string }{
		{"https://example.com/org/process.git", "processes/aimem/manifest.json", "aimem"},
		{"https://example.com/org/process.git", "proc/manifest.json", "proc"},
		{"https://example.com/org/process.git", "manifest.json", "process"},
		{"git@example.com:org/team-process.git", "manifest.json", "team-process"},
		{"https://example.com/org/process/", "manifest.json", "process"},
	} {
		if got := SetName(c.repo, c.manifest); got != c.want {
			t.Errorf("SetName(%s, %s) = %s, want %s", c.repo, c.manifest, got, c.want)
		}
	}
	r := Ref{Repo: "https://example.com/org/process.git", Manifest: "manifest.json"}
	if r.DisplayName() != "process" {
		t.Fatal(r.DisplayName())
	}
	r.Name = "recorded"
	if r.DisplayName() != "recorded" {
		t.Fatal(r.DisplayName())
	}
	if err := (Ref{Repo: r.Repo, Commit: strings.Repeat("a", 40), Manifest: r.Manifest, Digest: "xyz"}).Validate(); err == nil {
		t.Fatal("a malformed digest validated")
	}
	if err := (Ref{Repo: r.Repo, Commit: strings.Repeat("a", 40), Manifest: r.Manifest, Name: "has space"}).Validate(); err == nil {
		t.Fatal("a name with whitespace validated")
	}
}

// A set is stored under its digest, complete or not at all, and read back
// by the digest without contacting anything; the digest must be the set's.
func TestStoreSetAndCachedSet(t *testing.T) {
	root := t.TempDir()
	b := fixtureBundle()
	d := b.Digest()
	if _, err := StoreSet(root, b, strings.Repeat("0", 64)); err == nil {
		t.Fatal("stored under a digest that is not the set's")
	}
	if _, err := os.Stat(filepath.Join(root, "process", "sets")); err == nil {
		t.Fatal("a refused store left a directory")
	}
	dir, err := StoreSet(root, b, d)
	if err != nil || dir != SetDir(root, d) {
		t.Fatalf("store: %s %v", dir, err)
	}
	if _, err := os.Stat(filepath.Join(dir, completeMarker)); err != nil {
		t.Fatal("complete marker missing")
	}
	ref := Ref{Repo: "https://example.com/p.git", Commit: strings.Repeat("a", 40), Manifest: b.Manifest, Digest: d}
	set, err := CachedSet(root, ref)
	if err != nil || !set.FromCache || set.Handbook == "" || set.Dir != dir {
		t.Fatalf("cached set: %+v %v", set, err)
	}
	if _, err := StoreSet(root, b, d); err != nil {
		t.Fatalf("storing again: %v", err)
	}
	// Cached prefers the digest's entry and never needs the commit's.
	if res := Cached(root, ref); res.Status != StatusCached || res.Set == nil {
		t.Fatalf("Cached by digest: %s %v", res.Status, res.Err)
	}
	ref.Digest = ""
	if _, err := CachedSet(root, ref); err == nil {
		t.Fatal("a selection without a digest read a set")
	}
	if res := Cached(root, ref); res.Status == StatusCached {
		t.Fatal("Cached found a commit entry that was never fetched")
	}
}

// The archive form round-trips, reads a forge tarball with one leading
// directory, takes only what the manifest names, and is imported only
// when the SHA256SUMS beside it vouches for it.
func TestArchiveRoundTripAndSums(t *testing.T) {
	b := fixtureBundle()
	var buf bytes.Buffer
	if err := WriteArchive(&buf, b); err != nil {
		t.Fatal(err)
	}
	var again bytes.Buffer
	WriteArchive(&again, fixtureBundle())
	if !bytes.Equal(buf.Bytes(), again.Bytes()) {
		t.Fatal("the archive is not deterministic")
	}
	got, err := ReadArchive(bytes.NewReader(buf.Bytes()), b.Manifest)
	if err != nil || got.Digest() != b.Digest() {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := ReadArchive(bytes.NewReader(buf.Bytes()), "other/manifest.json"); err == nil {
		t.Fatal("an archive without the manifest was read")
	}
	// An archive records its commit only when written with one, as git
	// archive does, and reads the same set either way.
	if c, err := ArchiveCommit(buf.Bytes()); err != nil || c != "" {
		t.Fatalf("an archive written without a commit records %q %v", c, err)
	}
	commit := strings.Repeat("c", 40)
	var at bytes.Buffer
	if err := WriteArchiveAt(&at, b, commit); err != nil {
		t.Fatal(err)
	}
	if c, err := ArchiveCommit(at.Bytes()); err != nil || c != commit {
		t.Fatalf("recorded commit %q %v", c, err)
	}
	if got, err := ReadArchive(bytes.NewReader(at.Bytes()), b.Manifest); err != nil || got.Digest() != b.Digest() {
		t.Fatalf("an archive with its commit: %v", err)
	}

	// A forge tarball: everything under repo-<sha>/, with files the
	// manifest does not name beside the set.
	var forge bytes.Buffer
	gz := gzip.NewWriter(&forge)
	tw := tar.NewWriter(gz)
	files := fixtureFiles()
	files["README.md"] = "not part of the set"
	files["process/notes.md"] = "nor this"
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "process-abc123/", Mode: 0o755})
	for p, body := range files {
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "process-abc123/" + p, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	got, err = ReadArchive(bytes.NewReader(forge.Bytes()), b.Manifest)
	if err != nil || got.Digest() != b.Digest() || len(got.Files) != len(b.Files) {
		t.Fatalf("forge tarball: %v (%d files)", err, len(got.Files))
	}

	dir := t.TempDir()
	archive := filepath.Join(dir, "process-abc.tar.gz")
	sums := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(archive, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchiveFile(archive, sums, b.Manifest); err == nil {
		t.Fatal("imported without a sums file")
	}
	if err := os.WriteFile(sums, []byte("deadbeef  other.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchiveFile(archive, sums, b.Manifest); err == nil {
		t.Fatal("imported with no line for the archive")
	}
	if err := os.WriteFile(sums, []byte(strings.Repeat("0", 64)+"  process-abc.tar.gz\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchiveFile(archive, sums, b.Manifest); err == nil {
		t.Fatal("imported with a wrong digest")
	}
	if err := os.WriteFile(sums, []byte("deadbeef  other.tar.gz\n"+SumsLine(buf.Bytes(), archive)), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = ReadArchiveFile(archive, sums, b.Manifest)
	if err != nil || got.Digest() != b.Digest() {
		t.Fatalf("verified import: %v", err)
	}
	// The binary-mode form (sha256sum -b) is the same line.
	if err := os.WriteFile(sums, []byte(strings.Replace(SumsLine(buf.Bytes(), archive), "  ", " *", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadArchiveFile(archive, sums, b.Manifest); err != nil {
		t.Fatalf("binary-mode sums line: %v", err)
	}
}

// The admin's import from Git: a finished fetch becomes the bundle with
// exactly the manifest's files and the same digest as the archive form.
func TestBundleFromDirMatchesTheArchive(t *testing.T) {
	repo, sha := makeRepo(t, fixtureFiles())
	root := t.TempDir()
	ref := Ref{Repo: "file://" + filepath.ToSlash(repo), Commit: sha, Manifest: "process/manifest.json"}
	res := fetchNoValidate(context.Background(), root, ref)
	if res.Set == nil {
		t.Fatalf("fetch: %s %v", res.Status, res.Err)
	}
	b, err := BundleFromDir(res.Set.Dir, ref)
	if err != nil || b.Digest() != fixtureBundle().Digest() {
		t.Fatalf("bundle from the fetch: %v", err)
	}
}

// tarOf writes a tar.gz of the given headers, each regular file with its
// body.
func tarOf(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := e.hdr
		if hdr.Typeflag == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if hdr.Mode == 0 {
			hdr.Mode = 0o644
		}
		if err := tw.WriteHeader(&hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

type tarEntry struct {
	hdr  tar.Header
	body []byte
}

// setEntries is the fixture set as archive entries, plus extra.
func setEntries(b *Bundle, extra ...tarEntry) []tarEntry {
	var out []tarEntry
	for p, body := range b.Files {
		out = append(out, tarEntry{tar.Header{Typeflag: tar.TypeReg, Name: p}, body})
	}
	return append(out, extra...)
}

// An archive entry whose cleaned name leaves the archive refuses the whole
// archive, even where the manifest never names it (audit T-1). A leading
// slash is dropped after cleaning, so a rooted spelling stays inside and is
// not among these; nor is "./", which the tar reader returns as a directory.
// A set file named ".." is refused by the path check itself, so by the
// manifest, the bundle and the archive reader alike.
func TestReadArchiveRefusesAnEntryThatLeavesIt(t *testing.T) {
	b := fixtureBundle()
	for _, name := range []string{"../evil.md", "./../evil.md", "a/../../evil.md", "process/../../evil.md", ".", ".."} {
		data := tarOf(t, setEntries(b, tarEntry{tar.Header{Typeflag: tar.TypeReg, Name: name}, []byte("evil")}))
		_, err := ReadArchive(bytes.NewReader(data), b.Manifest)
		if err == nil || !strings.Contains(err.Error(), "leaves the archive") {
			t.Errorf("entry %q: want the archive refused as leaving it, got %v", name, err)
		}
	}

	// ".." as a set file: the path check, the manifest, the bundle.
	if err := checkRepoPath(".."); err == nil || !strings.Contains(err.Error(), "inside the repository") {
		t.Errorf("checkRepoPath(\"..\"): %v", err)
	}
	dotdot := []byte(`{"version":1,"handbook":".."}`)
	if _, err := ParseManifest(dotdot); err == nil {
		t.Error(`a manifest whose handbook is ".." was parsed`)
	}
	bad := &Bundle{Version: 1, Manifest: "m.json", Files: map[string][]byte{"m.json": dotdot, "..": []byte("x")}}
	if err := bad.Validate(); err == nil {
		t.Error(`a bundle with a file ".." validated`)
	}

	// A symlink entry is never kept, so it cannot stand in for a set file:
	// an archive whose handbook is only a link has no handbook.
	var linked []tarEntry
	for _, e := range setEntries(b) {
		if e.hdr.Name == "process/HANDBOOK.md" {
			e = tarEntry{tar.Header{Typeflag: tar.TypeSymlink, Name: e.hdr.Name, Linkname: "../../../etc/passwd"}, nil}
		}
		linked = append(linked, e)
	}
	_, err := ReadArchive(bytes.NewReader(tarOf(t, linked)), b.Manifest)
	if err == nil || !strings.Contains(err.Error(), "the manifest names process/HANDBOOK.md, which is not in it") {
		t.Errorf("a symlinked handbook: %v", err)
	}
}

// ReadArchive bounds the files it reads and the bytes of their names,
// besides their data (audit A-16): an archive at each cap is read, one
// past it is refused with a message that names the cap.
func TestReadArchiveBoundsItsFilesAndNames(t *testing.T) {
	b := fixtureBundle()
	set := setEntries(b)
	setNames := 0
	for _, e := range set {
		setNames += len(e.hdr.Name)
	}
	filler := func(n, nameLen int) []tarEntry {
		var out []tarEntry
		for i := 0; i < n; i++ {
			name := fmt.Sprintf("f/%06d", i)
			name += strings.Repeat("x", nameLen-len(name))
			out = append(out, tarEntry{tar.Header{Typeflag: tar.TypeReg, Name: name}, nil})
		}
		return out
	}
	read := func(entries []tarEntry) error {
		_, err := ReadArchive(bytes.NewReader(tarOf(t, entries)), b.Manifest)
		return err
	}

	// Files: at the cap, and one past it.
	atCap := append(append([]tarEntry{}, set...), filler(MaxArchiveEntries-len(set), 8)...)
	if err := read(atCap); err != nil {
		t.Fatalf("%d files: %v", MaxArchiveEntries, err)
	}
	over := append(atCap, filler(1, 8)[0])
	over[len(over)-1].hdr.Name = "f/one-more"
	if err := read(over); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("more than %d files", MaxArchiveEntries)) {
		t.Fatalf("%d files: %v", MaxArchiveEntries+1, err)
	}

	// Name bytes: 1024 names of 1024 bytes fill the cap with the set's
	// names; the last filler is shortened to land on it exactly.
	names := filler(MaxArchiveNameBytes/1024, 1024)
	last := &names[len(names)-1]
	last.hdr.Name = last.hdr.Name[:1024-setNames]
	exact := append(append([]tarEntry{}, set...), names...)
	if err := read(exact); err != nil {
		t.Fatalf("%d bytes of names: %v", MaxArchiveNameBytes, err)
	}
	last.hdr.Name += "y"
	if err := read(append(append([]tarEntry{}, set...), names...)); err == nil ||
		!strings.Contains(err.Error(), fmt.Sprintf("more than %d bytes of file names", MaxArchiveNameBytes)) {
		t.Fatalf("%d bytes of names: %v", MaxArchiveNameBytes+1, err)
	}
}
