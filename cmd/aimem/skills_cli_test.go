package main

// `aimem skills import` on the hub host verifies the archive against its
// SHA256SUMS, reads the skills below the release's leading directory and
// sends them to the hub; `aimem skills export` writes the archive pair back;
// `aimem skills list --hub` prints the hub's index.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/process"
	"aimem/internal/skills"
)

// skillsReleaseFixture writes a release-shaped archive (one leading
// directory, VERSION, skills/<name>/...) and its SHA256SUMS.
func skillsReleaseFixture(t *testing.T, version string, files map[string]string) (archive, sums string) {
	t.Helper()
	all := map[string][]byte{"aiskills-" + version + "/VERSION": []byte(version + "\n"), "aiskills-" + version + "/README.md": []byte("readme\n")}
	for p, c := range files {
		all["aiskills-"+version+"/skills/"+p] = []byte(c)
	}
	var buf bytes.Buffer
	if err := process.WriteArchive(&buf, &process.Bundle{Files: all}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	archive = filepath.Join(dir, "aiskills-"+version+".tar.gz")
	os.WriteFile(archive, buf.Bytes(), 0o600)
	sum := sha256.Sum256(buf.Bytes())
	sums = filepath.Join(dir, "SHA256SUMS")
	os.WriteFile(sums, []byte(hex.EncodeToString(sum[:])+"  "+filepath.Base(archive)+"\n"), 0o600)
	return archive, sums
}

func TestSkillsImportExportListOnTheHubHost(t *testing.T) {
	projectService(t)
	archive, sums := skillsReleaseFixture(t, "1.33.13", map[string]string{"oh-code-review/SKILL.md": "review", "oh-code-review/references/d.md": "d", "verify-delivery/SKILL.md": "verify"})
	run := func(args ...string) (string, error) {
		return stdoutOf(t, func() error { return skillsCmd(args) })
	}
	// A tampered archive, a missing sums line and an unknown source are
	// refused before anything reaches the hub.
	raw, _ := os.ReadFile(archive)
	tampered := filepath.Join(t.TempDir(), filepath.Base(archive))
	os.WriteFile(tampered, append(raw, 0), 0o600)
	if _, err := run("import", "--from-archive", tampered, "--sums", sums); err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := run("import", "--from-archive", tampered); err == nil || !strings.Contains(err.Error(), "SHA256SUMS") {
		t.Fatalf("no sums beside it: %v", err)
	}
	if _, err := run("import", "--from-archive", archive, "--source", "github"); err == nil || !strings.Contains(err.Error(), "release or instance") {
		t.Fatalf("bad source: %v", err)
	}
	if text, _ := run("list", "--hub"); !strings.Contains(text, `"effective":[]`) {
		t.Fatalf("something was imported by a refused command:\n%s", text)
	}
	// The verified archive imports as the release set (SHA256SUMS beside it).
	text, err := run("import", "--from-archive", archive)
	if err != nil || !strings.Contains(text, `"imported":true`) || !strings.Contains(text, `"version":"1.33.13"`) {
		t.Fatalf("import: %v\n%s", err, text)
	}
	// An instance archive without VERSION needs --version; with it, the
	// instance set overrides by name.
	inst := map[string][]byte{"skills/oh-code-review/SKILL.md": []byte("house rules")}
	var buf bytes.Buffer
	process.WriteArchive(&buf, &process.Bundle{Files: inst})
	instArchive := filepath.Join(t.TempDir(), "instance.tar.gz")
	os.WriteFile(instArchive, buf.Bytes(), 0o600)
	sum := sha256.Sum256(buf.Bytes())
	os.WriteFile(filepath.Join(filepath.Dir(instArchive), "SHA256SUMS"), []byte(hex.EncodeToString(sum[:])+"  instance.tar.gz\n"), 0o600)
	if _, err := run("import", "--from-archive", instArchive, "--source", "instance"); err == nil || !strings.Contains(err.Error(), "--version") {
		t.Fatalf("instance without a version: %v", err)
	}
	if text, err := run("import", "--from-archive", instArchive, "--source", "instance", "--version", "site-1"); err != nil || !strings.Contains(text, `"version":"site-1"`) {
		t.Fatalf("instance import: %v\n%s", err, text)
	}
	text, err = run("list", "--hub")
	if err != nil || !strings.Contains(text, `"name":"oh-code-review","version":"site-1","source":"instance"`) || !strings.Contains(text, `"name":"verify-delivery","version":"1.33.13","source":"release"`) {
		t.Fatalf("list --hub: %v\n%s", err, text)
	}
	// export writes the archive pair that import reads.
	out := t.TempDir()
	if text, err := run("export", "--out", out); err != nil || !strings.Contains(text, "skills-release-1.33.13.tar.gz") {
		t.Fatalf("export: %v\n%s", err, text)
	}
	set, err := skills.ReadArchiveFile(filepath.Join(out, "skills-release-1.33.13.tar.gz"), filepath.Join(out, "SHA256SUMS"), skills.SourceRelease, "")
	if err != nil || set.Version != "1.33.13" || len(set.Files) != 3 || string(set.Files["oh-code-review/SKILL.md"]) != "review" {
		t.Fatalf("exported archive: %v %+v", err, set)
	}
	if text, err := run("export", "--out", out, "--source", "instance"); err != nil || !strings.Contains(text, "skills-instance-site-1.tar.gz") {
		t.Fatalf("export instance: %v\n%s", err, text)
	}
	if raw, _ := os.ReadFile(filepath.Join(out, "SHA256SUMS")); strings.Count(string(raw), "\n") != 2 {
		t.Fatalf("SHA256SUMS after two exports:\n%s", raw)
	}
}
