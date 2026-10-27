package skills

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aimem/internal/process"
)

// releaseFiles is a small skills release: two skills with their files.
func releaseFiles() map[string][]byte {
	return map[string][]byte{
		"oh-code-review/SKILL.md":            []byte("---\nname: oh-code-review\n---\nReview.\n"),
		"oh-code-review/references/d.md":     []byte("dispositions\n"),
		"verify-delivery/SKILL.md":           []byte("---\nname: verify-delivery\n---\nVerify.\n"),
		"verify-delivery/verify_delivery.py": []byte("print('ok')\n"),
	}
}

// releaseArchive writes an archive laid out as a skills release
// (aiskills-X.Y.Z/skills/<name>/..., VERSION, and other files beside) with
// its SHA256SUMS, and returns both paths.
func releaseArchive(t *testing.T, version string, files map[string][]byte) (archive, sums string) {
	t.Helper()
	all := map[string][]byte{"aiskills-" + version + "/VERSION": []byte(version + "\n"), "aiskills-" + version + "/README.md": []byte("# aiskills\n"), "aiskills-" + version + "/install.sh": []byte("#!/bin/sh\n")}
	for p, data := range files {
		all["aiskills-"+version+"/skills/"+p] = data
	}
	var buf bytes.Buffer
	if err := process.WriteArchive(&buf, &process.Bundle{Files: all}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archive = filepath.Join(dir, "aiskills-"+version+".tar.gz")
	if err := os.WriteFile(archive, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	sums = filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sums, []byte(hex.EncodeToString(sum[:])+"  "+filepath.Base(archive)+"\n"+strings.Repeat("0", 64)+"  aiskills-"+version+".zip\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return archive, sums
}

func TestReleaseArchiveIsReadBelowItsTopDirectoryAndVerified(t *testing.T) {
	archive, sums := releaseArchive(t, "1.33.13", releaseFiles())
	set, err := ReadArchiveFile(archive, sums, SourceRelease, "")
	if err != nil {
		t.Fatal(err)
	}
	if set.Version != "1.33.13" || set.Source != SourceRelease || len(set.Files) != 4 || strings.Join(set.Names(), ",") != "oh-code-review,verify-delivery" {
		t.Fatalf("set: %s %v", set.Version, set.Names())
	}
	if _, ok := set.Files["oh-code-review/SKILL.md"]; !ok {
		t.Fatalf("files keyed by skill path: %v", set.Files)
	}
	// --version overrides the archive's VERSION; an instance archive
	// without VERSION needs it.
	if s, err := ReadArchiveFile(archive, sums, SourceInstance, "site-7"); err != nil || s.Version != "site-7" {
		t.Fatalf("version override: %v %v", s, err)
	}
	// A tampered archive, or one the sums file does not name, is refused.
	raw, _ := os.ReadFile(archive)
	tampered := filepath.Join(t.TempDir(), filepath.Base(archive))
	os.WriteFile(tampered, append(raw, 0), 0o600)
	if _, err := ReadArchiveFile(tampered, sums, SourceRelease, ""); err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("tampered: %v", err)
	}
	other := filepath.Join(t.TempDir(), "other.tar.gz")
	os.WriteFile(other, raw, 0o600)
	if _, err := ReadArchiveFile(other, sums, SourceRelease, ""); err == nil || !strings.Contains(err.Error(), "no line for") {
		t.Fatalf("unnamed: %v", err)
	}
	// The export form round-trips through the same reader.
	var buf bytes.Buffer
	if err := WriteArchive(&buf, set); err != nil {
		t.Fatal(err)
	}
	again, err := ReadArchive(bytes.NewReader(buf.Bytes()), SourceRelease, "")
	if err != nil || again.Digest() != set.Digest() || again.Version != "1.33.13" {
		t.Fatalf("round trip: %v", err)
	}
}

// A skill file over the bound refuses the archive whole; it is never
// dropped silently, which would import an incomplete skill. A large file
// beside the skills (an installer's binary) is not a skill's and is fine.
func TestOversizedSkillFileRefusesTheArchive(t *testing.T) {
	files := releaseFiles()
	files["oh-code-review/references/huge.bin"] = bytes.Repeat([]byte("x"), MaxFileBytes+1)
	archive, sums := releaseArchive(t, "1.33.14", files)
	if _, err := ReadArchiveFile(archive, sums, SourceRelease, ""); err == nil || !strings.Contains(err.Error(), "huge.bin is larger than") {
		t.Fatalf("oversized skill file: %v", err)
	}
	var buf bytes.Buffer
	all := map[string][]byte{"aiskills-2/VERSION": []byte("2\n"), "aiskills-2/bin/tool": bytes.Repeat([]byte("y"), MaxFileBytes+1)}
	for p, data := range releaseFiles() {
		all["aiskills-2/skills/"+p] = data
	}
	if err := process.WriteArchive(&buf, &process.Bundle{Files: all}); err != nil {
		t.Fatal(err)
	}
	set, err := ReadArchive(&buf, SourceRelease, "")
	if err != nil || len(set.Files) != 4 {
		t.Fatalf("large file beside the skills: %v %v", err, set)
	}
}

func TestSetValidation(t *testing.T) {
	good := func() *Set { return &Set{Source: SourceRelease, Version: "1.0", Files: releaseFiles()} }
	if err := good().Validate(); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		mut  func(s *Set)
		want string
	}{
		{"source", func(s *Set) { s.Source = "github" }, "not release or instance"},
		{"version", func(s *Set) { s.Version = "" }, "version must be"},
		{"empty", func(s *Set) { s.Files = nil }, "no files"},
		{"no SKILL.md", func(s *Set) { delete(s.Files, "verify-delivery/SKILL.md") }, "has no SKILL.md"},
		{"bad name", func(s *Set) { s.Files["Bad Name/SKILL.md"] = []byte("x") }, "plain name"},
		{"top-level file", func(s *Set) { s.Files["README.md"] = []byte("x") }, "not inside a skill directory"},
		{"escape", func(s *Set) { s.Files["oh-code-review/../x"] = []byte("x") }, "clean relative path"},
		{"too large", func(s *Set) { s.Files["oh-code-review/big"] = make([]byte, MaxFileBytes+1) }, "larger than"},
	} {
		s := good()
		c.mut(s)
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v (want %q)", c.name, err, c.want)
		}
	}
	// An archive whose skill lacks SKILL.md is refused as a whole.
	files := releaseFiles()
	delete(files, "verify-delivery/SKILL.md")
	archive, sums := releaseArchive(t, "1.0", files)
	if _, err := ReadArchiveFile(archive, sums, SourceRelease, ""); err == nil || !strings.Contains(err.Error(), "no SKILL.md") {
		t.Fatalf("archive without SKILL.md: %v", err)
	}
}

func TestStoreImportKeepsFivePerSourceAndPrunes(t *testing.T) {
	root := t.TempDir()
	st := NewStore(root)
	ix, err := st.Load()
	if err != nil || len(ix.Sources) != 0 || len(ix.Effective()) != 0 {
		t.Fatalf("empty store: %+v %v", ix, err)
	}
	now := time.Date(2026, 10, 18, 20, 0, 0, 0, time.UTC)
	var digests []string
	for i := 0; i < 7; i++ {
		files := releaseFiles()
		files["oh-code-review/SKILL.md"] = []byte("version " + string(rune('a'+i)))
		set := &Set{Source: SourceRelease, Version: "1." + string(rune('0'+i)), Files: files}
		e, written, err := st.Import(set, "admin", now.Add(time.Duration(i)*time.Minute))
		if err != nil || !written || e.Digest != set.Digest() || e.Version != set.Version || len(e.Skills) != 2 {
			t.Fatalf("import %d: %+v %v %v", i, e, written, err)
		}
		digests = append(digests, e.Digest)
	}
	ix, err = st.Load()
	if err != nil {
		t.Fatal(err)
	}
	rel := ix.Sources[SourceRelease]
	if rel.Current.Version != "1.6" || len(rel.Kept) != Kept || rel.Kept[0].Digest != digests[6] || rel.Kept[4].Digest != digests[2] {
		t.Fatalf("kept: %+v", rel)
	}
	for i, d := range digests {
		if st.Has(d) != (i >= 2) {
			t.Fatalf("set %d kept on disk: %v", i, st.Has(d))
		}
	}
	// Re-importing a kept digest makes it current without writing files.
	files := releaseFiles()
	files["oh-code-review/SKILL.md"] = []byte("version d")
	e, written, err := st.Import(&Set{Source: SourceRelease, Version: "1.3", Files: files}, "admin", now.Add(time.Hour))
	if err != nil || written || e.Digest != digests[3] {
		t.Fatalf("re-import: %+v %v %v", e, written, err)
	}
	ix, _ = st.Load()
	if ix.Sources[SourceRelease].Current.Digest != digests[3] || len(ix.Sources[SourceRelease].Kept) != Kept {
		t.Fatalf("re-import kept list: %+v", ix.Sources[SourceRelease])
	}
	// A set that does not validate stores nothing and changes nothing.
	before, _ := os.ReadFile(st.IndexPath())
	if _, _, err := st.Import(&Set{Source: SourceInstance, Version: "x", Files: map[string][]byte{"a/b.md": []byte("x")}}, "admin", now); err == nil {
		t.Fatal("invalid set imported")
	}
	after, _ := os.ReadFile(st.IndexPath())
	if !bytes.Equal(before, after) {
		t.Fatal("a refused import changed the index")
	}
	// The instance source is layered on top: its skill of the same name
	// overrides the release's; the other release skill stays.
	inst := &Set{Source: SourceInstance, Version: "site-1", Files: map[string][]byte{"oh-code-review/SKILL.md": []byte("house rules"), "house-style/SKILL.md": []byte("style")}}
	if _, _, err := st.Import(inst, "admin", now); err != nil {
		t.Fatal(err)
	}
	ix, _ = st.Load()
	eff := ix.Effective()
	if len(eff) != 3 || eff[0].Name != "house-style" || eff[0].Source != SourceInstance || eff[1].Name != "oh-code-review" || eff[1].Source != SourceInstance || eff[1].Version != "site-1" || eff[2].Name != "verify-delivery" || eff[2].Source != SourceRelease || eff[2].Version != "1.3" {
		t.Fatalf("effective: %+v", eff)
	}
	// Read gives back exactly the stored files; a damaged entry is refused.
	got, err := st.Read(inst.Digest())
	if err != nil || string(got["house-style/SKILL.md"]) != "style" || len(got) != 2 {
		t.Fatalf("read: %v %v", got, err)
	}
	os.WriteFile(filepath.Join(SetDir(root, inst.Digest()), "house-style", "SKILL.md"), []byte("changed"), 0o600)
	if _, err := st.Read(inst.Digest()); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Fatalf("damaged entry: %v", err)
	}
	// An index that exists and cannot be read is a fault, not "no skills".
	os.WriteFile(st.IndexPath(), []byte("{not json"), 0o600)
	if _, err := st.Load(); err == nil || !strings.Contains(err.Error(), "not readable") {
		t.Fatalf("corrupt index: %v", err)
	}
}

// Two versions with the same files (a release that changed only its
// installer) are two kept imports sharing one stored set, so a pin on
// either version resolves; the same version re-imported with the same
// files replaces its entry.
func TestSameContentUnderTwoVersionsKeepsBoth(t *testing.T) {
	root := t.TempDir()
	st := NewStore(root)
	now := time.Date(2026, 10, 18, 20, 0, 0, 0, time.UTC)
	e10, written, err := st.Import(&Set{Source: SourceRelease, Version: "1.0", Files: releaseFiles()}, "admin", now)
	if err != nil || !written {
		t.Fatal(err)
	}
	e11, written, err := st.Import(&Set{Source: SourceRelease, Version: "1.1", Files: releaseFiles()}, "admin", now.Add(time.Minute))
	if err != nil || written || e11.Digest != e10.Digest {
		t.Fatalf("second import: %+v %v %v", e11, written, err)
	}
	ix, _ := st.Load()
	rel := ix.Sources[SourceRelease]
	if rel.Current.Version != "1.1" || len(rel.Kept) != 2 || rel.Kept[1].Version != "1.0" {
		t.Fatalf("kept: %+v", rel)
	}
	if !st.Has(e10.Digest) {
		t.Fatal("the shared files were pruned")
	}
	answer := &IndexAnswer{Sources: ix.Current(), Kept: map[string][]Entry{SourceRelease: rel.Kept}, Effective: ix.Effective()}
	picks, problems := Resolve(answer, []process.SkillPin{{Name: "oh-code-review", Version: "1.0", Source: SourceRelease}, {Name: "verify-delivery", Version: "1.1", Source: SourceRelease}})
	if len(problems) != 0 || len(picks) != 2 || picks[0].Version != "1.0" || picks[1].Version != "1.1" || picks[0].Digest != e10.Digest {
		t.Fatalf("resolve: %+v %v", picks, problems)
	}
	// The same version again, same files: one entry, still current.
	if _, _, err := st.Import(&Set{Source: SourceRelease, Version: "1.1", Files: releaseFiles()}, "admin", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ix, _ = st.Load()
	if rel := ix.Sources[SourceRelease]; len(rel.Kept) != 2 || rel.Current.Version != "1.1" {
		t.Fatalf("re-import of the same version: %+v", rel)
	}
}

// The member's trim never removes a set the index keeps: on the hub host
// the admin's cache and the hub's store share one root. With no readable
// index it trims nothing.
func TestTrimSparesTheIndexKeptSets(t *testing.T) {
	root := t.TempDir()
	st := NewStore(root)
	now := time.Date(2026, 10, 18, 20, 0, 0, 0, time.UTC)
	var kept []string
	for i := 0; i < 3; i++ {
		files := releaseFiles()
		files["oh-code-review/SKILL.md"] = []byte("v" + string(rune('a'+i)))
		e, _, err := st.Import(&Set{Source: SourceRelease, Version: "1." + string(rune('0'+i)), Files: files}, "admin", now)
		if err != nil {
			t.Fatal(err)
		}
		kept = append(kept, e.Digest)
	}
	// Two cached-only sets, older than the imports.
	var cached []string
	for i := 0; i < 2; i++ {
		files := map[string][]byte{"x/SKILL.md": []byte("cached " + string(rune('a'+i)))}
		d := (&Set{Files: files}).Digest()
		if _, err := st.Put(files, d); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(filepath.Join(SetDir(root, d), completeMarker), now.Add(-time.Hour), now.Add(-time.Hour))
		cached = append(cached, d)
	}
	st.Trim(0)
	for _, d := range kept {
		if !st.Has(d) {
			t.Fatalf("trim removed a set the index keeps: %s", d[:12])
		}
	}
	if st.Has(cached[0]) || st.Has(cached[1]) {
		t.Fatal("trim kept a cached-only set over the bound")
	}
	// A cache entry damaged under its marker is removed on request.
	if err := st.Remove(kept[0]); err != nil || st.Has(kept[0]) {
		t.Fatalf("remove: %v", err)
	}
	// No readable index: nothing is trimmed.
	os.WriteFile(st.IndexPath(), []byte("{not json"), 0o600)
	st.Trim(0)
	if !st.Has(kept[1]) || !st.Has(kept[2]) {
		t.Fatal("a trim under a corrupt index removed sets")
	}
}

func TestResolvePinsAgainstTheIndex(t *testing.T) {
	ix := &IndexAnswer{
		Sources: map[string]*Entry{
			SourceRelease:  {Version: "1.33.13", Digest: strings.Repeat("a", 64), Skills: []string{"oh-code-review", "verify-delivery"}},
			SourceInstance: {Version: "site-2", Digest: strings.Repeat("b", 64), Skills: []string{"oh-code-review"}},
		},
		Kept: map[string][]Entry{
			SourceRelease: {{Version: "1.33.13", Digest: strings.Repeat("a", 64), Skills: []string{"oh-code-review", "verify-delivery"}}, {Version: "1.33.12", Digest: strings.Repeat("c", 64), Skills: []string{"oh-code-review", "verify-delivery"}}},
		},
	}
	ix.Effective = Effective(ix.Sources)
	picks, problems := Resolve(ix, []process.SkillPin{
		{Name: "oh-code-review"}, // unpinned: the effective one, the instance's
		{Name: "verify-delivery", Version: "1.33.12", Source: "release"}, // a kept version
		{Name: "oh-code-review", Version: "1.33.13", Source: "release"},  // the release copy, by the pin's source
		{Name: "verify-delivery", Version: "1.33.11", Source: "release"}, // not kept
		{Name: "house-style", Version: "site-2", Source: "instance"},     // not in the set
		{Name: "nope"}, // nowhere
	})
	if len(picks) != 3 || picks[0].Source != SourceInstance || picks[0].Version != "site-2" || picks[1].Digest != strings.Repeat("c", 64) || picks[2].Source != SourceRelease || picks[2].Digest != strings.Repeat("a", 64) {
		t.Fatalf("picks: %+v", picks)
	}
	if len(problems) != 3 || !strings.Contains(problems[0], "1.33.11 (release) is not on the hub: the current release set is 1.33.13") || !strings.Contains(problems[1], "which has no skill house-style") || !strings.Contains(problems[2], "nope: no skill of that name") {
		t.Fatalf("problems: %q", problems)
	}
}

// A required skill the hub cannot serve names the command that imports
// it on the hub host: the release archive for a release skill, the
// instance set for any other, with the manual's section.
func TestImportFixNamesTheCommand(t *testing.T) {
	const manual = " (docs/ADMIN-MANUAL.md#importing-the-skills-sets)"
	release := func(v string) string {
		return "the admin imports the skills release on the hub host: `aimem skills import --from-archive aiskills-" + v + ".tar.gz`" + manual
	}
	instance := func(v string) string {
		return "the admin imports the instance set on the hub host: `aimem skills import --from-archive <file> --source instance --version " + v + "`" + manual
	}
	empty := &IndexAnswer{Sources: map[string]*Entry{SourceRelease: nil, SourceInstance: nil}}
	onHub := &IndexAnswer{
		Sources: map[string]*Entry{SourceRelease: {Version: "1.34.0", Skills: []string{"verify-delivery"}}},
		Kept:    map[string][]Entry{SourceRelease: {{Version: "1.33.13", Skills: []string{"oh-qa-changes"}}}},
	}
	pinnedRelease := process.SkillPin{Name: "verify-delivery", Version: "1.34.0", Source: "release"}
	for _, c := range []struct {
		name string
		ix   *IndexAnswer
		pins []process.SkillPin
		p    process.SkillPin
		want string
	}{
		{"no set imported, a release name, the version another pin gives", empty, []process.SkillPin{{Name: "oh-code-review"}, pinnedRelease}, process.SkillPin{Name: "oh-code-review"}, release("1.34.0")},
		{"no set imported, a release name, no version pinned", empty, []process.SkillPin{{Name: "oh-code-review"}}, process.SkillPin{Name: "oh-code-review"}, release("<version>")},
		{"no set imported, another name", empty, []process.SkillPin{{Name: "house-style"}}, process.SkillPin{Name: "house-style"}, "it is not in the skills release, so it must come from the instance set: " + instance("<version>")},
		{"a release set imported decides over the built-in names", onHub, []process.SkillPin{{Name: "forge-gates"}}, process.SkillPin{Name: "forge-gates"}, "it is not in the skills release, so it must come from the instance set: " + instance("<version>")},
		{"a kept release set lists it", onHub, []process.SkillPin{{Name: "oh-qa-changes"}}, process.SkillPin{Name: "oh-qa-changes"}, release("<version>")},
		{"a release pin", onHub, nil, process.SkillPin{Name: "oh-code-review", Version: "1.33.11", Source: "release"}, release("1.33.11")},
		{"an instance pin", empty, nil, process.SkillPin{Name: "oh-code-review", Version: "site-2", Source: "instance"}, instance("site-2")},
	} {
		if got := ImportFix(c.ix, c.pins, c.p); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
	// The hub without any set (2026-10-26): every problem carries its fix.
	_, problems := Resolve(empty, []process.SkillPin{{Name: "oh-code-review"}, pinnedRelease})
	want := []string{
		"oh-code-review: no skill of that name in the hub's sets; " + release("1.34.0"),
		"verify-delivery 1.34.0 (release) is not on the hub: no release set is imported; " + release("1.34.0") + ", or the process pins the current one",
	}
	if got, w := strings.Join(problems, "\n"), strings.Join(want, "\n"); got != w {
		t.Fatalf("problems:\n%s\nwant:\n%s", got, w)
	}
}

func TestInstallWritesSkillsWholeAndRecordsVersions(t *testing.T) {
	files := releaseFiles()
	digest := (&Set{Files: files}).Digest()
	instFiles := map[string][]byte{"oh-code-review/SKILL.md": []byte("house rules")}
	instDigest := (&Set{Files: instFiles}).Digest()
	sets := map[string]map[string][]byte{digest: files, instDigest: instFiles}
	read := func(d string) (map[string][]byte, error) { return sets[d], nil }
	dir := t.TempDir()
	claude := filepath.Join(dir, ".claude", "skills")
	// A copy by hand with a stray file, as the installer would also replace.
	os.MkdirAll(filepath.Join(claude, "verify-delivery"), 0o755)
	os.WriteFile(filepath.Join(claude, "verify-delivery", "stale.txt"), []byte("old"), 0o644)
	// An untouched skill the installers' record lists under its one version.
	os.MkdirAll(filepath.Join(claude, "oh-technical-writing"), 0o755)
	os.WriteFile(filepath.Join(claude, "oh-technical-writing", "SKILL.md"), []byte("write"), 0o644)
	os.WriteFile(filepath.Join(claude, ".ai-skills.json"), []byte(`{"version":"1.20.0","commit":"abc","skills":["verify-delivery","oh-technical-writing"],"archive_sha256":"x","installed_at":"2026-01-01T00:00:00Z"}`), 0o644)
	targets := []Target{{Client: "claude", Dir: claude}, {Client: "codex", Dir: filepath.Join(dir, ".agents", "skills")}}
	picks := []Skill{
		{Name: "verify-delivery", Version: "1.33.13", Source: SourceRelease, Digest: digest},
		{Name: "oh-code-review", Version: "site-2", Source: SourceInstance, Digest: instDigest},
	}
	now := time.Date(2026, 10, 18, 21, 0, 0, 0, time.UTC)
	done, err := Install(targets, picks, read, now)
	if err != nil || len(done) != 4 || !done[0].Replaced || done[1].Replaced || done[2].Replaced {
		t.Fatalf("install: %+v %v", done, err)
	}
	if _, err := os.Stat(filepath.Join(claude, "verify-delivery", "stale.txt")); err == nil {
		t.Fatal("the old directory was not replaced whole")
	}
	for _, p := range []string{
		filepath.Join(claude, "verify-delivery", "verify_delivery.py"),
		filepath.Join(claude, "oh-code-review", "SKILL.md"),
		filepath.Join(dir, ".agents", "skills", "verify-delivery", "SKILL.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("missing %s", p)
		}
	}
	if raw, _ := os.ReadFile(filepath.Join(claude, "oh-code-review", "SKILL.md")); string(raw) != "house rules" {
		t.Fatalf("the instance copy was not the one installed: %q", raw)
	}
	// The record: the installers' fields kept, the skills merged, the
	// version and source per skill, and the bootstrap reads them back.
	rec := process.ReadInstalledRecord(claude)
	if rec == nil || rec.Commit != "abc" || rec.Version != "1.33.13" || strings.Join(rec.Skills, ",") != "oh-code-review,oh-technical-writing,verify-delivery" || rec.InstalledAt != "2026-10-18T21:00:00Z" {
		t.Fatalf("record: %+v", rec)
	}
	state := process.SkillStateOn(dir, "")
	if st := state("oh-code-review"); !st.Installed || st.Version != "site-2" || st.Source != SourceInstance {
		t.Fatalf("state after install: %+v", st)
	}
	// The untouched skill keeps the version it was installed at: the
	// record's one version moved to 1.33.13, its own entry did not.
	if st := state("oh-technical-writing"); !st.Installed || st.Version != "1.20.0" || rec.Versions["oh-technical-writing"] != "1.20.0" {
		t.Fatalf("an untouched skill took the new version: %+v %v", st, rec.Versions)
	}
	if line := process.SkillLine(process.SkillPin{Name: "oh-technical-writing", Version: "1.33.13", Source: "release"}, state("oh-technical-writing")); line != "oh-technical-writing 1.33.13 (release; installed 1.20.0, not from the hub, run `aimem skills install`)" {
		t.Fatalf("untouched skill line: %q", line)
	}
	if line := process.SkillLine(process.SkillPin{Name: "verify-delivery", Version: "1.33.13", Source: "release"}, state("verify-delivery")); line != "verify-delivery 1.33.13 (release; installed 1.33.13 from the hub (release))" {
		t.Fatalf("line: %q", line)
	}
}
