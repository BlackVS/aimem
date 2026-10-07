package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// extract returns an installer's text between the BEGIN and END markers.
func extract(t *testing.T, file, name string) string {
	t.Helper()
	s := read(t, file)
	begin, end := "# BEGIN "+name+"\n", "# END "+name+"\n"
	i, j := strings.Index(s, begin), strings.Index(s, end)
	if i < 0 || j < i {
		t.Fatalf("%s has no %s between markers", file, name)
	}
	return s[i : j+len(end)]
}

// The hub installer runs alone (curl | bash) and cannot source install.sh,
// so it carries its own copy of the transaction; the copies must not drift.
func TestUpgradeTransactionIsOneCopy(t *testing.T) {
	if a, b := extract(t, "install.sh", "upgrade-transaction"), extract(t, "install-hub.sh", "upgrade-transaction"); a != b {
		t.Fatal("install.sh and install-hub.sh carry different upgrade transactions; keep them identical")
	}
}

var (
	binDir  string
	binOnce sync.Map // name -> *sync.Once
	binErr  sync.Map // name -> error
)

func TestMain(m *testing.M) {
	d, err := os.MkdirTemp("", "aimem-installer-bin")
	if err != nil {
		panic(err)
	}
	binDir = d
	code := m.Run()
	os.RemoveAll(d)
	os.Exit(code)
}

// binary builds, once per test run, an aimem stamped with version, or the
// broken stand-in when version is "broken".
func binary(t *testing.T, version string) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to build the binaries under test")
	}
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	out := filepath.Join(binDir, "aimem-"+version+exe)
	once, _ := binOnce.LoadOrStore(version, new(sync.Once))
	once.(*sync.Once).Do(func() {
		args := []string{"build", "-o", out}
		if version == "broken" {
			args = append(args, "./testdata/broken")
		} else {
			args = append(args, "-ldflags", "-X main.version="+version, "aimem/cmd/aimem")
		}
		cmd := exec.Command("go", args...)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if b, err := cmd.CombinedOutput(); err != nil {
			binErr.Store(version, string(b)+err.Error())
		}
	})
	if e, ok := binErr.Load(version); ok {
		t.Fatalf("build %s: %s", version, e)
	}
	return out
}

// sandbox is a disposable installation: a binary directory with the old
// release installed, and a state root. Its parent directory is short,
// because the service socket lives in the state root and Unix socket paths
// are limited to about 104 bytes.
//
// An offline sandbox is an installation whose service is stopped and whose
// state root is named only in its env file (~/.config/aimem/env), not in
// the environment and not at the default location: the installer must ask
// the installed binary where the state is.
type sandbox struct {
	dir, bin, state string
	env             []string
}

func newSandbox(t *testing.T, oldBinary string, offline bool) *sandbox {
	t.Helper()
	base := ""
	if runtime.GOOS != "windows" {
		base = "/tmp"
	}
	dir, err := os.MkdirTemp(base, "aimem-up")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s := &sandbox{dir: dir, bin: filepath.Join(dir, "bin"), state: filepath.Join(dir, "state")}
	home := filepath.Join(dir, "home")
	for _, d := range []string{s.bin, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	installed := filepath.Join(s.bin, filepath.Base(oldBinaryName()))
	b, err := os.ReadFile(oldBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, b, 0o755); err != nil {
		t.Fatal(err)
	}
	// Nothing the developer's or runner's environment says about aimem
	// reaches the processes under test: their HOME, state root and socket
	// are all inside the sandbox.
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(strings.ToUpper(k), "AIMEM_"), strings.HasPrefix(strings.ToUpper(k), "XDG_"),
			strings.EqualFold(k, "HOME"), strings.EqualFold(k, "USERPROFILE"):
			continue
		}
		s.env = append(s.env, kv)
	}
	s.env = append(s.env, "HOME="+home, "USERPROFILE="+home, "STATE="+s.state)
	if !offline {
		s.env = append(s.env, "AIMEM_STATE_DIR="+s.state)
		return s
	}
	s.env = append(s.env, "OFFLINE=1")
	conf := filepath.Join(home, ".config", "aimem")
	if err := os.MkdirAll(conf, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conf, "env"), []byte("AIMEM_STATE_DIR="+s.state+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return s
}

func oldBinaryName() string {
	if runtime.GOOS == "windows" {
		return "aimem.exe"
	}
	return "aimem"
}

func (s *sandbox) installed() string { return filepath.Join(s.bin, oldBinaryName()) }

// version runs the installed binary's `version`.
func (s *sandbox) version(t *testing.T) string {
	t.Helper()
	cmd := exec.Command(s.installed(), "version")
	cmd.Env = s.env
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("installed binary does not run: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// siblings returns the directories beside the state root whose names start
// with the state root's name plus suffix.
func (s *sandbox) siblings(t *testing.T, suffix string) []string {
	t.Helper()
	m, err := filepath.Glob(s.state + suffix + "*")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// checkSuccess: the new release is installed and the backup holds the
// state as it was before the swap, without the socket.
func checkSuccess(t *testing.T, s *sandbox, out string) {
	t.Helper()
	if got := s.version(t); got != "aimem v0.0.2" {
		t.Errorf("installed binary reports %q after the upgrade; want aimem v0.0.2\n%s", got, out)
	}
	if !strings.Contains(out, "upgraded aimem v0.0.1 -> v0.0.2") {
		t.Errorf("the output does not report both versions:\n%s", out)
	}
	backups := s.siblings(t, ".backup-")
	if len(backups) != 1 {
		t.Fatalf("want one state backup beside the state root, found %v\n%s", backups, out)
	}
	if !strings.Contains(out, backups[0]) {
		t.Errorf("the output does not name the backup %s:\n%s", backups[0], out)
	}
	if b, err := os.ReadFile(filepath.Join(backups[0], "marker")); err != nil || string(b) != "before\n" {
		t.Errorf("backup lacks the pre-upgrade marker: %q %v", b, err)
	}
	if exists(filepath.Join(backups[0], "aimem.sock")) {
		t.Error("the backup holds the service socket")
	}
	if runtime.GOOS != "windows" {
		a, _ := os.Stat(s.state)
		b, _ := os.Stat(backups[0])
		if a == nil || b == nil || a.Mode().Perm() != b.Mode().Perm() {
			t.Errorf("backup mode differs from the state root's: %v vs %v", b, a)
		}
	}
}

// checkRolledBack: the previous binary and the pre-upgrade state are back,
// and the state the failed release left is kept aside.
func checkRolledBack(t *testing.T, s *sandbox, out string) {
	t.Helper()
	if got := s.version(t); got != "aimem v0.0.1" {
		t.Errorf("installed binary reports %q after the failed upgrade; want the previous aimem v0.0.1\n%s", got, out)
	}
	if !strings.Contains(out, "ROLLED BACK: aimem v0.0.1 is running again") {
		t.Errorf("the output does not say the upgrade was rolled back and the service is up:\n%s", out)
	}
	if b, err := os.ReadFile(filepath.Join(s.state, "marker")); err != nil || string(b) != "before\n" {
		t.Errorf("state root lost the pre-upgrade marker: %q %v", b, err)
	}
	if exists(filepath.Join(s.state, "migrated-by-broken")) {
		t.Error("the state root still holds what the failed release wrote")
	}
	failed := s.siblings(t, ".failed-")
	if len(failed) != 1 || !exists(filepath.Join(failed[0], "migrated-by-broken")) {
		t.Errorf("want the failed release's state kept aside, found %v", failed)
	}
	if len(s.siblings(t, ".backup-")) != 1 {
		t.Error("the backup did not stay after the rollback")
	}
}
