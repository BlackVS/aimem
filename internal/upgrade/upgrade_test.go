package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
// binary in user mode: put it at the installed path. They record the
// release they were told, and fail when STUB_FAIL is set.
const stubInstallSh = `#!/bin/bash
set -e
[ "$1" = user ] || { echo "stub installer: wrong mode $1"; exit 9; }
if [ -n "$STUB_FAIL" ]; then echo "stub installer: the new release did not come up; rolled back"; exit 3; fi
mkdir -p "$AIMEM_BIN_DIR"
cp "$AIMEM_PREBUILT" "$AIMEM_BIN_DIR/aimem"
chmod 755 "$AIMEM_BIN_DIR/aimem"
printf '%s' "$AIMEM_TARGET_VERSION" > "$STUB_MARK"
`

const stubInstallPs1 = `param([switch]$UserOnly)
if (-not $UserOnly) { Write-Output 'stub installer: wrong mode'; exit 9 }
if ($env:STUB_FAIL) { Write-Output 'stub installer: the new release did not come up; rolled back'; exit 3 }
$bin = Join-Path $env:LOCALAPPDATA 'aimem\bin'
New-Item -ItemType Directory -Force $bin | Out-Null
Copy-Item -Force -LiteralPath $env:AIMEM_PREBUILT -Destination (Join-Path $bin 'aimem.exe')
[IO.File]::WriteAllText($env:STUB_MARK, $env:AIMEM_TARGET_VERSION)
`

// release is a stub forge: the latest-release answer and, per tag, the
// files a release serves. Downloads counts the release-file requests.
type release struct {
	latest    string
	files     map[string][]byte // path under the web base -> body
	downloads atomic.Int32
}

func (r *release) ServeHTTP(w http.ResponseWriter, q *http.Request) {
	if q.URL.Path == "/api/repos/o/r/releases/latest" {
		fmt.Fprintf(w, `{"tag_name": %q, "draft": false, "prerelease": false}`, r.latest)
		return
	}
	b, ok := r.files[strings.TrimPrefix(q.URL.Path, "/web")]
	if !ok {
		http.NotFound(w, q)
		return
	}
	r.downloads.Add(1)
	w.Write(b)
}

// publish adds a release: the binary (this test binary, which runs as
// the stub aimem), SHA256SUMS listing it, and the source archive with the
// stub installers.
func (r *release) publish(t *testing.T, tag string) []byte {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	asset := AssetName(runtime.GOOS, runtime.GOARCH)
	sum := sha256.Sum256(bin)
	pre := "/o/r/releases/download/" + tag + "/"
	r.files[pre+asset] = bin
	r.files[pre+"SHA256SUMS"] = []byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n" +
		strings.Repeat("0", 64) + "  LICENSE\n")
	r.files["/o/r/archive/refs/tags/"+tag+".tar.gz"] = archive(t, map[string]string{
		"install.sh": stubInstallSh, "install.ps1": stubInstallPs1,
	})
	return bin
}

func archive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	tw.WriteHeader(&tar.Header{Name: "aimem-x/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "aimem-x/" + name, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	zw.Close()
	return buf.Bytes()
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
	r := &release{latest: "v0.10.18", files: map[string][]byte{}}
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
// release's own installer, which is told the release; the installed
// binary then reports it.
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
			for _, w := range []string{"checksum OK: " + AssetName(runtime.GOOS, runtime.GOARCH), "aimem " + c.want + " is installed at " + s.installed} {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
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

// A missing sums file, an unlisted asset and a mismatch each refuse
// before anything is touched.
func TestUpgradeRefusesAnUnverifiedBinary(t *testing.T) {
	asset := AssetName(runtime.GOOS, runtime.GOARCH)
	pre := "/o/r/releases/download/v0.10.18/"
	for _, c := range []struct {
		name   string
		break_ func(r *release)
		want   string
	}{
		{"no sums", func(r *release) { delete(r.files, pre+"SHA256SUMS") }, "has no SHA256SUMS"},
		{"unlisted", func(r *release) {
			r.files[pre+"SHA256SUMS"] = []byte(strings.Repeat("0", 64) + "  LICENSE\n")
		}, "does not list " + asset},
		{"tampered", func(r *release) { r.files[pre+asset] = append(append([]byte{}, r.files[pre+asset]...), 'x') }, "checksum mismatch for " + asset},
		{"no binary", func(r *release) { delete(r.files, pre+asset) }, asset},
		{"no archive", func(r *release) { delete(r.files, "/o/r/archive/refs/tags/v0.10.18.tar.gz") }, "the source archive"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, src, s := setup(t)
			r.publish(t, "v0.10.18")
			c.break_(r)
			_, err := run(src, Options{Current: "v0.10.17"})
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a refusal naming %q, got %v", c.want, err)
			}
			s.untouched(t)
		})
	}
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

// An archive entry that would leave the extraction directory is refused.
func TestExtractRefusesAnEscape(t *testing.T) {
	for _, name := range []string{"aimem-x/../../evil", "aimem-x/../evil", "/abs/evil", `aimem-x/..\..\evil`, "aimem-x/C:evil"} {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: 1})
		tw.Write([]byte("x"))
		tw.Close()
		zw.Close()
		dir := t.TempDir()
		if err := extract(buf.Bytes(), filepath.Join(dir, "src")); err == nil {
			t.Errorf("%s: extracted", name)
		}
		if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
			t.Errorf("%s: wrote outside", name)
		}
	}
}
