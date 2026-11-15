package upgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// TestMain doubles as the released binary: a copy named aimem answers
// `version` with STUB_VERSION, so the check after the installer sees the
// release that was installed.
func TestMain(m *testing.M) {
	base := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if base == "aimem" && len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("aimem", os.Getenv("STUB_VERSION"))
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The stub installers do what the real ones do with the handed-over
// binary in user mode: put it at the installed path. They refuse to run
// without the plugin where the real ones read it, record the release they
// were told, and fail when STUB_FAIL is set.
const stubInstallSh = `#!/bin/bash
set -e
[ "$1" = user ] || { echo "stub installer: wrong mode $1"; exit 9; }
[ "$(cat "$(dirname "$0")/.opencode/plugin/aimem.ts")" = "the plugin" ] || { echo "stub installer: no plugin beside it"; exit 8; }
if [ -n "$STUB_FAIL" ]; then echo "stub installer: the new release did not come up; rolled back"; exit 3; fi
mkdir -p "$AIMEM_BIN_DIR"
cp "$AIMEM_PREBUILT" "$AIMEM_BIN_DIR/aimem"
chmod 755 "$AIMEM_BIN_DIR/aimem"
printf '%s' "$AIMEM_TARGET_VERSION" > "$STUB_MARK"
`

const stubInstallPs1 = `param([switch]$UserOnly)
if (-not $UserOnly) { Write-Output 'stub installer: wrong mode'; exit 9 }
$plugin = Join-Path $PSScriptRoot '.opencode\plugin\aimem.ts'
if (-not (Test-Path -LiteralPath $plugin) -or (Get-Content -Raw -LiteralPath $plugin) -ne 'the plugin') { Write-Output 'stub installer: no plugin beside it'; exit 8 }
if ($env:STUB_FAIL) { Write-Output 'stub installer: the new release did not come up; rolled back'; exit 3 }
$bin = Join-Path $env:LOCALAPPDATA 'aimem\bin'
New-Item -ItemType Directory -Force $bin | Out-Null
Copy-Item -Force -LiteralPath $env:AIMEM_PREBUILT -Destination (Join-Path $bin 'aimem.exe')
[IO.File]::WriteAllText($env:STUB_MARK, $env:AIMEM_TARGET_VERSION)
`

// release is a stub https server: the latest-release answer and the files
// it serves by path. Hits counts every request, downloads the files
// served; redirect sends a path elsewhere.
type release struct {
	latest    string
	files     map[string][]byte // path under the web base -> body
	redirect  map[string]string // path under the web base -> location
	hits      atomic.Int32
	downloads atomic.Int32
}

func (r *release) ServeHTTP(w http.ResponseWriter, q *http.Request) {
	r.hits.Add(1)
	if q.URL.Path == "/api/repos/o/r/releases/latest" {
		fmt.Fprintf(w, `{"tag_name": %q, "draft": false, "prerelease": false}`, r.latest)
		return
	}
	p := strings.TrimPrefix(q.URL.Path, "/web")
	if to, ok := r.redirect[p]; ok {
		http.Redirect(w, q, to, http.StatusFound)
		return
	}
	b, ok := r.files[p]
	if !ok {
		http.NotFound(w, q)
		return
	}
	r.downloads.Add(1)
	w.Write(b)
}

// installer is the platform's installer asset.
func installer() string {
	if runtime.GOOS == "windows" {
		return "install.ps1"
	}
	return "install.sh"
}

// assets is a release's files: the binary (this test binary, which runs as
// the stub aimem), the installers, the plugin, and SHA256SUMS listing them.
func assets(t *testing.T) map[string][]byte {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		AssetName(runtime.GOOS, runtime.GOARCH): bin,
		"install.sh":                            []byte(stubInstallSh),
		"install.ps1":                           []byte(stubInstallPs1),
		pluginAsset:                             []byte("the plugin"),
	}
	var sums strings.Builder
	for n, b := range files {
		sum := sha256.Sum256(b)
		sums.WriteString(hex.EncodeToString(sum[:]) + "  " + n + "\n")
	}
	sums.WriteString(strings.Repeat("0", 64) + "  LICENSE\n")
	files["SHA256SUMS"] = []byte(sums.String())
	return files
}

func releaseDir(tag string) string { return "/o/r/releases/download/" + tag + "/" }

// publish adds a release to the forge and returns its binary.
func (r *release) publish(t *testing.T, tag string) []byte {
	t.Helper()
	files := assets(t)
	for n, b := range files {
		r.files[releaseDir(tag)+n] = b
	}
	return files[AssetName(runtime.GOOS, runtime.GOARCH)]
}

// sandbox is an installed client: the installed binary at the path the
// installers use, under a temporary home.
type sandbox struct {
	installed, mark string
	before          []byte
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("AIMEM_BIN_DIR", filepath.Join(home, ".local", "bin"))
	t.Setenv("STUB_FAIL", "")
	s := &sandbox{installed: InstalledPath(runtime.GOOS), mark: filepath.Join(home, "installer-ran"), before: []byte("the installed v0.10.17 binary")}
	t.Setenv("STUB_MARK", s.mark)
	if err := os.MkdirAll(filepath.Dir(s.installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.installed, s.before, 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

// untouched asserts a refusal changed nothing: the installed binary is
// byte-identical and the installer never ran.
func (s *sandbox) untouched(t *testing.T) {
	t.Helper()
	if b, _ := os.ReadFile(s.installed); !bytes.Equal(b, s.before) {
		t.Error("the installed binary changed")
	}
	if _, err := os.Stat(s.mark); err == nil {
		t.Error("the installer ran")
	}
}

func setup(t *testing.T) (*release, Source, *sandbox) {
	t.Helper()
	r := &release{latest: "v0.10.18", files: map[string][]byte{}, redirect: map[string]string{}}
	srv := httptest.NewTLSServer(r)
	t.Cleanup(srv.Close)
	return r, Source{Repo: "o/r", Web: srv.URL + "/web", API: srv.URL + "/api", Client: srv.Client()}, newSandbox(t)
}

func run(src Source, o Options) (string, error) {
	var out bytes.Buffer
	o.Out = &out
	err := Run(context.Background(), src, o)
	return out.String(), err
}

// The newest release, and a named one, are verified and installed by the
// release's own installer asset, which is told the release; the installed
// binary then reports it. Only release assets are fetched.
func TestUpgradeInstallsTheNewestOrNamedRelease(t *testing.T) {
	for _, c := range []struct{ name, release, want string }{
		{"newest", "", "v0.10.18"},
		{"named", "v0.10.19", "v0.10.19"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, src, s := setup(t)
			bin := r.publish(t, "v0.10.18")
			r.publish(t, "v0.10.19")
			t.Setenv("STUB_VERSION", c.want)
			out, err := run(src, Options{Release: c.release, Current: "v0.10.17"})
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if got, _ := os.ReadFile(s.installed); !bytes.Equal(got, bin) {
				t.Error("the installed binary is not the release's")
			}
			if m, _ := os.ReadFile(s.mark); string(m) != c.want {
				t.Errorf("the installer was told %q, want %q", m, c.want)
			}
			for _, w := range []string{
				"checksum OK: " + AssetName(runtime.GOOS, runtime.GOARCH),
				"checksum OK: " + installer(),
				"checksum OK: " + pluginAsset,
				"aimem " + c.want + " is installed at " + s.installed,
			} {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			// SHA256SUMS, the binary, the installer and the plugin.
			if n := r.downloads.Load(); n != 4 {
				t.Errorf("%d files fetched, want 4", n)
			}
		})
	}
}

// The release that is already running: said so, nothing fetched, nothing
// changed.
func TestUpgradeOnTheSameReleaseChangesNothing(t *testing.T) {
	r, src, s := setup(t)
	r.publish(t, "v0.10.18")
	out, err := run(src, Options{Current: "v0.10.18"})
	if err != nil || !strings.Contains(out, "aimem v0.10.18 is already installed; nothing to do") {
		t.Fatalf("%v\n%s", err, out)
	}
	if n := r.downloads.Load(); n != 0 {
		t.Errorf("%d release files fetched", n)
	}
	s.untouched(t)
}

// An older release is installed only when --release names it.
func TestUpgradeRefusesADowngradeUnlessNamed(t *testing.T) {
	r, src, s := setup(t)
	r.publish(t, "v0.10.18")
	_, err := run(src, Options{Current: "v0.10.20"})
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "name a release with --release") {
		t.Fatalf("unnamed downgrade: %v", err)
	}
	s.untouched(t)

	t.Setenv("STUB_VERSION", "v0.10.18")
	out, err := run(src, Options{Current: "v0.10.20", Release: "v0.10.18"})
	if err != nil || !strings.Contains(out, "downgrading aimem v0.10.20 to v0.10.18, as --release names it") {
		t.Fatalf("named downgrade: %v\n%s", err, out)
	}
}

// A missing sums file, and for each of the binary, the installer and the
// plugin a missing sums line, a missing file and a mismatch, each refuse
// before anything is run or touched.
func TestUpgradeRefusesUnverifiedFiles(t *testing.T) {
	pre := releaseDir("v0.10.18")
	type brk struct {
		name  string
		spoil func(r *release)
		want  string
	}
	cases := []brk{{"no sums", func(r *release) { delete(r.files, pre+"SHA256SUMS") }, "has no SHA256SUMS"}}
	for _, n := range []string{AssetName(runtime.GOOS, runtime.GOARCH), installer(), pluginAsset} {
		n := n
		cases = append(cases,
			brk{"tampered " + n, func(r *release) { r.files[pre+n] = append(append([]byte{}, r.files[pre+n]...), 'x') }, "checksum mismatch for " + n},
			brk{"no file " + n, func(r *release) { delete(r.files, pre+n) }, "release v0.10.18: " + n},
		)
		if n != installer() { // an unlisted installer is the pre-asset refusal
			cases = append(cases, brk{"unlisted " + n, func(r *release) { r.files[pre+"SHA256SUMS"] = dropLine(r.files[pre+"SHA256SUMS"], n) }, "does not list " + n})
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, src, s := setup(t)
			r.publish(t, "v0.10.18")
			c.spoil(r)
			_, err := run(src, Options{Current: "v0.10.17"})
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a refusal naming %q, got %v", c.want, err)
			}
			s.untouched(t)
		})
	}
}

func dropLine(sums []byte, name string) []byte {
	var keep []string
	for _, l := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[1] == name {
			continue
		}
		keep = append(keep, l)
	}
	return []byte(strings.Join(keep, "\n"))
}

// A release whose sums list no installer predates the installer assets:
// refused with the release's own one-liner, decided by the sums and not by
// the version, and nothing but SHA256SUMS is fetched. No source archive is
// ever asked for.
func TestUpgradeRefusesAReleaseWithoutInstallerAssets(t *testing.T) {
	r, src, s := setup(t)
	r.publish(t, "v0.10.30")
	pre := releaseDir("v0.10.30")
	r.files[pre+"SHA256SUMS"] = dropLine(dropLine(r.files[pre+"SHA256SUMS"], "install.sh"), "install.ps1")
	_, err := run(src, Options{Current: "v0.10.17", Release: "v0.10.30"})
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("%v", err)
	}
	for _, w := range []string{"publishes no installer files as assets", "v0.10.23 is the first release that does", oneLiner("o/r", "v0.10.30", runtime.GOOS)} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("the refusal lacks %q: %v", w, err)
		}
	}
	if n := r.downloads.Load(); n != 1 {
		t.Errorf("%d files fetched, want only SHA256SUMS", n)
	}
	if n := r.hits.Load(); n != 1 {
		t.Errorf("%d requests, want only SHA256SUMS", n)
	}
	s.untouched(t)
}

// The installer's failure (its rollback included) is the command's
// failure, with the installer's own words in the output.
func TestUpgradeReportsAFailingInstaller(t *testing.T) {
	r, src, s := setup(t)
	r.publish(t, "v0.10.18")
	t.Setenv("STUB_FAIL", "1")
	out, err := run(src, Options{Current: "v0.10.17"})
	if err == nil || errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "the installer of v0.10.18 failed") {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, "stub installer: the new release did not come up; rolled back") {
		t.Errorf("the installer's output is missing:\n%s", out)
	}
	if b, _ := os.ReadFile(s.installed); !bytes.Equal(b, s.before) {
		t.Error("the installed binary changed")
	}
}

// An installer that finishes while the installed binary reports another
// release is not a success.
func TestUpgradeChecksTheVersionThatCameUp(t *testing.T) {
	r, src, _ := setup(t)
	r.publish(t, "v0.10.18")
	t.Setenv("STUB_VERSION", "v0.10.17")
	_, err := run(src, Options{Current: "v0.10.17"})
	if err == nil || !strings.Contains(err.Error(), `reports "v0.10.17"`) {
		t.Fatalf("%v", err)
	}
}

// A development build, and a binary other than the installed client (a
// hub's, say), are refused before anything is fetched.
func TestUpgradeRefusesWhatIsNotAnInstalledRelease(t *testing.T) {
	r, src, s := setup(t)
	r.publish(t, "v0.10.18")
	if _, err := run(src, Options{Current: "dev"}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "is not a release") {
		t.Fatalf("dev build: %v", err)
	}
	other := filepath.Join(t.TempDir(), "aimem")
	os.WriteFile(other, []byte("a hub's binary"), 0o755)
	if _, err := run(src, Options{Current: "v0.10.17", Self: other}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "is not the client the installers put at") {
		t.Fatalf("other binary: %v", err)
	}
	if n := r.downloads.Load(); n != 0 {
		t.Errorf("%d release files fetched", n)
	}
	s.untouched(t)

	t.Setenv("STUB_VERSION", "v0.10.18")
	if out, err := run(src, Options{Current: "v0.10.17", Self: s.installed}); err != nil {
		t.Fatalf("the installed binary itself: %v\n%s", err, out)
	}
}

// A hub's installation is refused before anything is fetched, by any of
// the markers only install-hub.sh leaves: its binary sits at the client's
// path, and install.sh user would replace its service with a client's.
func TestUpgradeRefusesAHub(t *testing.T) {
	for _, c := range []struct{ name, file, body string }{
		{"curation script", ".local/sbin/curate-all.sh", "#!/bin/sh\n"},
		{"curation unit", ".config/systemd/user/aimem-curate.service", "[Unit]\n"},
		{"listen address", ".config/aimem/env", "AIMEM_HTTP_TOKEN=x\nAIMEM_HTTP_LISTEN=:8440\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, src, s := setup(t)
			r.publish(t, "v0.10.18")
			p := filepath.Join(os.Getenv("HOME"), filepath.FromSlash(c.file))
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(c.body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := run(src, Options{Current: "v0.10.17", GOOS: "linux", GOARCH: "amd64"})
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "upgrade a hub with install-hub.sh") {
				t.Fatalf("%v", err)
			}
			if n := r.downloads.Load(); n != 0 {
				t.Errorf("%d release files fetched", n)
			}
			s.untouched(t)
		})
	}
	// A client's env file without a listen address is not a hub's.
	r, src, _ := setup(t)
	r.publish(t, "v0.10.18")
	p := filepath.Join(os.Getenv("HOME"), ".config", "aimem", "env")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("AIMEM_HUB_URL=https://hub.invalid\n# AIMEM_HTTP_LISTEN=:8440\n"), 0o644)
	t.Setenv("STUB_VERSION", "v0.10.18")
	if out, err := run(src, Options{Current: "v0.10.17"}); err != nil {
		t.Fatalf("a client's env file: %v\n%s", err, out)
	}
}

// A release named without its v is the same release.
func TestUpgradeAcceptsAReleaseWithoutItsV(t *testing.T) {
	r, src, _ := setup(t)
	r.publish(t, "v0.10.19")
	t.Setenv("STUB_VERSION", "v0.10.19")
	if out, err := run(src, Options{Current: "v0.10.17", Release: "0.10.19"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

// exportFiles is an aiforge export (format 1) holding the given releases,
// by path relative to the base, with MANIFEST naming one.
func exportFiles(t *testing.T, manifest string, tags ...string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{"MANIFEST": []byte(manifest)}
	for _, tag := range tags {
		for n, b := range assets(t) {
			files["aimem/"+tag+"/"+n] = b
		}
	}
	return files
}

// writeExport puts an export in a directory and returns the directory.
func writeExport(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for rel, b := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// serveExport serves an export over https and returns its base URL and the
// server's record of requests.
func serveExport(t *testing.T, files map[string][]byte) (string, *release) {
	t.Helper()
	e := &release{files: map[string][]byte{}, redirect: map[string]string{}}
	for rel, b := range files {
		e.files["/exp/"+rel] = b
	}
	srv := httptest.NewTLSServer(e)
	t.Cleanup(srv.Close)
	return srv.URL + "/web/exp", e
}

// With AIMEM_BASE, as a directory or as an https URL, the release comes
// from MANIFEST and its files from <base>/aimem/<tag>/, and the forge is
// never contacted. --release wins over MANIFEST.
func TestUpgradeFromAnExport(t *testing.T) {
	const manifest = "aiforge-export 1\naiskills v1.39.0\naimem v0.10.18\n"
	for _, form := range []string{"directory", "https"} {
		for _, c := range []struct{ name, release, want string }{
			{"manifest", "", "v0.10.18"},
			{"named", "v0.10.19", "v0.10.19"},
		} {
			t.Run(form+"/"+c.name, func(t *testing.T) {
				forge, src, s := setup(t)
				files := exportFiles(t, manifest, "v0.10.18", "v0.10.19")
				var served *release
				if form == "directory" {
					src.Base = writeExport(t, files) + string(filepath.Separator)
				} else {
					src.Base, served = serveExport(t, files)
					src.Base += "/"
				}
				t.Setenv("STUB_VERSION", c.want)
				out, err := run(src, Options{Release: c.release, Current: "v0.10.17"})
				if err != nil {
					t.Fatalf("%v\n%s", err, out)
				}
				if m, _ := os.ReadFile(s.mark); string(m) != c.want {
					t.Errorf("the installer was told %q, want %q", m, c.want)
				}
				if !strings.Contains(out, "aimem/"+c.want) && !strings.Contains(out, filepath.Join("aimem", c.want)) {
					t.Errorf("output does not name the export's release directory:\n%s", out)
				}
				if n := forge.hits.Load(); n != 0 {
					t.Errorf("the forge was contacted %d times", n)
				}
				if served != nil {
					want := int32(4)
					if c.release == "" {
						want = 5 // and MANIFEST
					}
					if n := served.downloads.Load(); n != want {
						t.Errorf("%d files fetched from the export, want %d", n, want)
					}
				}
			})
		}
	}
}

// An export holds only releases that publish the installer files, so a
// file its copy of the release lacks, unlisted in SHA256SUMS or absent,
// is reported as an incomplete export, and the forge's one-liner for an
// older release is never offered.
func TestUpgradeReportsAnIncompleteExport(t *testing.T) {
	installer := "install.sh"
	if runtime.GOOS == "windows" {
		installer = "install.ps1"
	}
	const manifest = "aiforge-export 1\naimem v0.10.18\n"
	dir := "aimem/v0.10.18/"
	for _, c := range []struct {
		name  string
		spoil func(files map[string][]byte)
		want  string
	}{
		{"installer unlisted", func(f map[string][]byte) { f[dir+"SHA256SUMS"] = dropLine(f[dir+"SHA256SUMS"], installer) }, "its SHA256SUMS does not list " + installer},
		{"plugin unlisted", func(f map[string][]byte) { f[dir+"SHA256SUMS"] = dropLine(f[dir+"SHA256SUMS"], pluginAsset) }, "its SHA256SUMS does not list " + pluginAsset},
		{"installer absent", func(f map[string][]byte) { delete(f, dir+installer) }, "(" + installer + ": "},
	} {
		for _, form := range []string{"directory", "https"} {
			t.Run(c.name+"/"+form, func(t *testing.T) {
				forge, src, s := setup(t)
				files := exportFiles(t, manifest, "v0.10.18")
				c.spoil(files)
				if form == "directory" {
					src.Base = writeExport(t, files)
				} else {
					src.Base, _ = serveExport(t, files)
				}
				_, err := run(src, Options{Current: "v0.10.17"})
				if !errors.Is(err, ErrRefused) {
					t.Fatalf("%v", err)
				}
				for _, w := range []string{"the export at " + src.Base + " holds an incomplete copy of release v0.10.18", c.want, "export the release again"} {
					if !strings.Contains(err.Error(), w) {
						t.Errorf("the refusal lacks %q: %v", w, err)
					}
				}
				for _, w := range []string{"one-liner", "raw.githubusercontent.com", "v0.10.23 is the first"} {
					if strings.Contains(err.Error(), w) {
						t.Errorf("the refusal names %q: %v", w, err)
					}
				}
				if n := forge.hits.Load(); n != 0 {
					t.Errorf("the forge was contacted %d times", n)
				}
				s.untouched(t)
			})
		}
	}
}

// An export that cannot name or hold the release is refused before
// anything is run, and so is a base that is neither an absolute directory
// nor https.
func TestUpgradeRefusesABadExport(t *testing.T) {
	for _, c := range []struct {
		name     string
		manifest string // "" for none
		release  string
		want     string
	}{
		{"no manifest", "", "", "MANIFEST is missing"},
		{"wrong format", "aiforge-export 2\naimem v0.10.18\n", "", `is "aiforge-export 2", not a format aimem upgrade reads (aiforge-export 1)`},
		{"no aimem line", "aiforge-export 1\naiskills v1.39.0\n", "", "names no aimem release"},
		{"tag not exported", "aiforge-export 1\naimem v0.10.30\n", "", "release v0.10.30 has no SHA256SUMS"},
		{"named tag not exported", "aiforge-export 1\naimem v0.10.18\n", "v0.10.31", "release v0.10.31 has no SHA256SUMS"},
	} {
		for _, form := range []string{"directory", "https"} {
			t.Run(c.name+"/"+form, func(t *testing.T) {
				forge, src, s := setup(t)
				files := exportFiles(t, c.manifest, "v0.10.18")
				if c.manifest == "" {
					delete(files, "MANIFEST")
				}
				if form == "directory" {
					src.Base = writeExport(t, files)
				} else {
					src.Base, _ = serveExport(t, files)
				}
				_, err := run(src, Options{Release: c.release, Current: "v0.10.17"})
				if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), c.want) {
					t.Fatalf("want a refusal naming %q, got %v", c.want, err)
				}
				if n := forge.hits.Load(); n != 0 {
					t.Errorf("the forge was contacted %d times", n)
				}
				s.untouched(t)
			})
		}
	}

	// A filesystem root is a directory base like any other, never an
	// empty one that would fall back to the forge.
	t.Run("root directory", func(t *testing.T) {
		forge, src, s := setup(t)
		src.Base = filepath.VolumeName(os.TempDir()) + string(filepath.Separator)
		_, err := run(src, Options{Current: "v0.10.17"})
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), filepath.Join(src.Base, "MANIFEST")+" is missing") {
			t.Fatalf("%v", err)
		}
		if n := forge.hits.Load(); n != 0 {
			t.Errorf("the forge was contacted %d times", n)
		}
		s.untouched(t)
	})

	plain := &release{files: map[string][]byte{}}
	psrv := httptest.NewServer(plain)
	t.Cleanup(psrv.Close)
	for _, base := range []string{psrv.URL + "/exp", "relative/export", "file:///export", "https://"} {
		t.Run("base "+base, func(t *testing.T) {
			forge, src, s := setup(t)
			src.Base = base
			_, err := run(src, Options{Current: "v0.10.17"})
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "AIMEM_BASE must be an absolute directory or an https:// URL") {
				t.Fatalf("%v", err)
			}
			if n := forge.hits.Load() + plain.hits.Load(); n != 0 {
				t.Errorf("%d requests", n)
			}
			s.untouched(t)
		})
	}
}

// No plain http source is read: neither a redirect hop to http, from the
// forge or from an export, nor a forge URL that is http itself.
func TestUpgradeRefusesPlainHTTP(t *testing.T) {
	plain := &release{files: map[string][]byte{}}
	psrv := httptest.NewServer(plain)
	t.Cleanup(psrv.Close)

	t.Run("redirect from the forge", func(t *testing.T) {
		r, src, s := setup(t)
		r.publish(t, "v0.10.18")
		r.redirect[releaseDir("v0.10.18")+"SHA256SUMS"] = psrv.URL + "/sums"
		_, err := run(src, Options{Current: "v0.10.17"})
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not https") {
			t.Fatalf("%v", err)
		}
		s.untouched(t)
	})
	t.Run("redirect from an export", func(t *testing.T) {
		_, src, s := setup(t)
		base, e := serveExport(t, exportFiles(t, "aiforge-export 1\naimem v0.10.18\n", "v0.10.18"))
		e.redirect["/exp/aimem/v0.10.18/"+installer()] = psrv.URL + "/installer"
		src.Base = base
		_, err := run(src, Options{Current: "v0.10.17"})
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "not https") {
			t.Fatalf("%v", err)
		}
		s.untouched(t)
	})
	t.Run("http forge", func(t *testing.T) {
		_, src, s := setup(t)
		src.Web = psrv.URL + "/web"
		_, err := run(src, Options{Current: "v0.10.17", Release: "v0.10.18"})
		if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "a source is an https:// URL or an absolute directory") {
			t.Fatalf("%v", err)
		}
		s.untouched(t)
	})
	if n := plain.hits.Load(); n != 0 {
		t.Errorf("the plain http server got %d requests", n)
	}
}
