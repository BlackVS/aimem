// Package upgrade brings an installed client to a release: `aimem
// upgrade`. It does what boot.sh and boot.ps1 do before they hand over to
// the installer, from inside the binary: it resolves the release, checks
// the platform binary against the release's SHA256SUMS, and fetches the
// tag's archive. Then it runs that archive's installer in its user-level
// mode with the verified binary, so the swap, the state backup, the
// health wait, the rollback and the hooks, plugin and service steps stay
// the installers' own (install.sh `user`, install.ps1 -UserOnly).
package upgrade

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Size bounds on what is downloaded: a release binary is about 20 MB and
// the repository archive a few MB.
const (
	maxSums    = 64 << 10
	maxBinary  = 256 << 20
	maxArchive = 128 << 20
	maxRelease = 1 << 20
)

// Source is where releases come from: the repository on the forge, as
// boot.sh names it (AIMEM_REPO, owner/name).
type Source struct {
	Repo   string       // owner/name
	Web    string       // https://github.com
	API    string       // https://api.github.com
	Client *http.Client // nil: a client with a timeout
}

// DefaultSource is the source the installers use: AIMEM_REPO or
// BlackVS/aimem on github.com.
func DefaultSource() Source {
	repo := os.Getenv("AIMEM_REPO")
	if repo == "" {
		repo = "BlackVS/aimem"
	}
	return Source{Repo: repo, Web: "https://github.com", API: "https://api.github.com"}
}

// Options is one upgrade.
type Options struct {
	Release string // the release named with --release; "" for the newest
	Current string // the running binary's version
	GOOS    string // "" for runtime.GOOS
	GOARCH  string // "" for runtime.GOARCH
	// Self is the running binary. It must be the one the installers put
	// in place, so a hub's binary (install-hub.sh) or a development build
	// is never swapped for a client install. "" skips the check.
	Self string
	Out  io.Writer
	// Install runs the extracted installer with the verified binary; nil
	// runs it as boot.sh and boot.ps1 do (RunInstaller).
	Install func(ctx context.Context, dir, binary, tag string, out io.Writer) error
}

// ErrRefused marks an upgrade refused before anything was changed.
var ErrRefused = errors.New("upgrade refused")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, a...))
}

// Run resolves, verifies and installs. A refusal (ErrRefused) leaves the
// installed client untouched; an installer failure is the installer's,
// which restores the previous binary itself.
func Run(ctx context.Context, src Source, o Options) error {
	if src.Client == nil {
		src.Client = &http.Client{Timeout: 5 * time.Minute}
	}
	goos, goarch := o.GOOS, o.GOARCH
	if goos == "" {
		goos = runtime.GOOS
	}
	if goarch == "" {
		goarch = runtime.GOARCH
	}
	install := o.Install
	if install == nil {
		install = RunInstaller
	}
	out := o.Out
	if out == nil {
		out = io.Discard
	}

	if o.Self != "" && !sameFile(o.Self, InstalledPath(goos)) {
		return refuse("this binary (%s) is not the client the installers put at %s; aimem upgrade upgrades that installed client (a hub upgrades with install-hub.sh)", o.Self, InstalledPath(goos))
	}

	tag := o.Release
	if tag == "" {
		latest, err := src.latest(ctx)
		if err != nil {
			return err
		}
		tag = latest
	}
	target, ok := parseVersion(tag)
	if !ok {
		return refuse("%q is not a release tag (vX.Y.Z)", tag)
	}
	cur, curOK := parseVersion(o.Current)
	switch {
	case curOK && cmpVersion(cur, target) == 0:
		fmt.Fprintf(out, "aimem %s is already installed; nothing to do\n", o.Current)
		return nil
	case curOK && cmpVersion(target, cur) < 0 && o.Release == "":
		return refuse("the newest release %s is older than the running %s; name a release with --release to install it", tag, o.Current)
	case !curOK && o.Release == "":
		return refuse("the running version %q is not a release; name one with --release", o.Current)
	}
	if curOK && cmpVersion(target, cur) < 0 {
		fmt.Fprintf(out, "downgrading aimem %s to %s, as --release names it\n", o.Current, tag)
	}

	asset := AssetName(goos, goarch)
	work, err := os.MkdirTemp("", "aimem-upgrade-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	fmt.Fprintf(out, "fetching aimem %s (%s) from %s\n", tag, asset, src.Repo)
	sums, err := src.get(ctx, src.download(tag, "SHA256SUMS"), maxSums)
	if err != nil {
		return refuse("release %s has no SHA256SUMS (%v); refusing an unverified binary", tag, err)
	}
	want, listed := parseSums(sums)[asset]
	if !listed {
		return refuse("release %s does not list %s in its SHA256SUMS", tag, asset)
	}
	bin, err := src.get(ctx, src.download(tag, asset), maxBinary)
	if err != nil {
		return refuse("release %s: %s: %v", tag, asset, err)
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != want {
		return refuse("checksum mismatch for %s (want %s, got %s)", asset, want, hex.EncodeToString(got[:]))
	}
	fmt.Fprintf(out, "checksum OK: %s\n", asset)
	name := "aimem-prebuilt"
	if goos == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(work, name)
	if err := os.WriteFile(binary, bin, 0o755); err != nil {
		return err
	}

	archive, err := src.get(ctx, src.Web+"/"+src.Repo+"/archive/refs/tags/"+tag+".tar.gz", maxArchive)
	if err != nil {
		return refuse("release %s: the source archive: %v", tag, err)
	}
	dir := filepath.Join(work, "src")
	if err := extract(archive, dir); err != nil {
		return refuse("release %s: the source archive: %v", tag, err)
	}
	if err := install(ctx, dir, binary, tag, out); err != nil {
		return fmt.Errorf("the installer of %s failed: %w", tag, err)
	}
	// The installer waited for the service at the new version, or rolled
	// back; the installed binary says which one came up.
	inst := InstalledPath(goos)
	v, err := exec.CommandContext(ctx, inst, "version").Output()
	if err != nil {
		return fmt.Errorf("the installer of %s finished, but %s does not answer: %w", tag, inst, err)
	}
	came := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(v)), "aimem"))
	if have, ok := parseVersion(came); !ok || cmpVersion(have, target) != 0 {
		return fmt.Errorf("the installer of %s finished, but %s reports %q", tag, inst, came)
	}
	fmt.Fprintf(out, "aimem %s is installed at %s\n", came, inst)
	return nil
}

func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	return err == nil && os.SameFile(fa, fb)
}

// InstalledPath is where the installers put the binary: install.sh's
// ${AIMEM_BIN_DIR:-$HOME/.local/bin}/aimem, install.ps1's
// %LOCALAPPDATA%\aimem\bin\aimem.exe.
func InstalledPath(goos string) string {
	if goos == "windows" {
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "aimem", "bin", "aimem.exe")
	}
	dir := os.Getenv("AIMEM_BIN_DIR")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "bin")
	}
	return filepath.Join(dir, "aimem")
}

// AssetName is the release asset for a platform, as release.yml names it.
func AssetName(goos, goarch string) string {
	n := "aimem-" + goos + "-" + goarch
	if goos == "windows" {
		n += ".exe"
	}
	return n
}

// RunInstaller runs the archive's installer in its user-level mode with
// the verified binary, as boot.sh and boot.ps1 hand it over: bash
// install.sh user, or install.ps1 -UserOnly in a child PowerShell whose
// execution policy is RemoteSigned for that process only.
func RunInstaller(ctx context.Context, dir, binary, tag string, out io.Writer) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-ExecutionPolicy", "RemoteSigned",
			"-File", filepath.Join(dir, "install.ps1"), "-UserOnly")
	} else {
		cmd = exec.CommandContext(ctx, "bash", filepath.Join(dir, "install.sh"), "user")
	}
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "AIMEM_PREBUILT="+binary, "AIMEM_TARGET_VERSION="+tag)
	cmd.Stdout, cmd.Stderr = out, out
	return cmd.Run()
}

func (s Source) download(tag, name string) string {
	return s.Web + "/" + s.Repo + "/releases/download/" + tag + "/" + name
}

// latest is the newest published release: GitHub's latest excludes
// drafts and prereleases.
func (s Source) latest(ctx context.Context) (string, error) {
	b, err := s.get(ctx, s.API+"/repos/"+s.Repo+"/releases/latest", maxRelease)
	if err != nil {
		return "", fmt.Errorf("could not find the newest release of %s: %w", s.Repo, err)
	}
	var r struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(b, &r); err != nil || r.Tag == "" || r.Draft || r.Prerelease {
		return "", fmt.Errorf("could not find the newest release of %s", s.Repo)
	}
	return r.Tag, nil
}

func (s Source) get(ctx context.Context, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s: larger than %d bytes", url, max)
	}
	return b, nil
}

// parseSums reads sha256sum output: "<hex>  <name>", the name possibly
// marked binary with "*".
func parseSums(b []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && len(f[0]) == 64 {
			out[strings.TrimPrefix(f[1], "*")] = strings.ToLower(f[0])
		}
	}
	return out
}

// extract unpacks a GitHub source archive into dir, dropping its single
// top-level directory as tar --strip-components=1 does. Only regular
// files and directories are written, and a name that would leave dir is
// refused.
func extract(archive []byte, dir string) error {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if unsafeName(h.Name) {
			return fmt.Errorf("unsafe path %q", h.Name)
		}
		_, rest, ok := strings.Cut(path.Clean(h.Name), "/")
		if !ok || rest == "" {
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(rest))
		if r, err := filepath.Rel(dir, dst); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe path %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			total += h.Size
			if total > 4*maxArchive {
				return errors.New("the archive unpacks larger than expected")
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o755|0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, io.LimitReader(tr, h.Size))
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
}

// unsafeName refuses what a source archive never holds: an absolute
// name, a ".." element, or a backslash or colon a Windows path would
// read as a separator or a drive.
func unsafeName(name string) bool {
	if path.IsAbs(name) || strings.ContainsAny(name, `\:`) {
		return true
	}
	for _, e := range strings.Split(name, "/") {
		if e == ".." {
			return true
		}
	}
	return false
}

var versionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)$`)

// parseVersion reads vX.Y.Z (the leading v optional); anything else, a
// development build included, is not a release.
func parseVersion(s string) ([3]int, bool) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

func cmpVersion(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}
