package installer

// The hub installer stages its binary from AIMEM_PREBUILT_DIR with the
// same SHA256SUMS check as a download, and aborts before anything is
// installed when the directory cannot be verified.

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runStage runs the binary-stage block's stage_binary with env added; the
// download path's fetch copies from the directory named by FAKE_RELEASE.
func runStage(t *testing.T, env ...string) (staged string, out string, err error) {
	t.Helper()
	shellOnly(t)
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("no sha256sum")
	}
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	staged = filepath.Join(bin, "aimem.new")
	script := "set -euo pipefail\nRELEASE=v0.0.1\nBASE=https://example.invalid/repo\n" +
		`fetch() { local src=${1##*/}; cp "$FAKE_RELEASE/$src" "$3"; }` + "\n" +
		extract(t, "install-hub.sh", "binary-stage") + `stage_binary "$1"` + "\n"
	cmd := exec.Command("bash", "-c", script, "stage", staged)
	cmd.Env = append(os.Environ(), "AIMEM_PREBUILT=", "AIMEM_PREBUILT_DIR=", "AIMEM_VERSION=")
	cmd.Env = append(cmd.Env, env...)
	b, err := cmd.CombinedOutput()
	return staged, string(b), err
}

// releaseDir writes both architectures' assets and, unless sums is
// false, a SHA256SUMS listing them; lie makes the sums wrong.
func releaseDir(t *testing.T, assets, sums, lie bool) string {
	t.Helper()
	dir := t.TempDir()
	var lines []string
	for _, a := range []string{"aimem-linux-amd64", "aimem-linux-arm64"} {
		body := []byte("#!/bin/sh\necho " + a + "\n")
		if assets {
			if err := os.WriteFile(filepath.Join(dir, a), body, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		h := sha256.Sum256(body)
		if lie {
			h = sha256.Sum256([]byte("something else"))
		}
		lines = append(lines, hex.EncodeToString(h[:])+"  "+a)
	}
	if sums {
		if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestHubStagesAVerifiedBinaryFromAPrebuiltDir(t *testing.T) {
	dir := releaseDir(t, true, true, false)
	staged, out, err := runStage(t, "AIMEM_PREBUILT_DIR="+dir, "AIMEM_VERSION=v9.9.9")
	if err != nil {
		t.Fatalf("stage: %v\n%s", err, out)
	}
	fi, serr := os.Stat(staged)
	if serr != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("no executable binary staged: %v\n%s", serr, out)
	}
	// Nothing names a release: not the pinned one, not AIMEM_VERSION, not
	// the forge. The version is the staged binary's own.
	if !strings.Contains(out, "installing aimem from "+dir) || !strings.Contains(out, "checksum OK: aimem-linux-") ||
		strings.Contains(out, "v0.0.1") || strings.Contains(out, "v9.9.9") || strings.Contains(out, "example.invalid") {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := os.Stat(staged + ".sums"); err == nil {
		t.Fatal("a sums file was left beside the binary")
	}
}

func TestHubPrebuiltDirRefusesWhatItCannotVerify(t *testing.T) {
	sumsOnly := releaseDir(t, false, true, false)
	noSums := releaseDir(t, true, false, false)
	noLine := releaseDir(t, true, true, false)
	if err := os.WriteFile(filepath.Join(noLine, "SHA256SUMS"), []byte("00  aimem-darwin-amd64\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, dir, want string
	}{
		{"missing binary", sumsOnly, "has no aimem-linux-"},
		{"missing SHA256SUMS", noSums, "has no SHA256SUMS; refusing unverified binary."},
		{"no line for the asset", noLine, "want absent"},
		{"mismatch", releaseDir(t, true, true, true), "checksum mismatch for aimem-linux-"},
	} {
		staged, out, err := runStage(t, "AIMEM_PREBUILT_DIR="+c.dir)
		if err == nil || !strings.Contains(out, "ERROR: ") || !strings.Contains(out, c.want) {
			t.Errorf("%s: err %v, want %q in:\n%s", c.name, err, c.want, out)
		}
		if _, serr := os.Stat(staged); serr == nil {
			t.Errorf("%s: a binary was staged", c.name)
		}
	}
}

func TestHubRefusesBothPrebuiltKnobs(t *testing.T) {
	dir := releaseDir(t, true, true, false)
	staged, out, err := runStage(t, "AIMEM_PREBUILT_DIR="+dir, "AIMEM_PREBUILT="+filepath.Join(dir, "aimem-linux-amd64"))
	if err == nil || !strings.Contains(out, "AIMEM_PREBUILT and AIMEM_PREBUILT_DIR are both set") {
		t.Fatalf("err %v:\n%s", err, out)
	}
	if _, serr := os.Stat(staged); serr == nil {
		t.Fatal("a binary was staged")
	}
}

// The download path runs the same verification: a release whose sums do
// not match is refused, one whose sums match is staged; AIMEM_PREBUILT
// still installs its file as given.
func TestHubDownloadAndPrebuiltPathsUnchanged(t *testing.T) {
	good := releaseDir(t, true, true, false)
	if staged, out, err := runStage(t, "FAKE_RELEASE="+good); err != nil || !strings.Contains(out, "installing aimem v0.0.1 (aimem-linux-") || !strings.Contains(out, "checksum OK") {
		t.Fatalf("download: %v\n%s", err, out)
	} else if _, err := os.Stat(staged + ".sums"); err == nil {
		t.Fatal("download: the sums file was left beside the binary")
	}
	if staged, out, err := runStage(t, "FAKE_RELEASE="+releaseDir(t, true, true, true)); err == nil || !strings.Contains(out, "checksum mismatch") {
		t.Fatalf("download with wrong sums: %v\n%s", err, out)
	} else if _, err := os.Stat(staged); err == nil {
		t.Fatal("download with wrong sums: a binary was staged")
	}
	if _, out, err := runStage(t, "FAKE_RELEASE="+releaseDir(t, true, false, false)); err == nil || !strings.Contains(out, "release v0.0.1 has no SHA256SUMS") {
		t.Fatalf("download without sums: %v\n%s", err, out)
	}
	own := filepath.Join(t.TempDir(), "mine")
	if err := os.WriteFile(own, []byte("unverified by design"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged, out, err := runStage(t, "AIMEM_PREBUILT="+own)
	if b, rerr := os.ReadFile(staged); err != nil || rerr != nil || string(b) != "unverified by design" {
		t.Fatalf("AIMEM_PREBUILT: %v %v\n%s", err, rerr, out)
	}
}
