package installer

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// wireCompare is a wire sandbox with the binary under test installed in
// a directory of its own and two empty directories to wire: one by the
// installer, one by `aimem wire` directly.
type wireCompare struct {
	*wireSandbox
	bin, byInstaller, byWire string
}

func newWireCompare(t *testing.T) *wireCompare {
	t.Helper()
	built := binary(t, "v99.0.0")
	w := &wireCompare{wireSandbox: newWireSandbox(t)}
	binDir := filepath.Join(w.dir, "bin")
	w.byInstaller, w.byWire = filepath.Join(w.dir, "by-installer"), filepath.Join(w.dir, "by-wire")
	for _, d := range []string{binDir, w.byInstaller, w.byWire} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	name := "aimem"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	w.bin = filepath.Join(binDir, name)
	raw, err := os.ReadFile(built)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.bin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, kv := range w.env {
		if k, v, _ := strings.Cut(kv, "="); strings.EqualFold(k, "PATH") {
			w.env[i] = k + "=" + binDir + string(os.PathListSeparator) + v
		}
	}
	w.env = append(w.env, "AIMEM_STATE_DIR="+filepath.Join(w.dir, "state"), "AIMEM_BIN_DIR="+binDir)
	return w
}

// wire runs `aimem wire` on dir with args.
func (w *wireCompare) wire(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(w.bin, append([]string{"wire", dir}, args...)...)
	cmd.Dir = w.dir
	cmd.Env = w.env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("aimem wire: %v\n%s", err, out)
	}
}

// tree reads every file below dir, keyed by its slash path.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		out[filepath.ToSlash(rel)] = string(raw)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, label string, got, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if g, ok := got[k]; !ok {
			t.Errorf("%s: %s missing", label, k)
		} else if g != v {
			t.Errorf("%s: %s differs:\n%s\nwant:\n%s", label, k, g, v)
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("%s: unexpected %s", label, k)
		}
	}
	if len(want) == 0 {
		t.Errorf("%s: nothing was wired", label)
	}
}

// checkInstallerMatchesWire wires byInstaller with install (twice, with
// AIMEM_PROJECT and AIMEM_GROUPS) and byWire with the binary, and compares.
func checkInstallerMatchesWire(t *testing.T, w *wireCompare, install func(env []string) (string, error)) {
	t.Helper()
	env := append(w.env, "AIMEM_PROJECT=demo", "AIMEM_GROUPS=a, b")
	if out, err := install(env); err != nil {
		t.Fatalf("installer: %v\n%s", err, out)
	}
	first := tree(t, w.byInstaller)
	if out, err := install(env); err != nil {
		t.Fatalf("installer, second run: %v\n%s", err, out)
	}
	sameTree(t, "second installer run", tree(t, w.byInstaller), first)
	w.wire(t, w.byWire, "--project", "demo", "--groups", "a,b")
	sameTree(t, "installer vs aimem wire", first, tree(t, w.byWire))
	if !strings.Contains(first[".aimem.json"], `"project": "demo"`) || !strings.Contains(first[".aimem.json"], `"b"`) {
		t.Errorf("AIMEM_PROJECT and AIMEM_GROUPS did not reach .aimem.json:\n%s", first[".aimem.json"])
	}
}

func TestShellProjectIsAimemWire(t *testing.T) {
	shellOnly(t)
	w := newWireCompare(t)
	installer, err := filepath.Abs(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	checkInstallerMatchesWire(t, w, func(env []string) (string, error) {
		cmd := exec.Command("bash", installer, "project", w.byInstaller)
		cmd.Dir = w.dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	})
}

func TestPowerShellTargetIsAimemWire(t *testing.T) {
	ps := powerShell(t)
	w := newWireCompare(t)
	installer, err := filepath.Abs(filepath.Join("..", "..", "install.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	checkInstallerMatchesWire(t, w, func(env []string) (string, error) {
		return runPowerShell(t, ps, w.dir, env, installer, "-Target", w.byInstaller)
	})
}
