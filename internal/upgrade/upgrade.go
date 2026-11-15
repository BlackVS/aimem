// Package upgrade brings an installed client to a release: `aimem
// upgrade`. It does what boot.sh and boot.ps1 do before they hand over to
// the installer, from inside the binary: it resolves the release, checks
// the platform binary, the installer (install.sh or install.ps1) and the
// OpenCode plugin against the release's SHA256SUMS, and lays them out as
// the boot scripts do. Then it runs that installer in its user-level mode
// with the verified binary, so the swap, the state backup, the health
// wait, the rollback and the hooks, plugin and service steps stay the
// installers' own (install.sh `user`, install.ps1 -UserOnly).
//
// The files come from the release on the forge or, with AIMEM_BASE, from
// an aiforge export in format 1: <base>/MANIFEST names the release, and
// <base>/aimem/<tag>/ holds its assets with its SHA256SUMS. A source is
// https or a local directory; plain http is refused, on every redirect hop
// too.
package upgrade

import (
	"bufio"
	"bytes"
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
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Size bounds on what is read: a release binary is about 20 MB, an
// installer or the plugin some tens of KB.
const (
	maxSums     = 64 << 10
	maxBinary   = 256 << 20
	maxScript   = 4 << 20
	maxRelease  = 1 << 20
	maxManifest = 64 << 10
)

// pluginAsset is the OpenCode plugin's release asset. The installers read
// it at .opencode/plugin/aimem.ts beside themselves, where a checkout has
// it.
const pluginAsset = "aimem-opencode.ts"

// exportFormat is the first line of an aiforge export's MANIFEST.
const exportFormat = "aiforge-export 1"

// Source is where releases come from: the repository on the forge, as
// boot.sh names it (AIMEM_REPO, owner/name), or an aiforge export.
type Source struct {
	Repo string // owner/name
	Web  string // https://github.com
	API  string // https://api.github.com
	// Base is AIMEM_BASE: an aiforge export, as an absolute directory or
	// an https:// URL. With it, Web and API are never contacted.
	Base   string
	Client *http.Client // nil: a client with a timeout
}

// DefaultSource is the source the installers use: AIMEM_BASE when it is
// set, and AIMEM_REPO or BlackVS/aimem on github.com.
func DefaultSource() Source {
	repo := os.Getenv("AIMEM_REPO")
	if repo == "" {
		repo = "BlackVS/aimem"
	}
	return Source{Repo: repo, Web: "https://github.com", API: "https://api.github.com", Base: os.Getenv("AIMEM_BASE")}
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
	// Install runs the laid-out installer with the verified binary; nil
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
	client := &http.Client{Timeout: 5 * time.Minute}
	if src.Client != nil {
		c := *src.Client
		client = &c
	}
	client.CheckRedirect = httpsOnly
	src.Client = client
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

	if goos != "windows" {
		if m := hubMarker(); m != "" {
			return refuse("this is a hub's installation (%s): aimem upgrade would replace its service with a client's; upgrade a hub with install-hub.sh", m)
		}
	}

	// A directory base keeps its form (filepath.Join cleans it), so a
	// filesystem root stays a root.
	if strings.HasPrefix(src.Base, "https://") && len(src.Base) > len("https://") {
		src.Base = strings.TrimRight(src.Base, "/")
	} else if src.Base != "" && !filepath.IsAbs(src.Base) {
		return refuse("AIMEM_BASE must be an absolute directory or an https:// URL, not %q", src.Base)
	}

	tag := o.Release
	if tag != "" && !strings.HasPrefix(tag, "v") {
		tag = "v" + tag // release tags are vX.Y.Z
	}
	newest := "the newest release"
	if tag == "" {
		var err error
		if src.Base != "" {
			newest = "the export's release"
			tag, err = src.manifest(ctx)
		} else {
			tag, err = src.latest(ctx)
		}
		if err != nil {
			return err
		}
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
		return refuse("%s %s is older than the running %s; name a release with --release to install it", newest, tag, o.Current)
	case !curOK && o.Release == "":
		return refuse("the running version %q is not a release; name one with --release", o.Current)
	}
	if curOK && cmpVersion(target, cur) < 0 {
		fmt.Fprintf(out, "downgrading aimem %s to %s, as --release names it\n", o.Current, tag)
	}

	asset := AssetName(goos, goarch)
	installer := "install.sh"
	if goos == "windows" {
		installer = "install.ps1"
	}
	work, err := os.MkdirTemp("", "aimem-upgrade-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	fmt.Fprintf(out, "fetching aimem %s (%s) from %s\n", tag, asset, src.release(tag))
	b, err := src.get(ctx, src.file(tag, "SHA256SUMS"), maxSums)
	if err != nil {
		return refuse("release %s has no SHA256SUMS (%v); refusing unverified files", tag, err)
	}
	sums := parseSums(b)
	if _, ok := sums[installer]; !ok && src.Base == "" {
		return refuse("release %s publishes no installer files as assets (its SHA256SUMS lists no %s); v0.10.23 is the first release that does. Install %s with its own one-liner: %s",
			tag, installer, tag, oneLiner(src.Repo, tag, goos))
	}
	// An export holds only releases that publish the installer files, so
	// with a base a file the release lacks means the export is incomplete,
	// and no one-liner of the forge is offered.
	missing := func(what string) error {
		if src.Base != "" {
			return refuse("the export at %s holds an incomplete copy of release %s (%s); export the release again", src.Base, tag, what)
		}
		return refuse("release %s: %s", tag, what)
	}
	for _, n := range []string{installer, asset, pluginAsset} {
		if _, ok := sums[n]; !ok {
			return missing("its SHA256SUMS does not list " + n)
		}
	}
	verified := func(name string, max int64) ([]byte, error) {
		b, err := src.get(ctx, src.file(tag, name), max)
		if err != nil {
			return nil, missing(fmt.Sprintf("%s: %v", name, err))
		}
		got := sha256.Sum256(b)
		if hex.EncodeToString(got[:]) != sums[name] {
			return nil, refuse("checksum mismatch for %s (want %s, got %s)", name, sums[name], hex.EncodeToString(got[:]))
		}
		fmt.Fprintf(out, "checksum OK: %s\n", name)
		return b, nil
	}
	bin, err := verified(asset, maxBinary)
	if err != nil {
		return err
	}
	script, err := verified(installer, maxScript)
	if err != nil {
		return err
	}
	plugin, err := verified(pluginAsset, maxScript)
	if err != nil {
		return err
	}

	// The layout the boot scripts give the installer: the installer, and
	// the plugin where a checkout has it.
	name := "aimem-prebuilt"
	if goos == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(work, name)
	dir := filepath.Join(work, "src")
	pluginDir := filepath.Join(dir, ".opencode", "plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		return err
	}
	for _, f := range []struct {
		path string
		body []byte
	}{
		{binary, bin},
		{filepath.Join(dir, installer), script},
		{filepath.Join(pluginDir, "aimem.ts"), plugin},
	} {
		if err := os.WriteFile(f.path, f.body, 0o755); err != nil {
			return err
		}
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

// httpsOnly keeps every redirect hop on TLS, within the default limit of
// ten hops.
func httpsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing a redirect to %s: not https", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	return nil
}

// oneLiner is a release's own install line, its boot script fetched from
// the repository at its tag, as boot.sh hands an older release over.
func oneLiner(repo, tag, goos string) string {
	raw := "https://raw.githubusercontent.com/" + repo + "/" + tag
	if goos == "windows" {
		return "irm " + raw + "/boot.ps1 | iex"
	}
	return "curl -fsSL " + raw + "/boot.sh | bash"
}

// hubMarker names what only install-hub.sh leaves in the service user's
// home: the curation script (written even without units), the curation
// unit, or a listen address in the env file. A hub's binary sits at the
// client's installed path, so the path alone does not tell them apart.
func hubMarker() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, p := range []string{
		filepath.Join(home, ".local", "sbin", "curate-all.sh"),
		filepath.Join(home, ".config", "systemd", "user", "aimem-curate.service"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	env := filepath.Join(home, ".config", "aimem", "env")
	if b, err := os.ReadFile(env); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(l), "AIMEM_HTTP_LISTEN=") {
				return env + " sets AIMEM_HTTP_LISTEN"
			}
		}
	}
	return ""
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

// RunInstaller runs the laid-out installer in its user-level mode with
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

// release is where the release's files are: its download URL on the
// forge, or its directory in the export.
func (s Source) release(tag string) string {
	switch {
	case s.Base == "":
		return s.Web + "/" + s.Repo + "/releases/download/" + tag
	case strings.HasPrefix(s.Base, "https://"):
		return s.Base + "/aimem/" + tag
	default:
		return filepath.Join(s.Base, "aimem", tag)
	}
}

// file is one file of the release.
func (s Source) file(tag, name string) string {
	if s.Base != "" && !strings.HasPrefix(s.Base, "https://") {
		return filepath.Join(s.release(tag), name)
	}
	return s.release(tag) + "/" + name
}

// manifest is the release an export's MANIFEST names for aimem.
func (s Source) manifest(ctx context.Context) (string, error) {
	loc := s.Base + "/MANIFEST"
	if !strings.HasPrefix(s.Base, "https://") {
		loc = filepath.Join(s.Base, "MANIFEST")
	}
	b, err := s.get(ctx, loc, maxManifest)
	if err != nil {
		return "", refuse("%s is missing (%v); name the release with --release, or check AIMEM_BASE", loc, err)
	}
	lines := strings.Split(strings.ReplaceAll(string(b), "\r", ""), "\n")
	if format := strings.TrimSpace(lines[0]); format != exportFormat {
		return "", refuse("%s is %q, not a format aimem upgrade reads (%s)", loc, format, exportFormat)
	}
	for _, l := range lines[1:] {
		if f := strings.Fields(l); len(f) >= 2 && f[0] == "aimem" {
			return f[1], nil
		}
	}
	return "", refuse("%s names no aimem release", loc)
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

// get reads one file: over https, or from a local directory given by an
// absolute path. Any other scheme, plain http included, is refused.
func (s Source) get(ctx context.Context, loc string, max int64) ([]byte, error) {
	var r io.Reader
	switch {
	case strings.HasPrefix(loc, "https://"):
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.Client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", loc, resp.Status)
		}
		r = resp.Body
	case strings.Contains(loc, "://") || !filepath.IsAbs(loc):
		return nil, fmt.Errorf("%s: a source is an https:// URL or an absolute directory", loc)
	default:
		f, err := os.Open(loc)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s: larger than %d bytes", loc, max)
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
