//go:build scenario

package scenario

// Throwaway probe: how long is a state-root socket path under t.TempDir()
// on this runner, and does `aimem serve` bind there? Dropped before review.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestScenarioCards(t *testing.T) {
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	bin := filepath.Join(t.TempDir(), "aimem"+exe)
	b := exec.Command("go", "build", "-o", bin, "aimem/cmd/aimem")
	b.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := b.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	tmp, _ := os.UserCacheDir()
	t.Logf("os.TempDir()=%q (%d bytes) cache=%q", os.TempDir(), len(os.TempDir()), tmp)
	short, err := os.MkdirTemp("", "aimem-sc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(short)
	for _, c := range [][2]string{{t.TempDir(), "hub"}, {t.TempDir(), "workstation"}, {short, "hub"}, {short, "workstation"}} {
		root, side := c[0], c[1]
		st := filepath.Join(root, side)
		os.MkdirAll(st, 0o700)
		sock := filepath.Join(st, "aimem.sock")
		t.Logf("%s socket %q: %d bytes (limit 103)", side, sock, len(sock))
		env := []string{"AIMEM_STATE_DIR=" + st, "HOME=" + st, "USERPROFILE=" + st}
		for _, kv := range os.Environ() {
			if len(kv) > 6 && kv[:6] == "AIMEM_" {
				continue
			}
			env = append(env, kv)
		}
		cmd := exec.Command(bin, "serve")
		cmd.Env = env
		logf, _ := os.Create(filepath.Join(root, side+".log"))
		cmd.Stdout, cmd.Stderr = logf, logf
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ok := false
		for i := 0; i < 100 && !ok; i++ {
			h := exec.Command(bin, "health")
			h.Env = env
			if h.Run() == nil {
				ok = true
			} else {
				time.Sleep(100 * time.Millisecond)
			}
		}
		cmd.Process.Kill()
		cmd.Wait()
		logf.Close()
		lb, _ := os.ReadFile(filepath.Join(root, side+".log"))
		t.Logf("%s health ok=%v; log:\n%s", side, ok, lb)
	}
}
