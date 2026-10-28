package installer

// install-hub.sh's install-only mode (AIMEM_NO_SERVICE=1): the hooks
// manage no service, an upgrade refuses while a hub answers on the
// configured address, and the last line printed is the serve command.
// scripts/check-install-hub-container.sh runs the whole installer this way
// in a container without systemd.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// hubHooks runs the hub-service-hooks block for a service user whose home
// is /home/sessiond, then script.
func hubHooks(t *testing.T, noService, script string) string {
	t.Helper()
	shellOnly(t)
	s := "set -euo pipefail\nHOME_DIR=/home/sessiond\nHUB_USER=sessiond\nNO_SERVICE=" + noService + "\n" +
		extract(t, "install-hub.sh", "hub-service-hooks") + script
	out, err := exec.Command("bash", "-c", s).CombinedOutput()
	if err != nil {
		t.Fatalf("hooks: %v\n%s", err, out)
	}
	return string(out)
}

const serveCmd = "runuser -u sessiond -- sh -c 'umask 077 && exec env -u XDG_RUNTIME_DIR HOME=/home/sessiond PATH=/home/sessiond/.local/bin:/usr/local/bin:/usr/bin:/bin /home/sessiond/.local/bin/aimem serve'"

func TestHubNoServiceHooksManageNoService(t *testing.T) {
	show := "echo \"TXN_MANAGED=$TXN_MANAGED\"\ndeclare -f svc_stop svc_start aimem_as\necho \"$SERVE_CMD\"\n"
	out := hubHooks(t, "1", show)
	if !strings.Contains(out, "TXN_MANAGED=0\n") {
		t.Errorf("install-only mode is managed:\n%s", out)
	}
	if strings.Contains(out, "sysuser") || strings.Contains(out, "systemctl") || strings.Contains(out, "XDG_RUNTIME_DIR=") {
		t.Errorf("install-only hooks reach systemd or set a runtime dir:\n%s", out)
	}
	if !strings.Contains(out, "env -u XDG_RUNTIME_DIR \"HOME=$HOME_DIR\"") || !strings.Contains(out, serveCmd+"\n") {
		t.Errorf("aimem_as and the serve command do not both clear XDG_RUNTIME_DIR:\n%s", out)
	}
	// The service mode keeps its hooks.
	out = hubHooks(t, "", show)
	if !strings.Contains(out, "TXN_MANAGED=1\n") || !strings.Contains(out, "sysuser stop aimem-curate.timer") ||
		!strings.Contains(out, "XDG_RUNTIME_DIR=/run/user/") {
		t.Errorf("the service mode's hooks changed:\n%s", out)
	}
}

// runPreflight runs no_service_preflight against an env file naming listen.
func runPreflight(t *testing.T, env string) (string, error) {
	t.Helper()
	shellOnly(t)
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("no curl")
	}
	envf := filepath.Join(t.TempDir(), "env")
	if err := os.WriteFile(envf, []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	s := "set -euo pipefail\n" + extract(t, "install-hub.sh", "no-service-preflight") + `no_service_preflight "$1"` + "\necho passed\n"
	out, err := exec.Command("bash", "-c", s, "preflight", envf).CombinedOutput()
	return string(out), err
}

func TestHubNoServicePreflightRefusesWhileAHubAnswers(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/health" {
				t.Errorf("probed %s", r.URL.Path)
			}
			w.WriteHeader(status)
		}))
		port := srv.Listener.Addr().(*net.TCPAddr).Port
		listen := ":" + strconv.Itoa(port)
		out, err := runPreflight(t, "AIMEM_HTTP_LISTEN="+listen+"\nAIMEM_HTTP_TOKEN=x\n")
		srv.Close()
		want := "ERROR: aimem answers at http://127.0.0.1:" + strconv.Itoa(port) + " (AIMEM_HTTP_LISTEN=" + listen +
			"), and with AIMEM_NO_SERVICE=1 this installer does not stop it. Stop it, run this again, then start it. Nothing was changed."
		if err == nil || !strings.Contains(out, want) || strings.Contains(out, "passed") {
			t.Errorf("status %d: err %v, want the refusal:\n%s", status, err, out)
		}
	}
}

func TestHubNoServicePreflightPassesWithNothingListening(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	for name, env := range map[string]string{
		"closed port":    "AIMEM_HTTP_LISTEN=" + addr + "\n",
		"empty env file": "",
		"no listen":      "AIMEM_HTTP_TOKEN=x\n",
	} {
		out, err := runPreflight(t, env)
		if err != nil || !strings.Contains(out, "passed") {
			t.Errorf("%s: %v\n%s", name, err, out)
		}
	}
}

// The serve command is the last line in install-only mode, after the
// curation hint; an upgrade also names the process refresh.
func TestHubNoServicePrintsTheServeCommandLast(t *testing.T) {
	for _, upgraded := range []string{"", "1"} {
		out := hubHooks(t, "1", "UPGRADED="+upgraded+"\nENVF=/home/sessiond/.config/aimem/env\n"+
			extract(t, "install-hub.sh", "no-service-finish"))
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if lines[len(lines)-1] != serveCmd {
			t.Errorf("upgraded=%q: the last line is not the serve command:\n%s", upgraded, out)
		}
		if !strings.Contains(out, "/home/sessiond/.local/sbin/curate-all.sh hourly as sessiond") {
			t.Errorf("upgraded=%q: no curation hint:\n%s", upgraded, out)
		}
		if got := strings.Contains(out, "aimem process refresh"); got != (upgraded == "1") {
			t.Errorf("upgraded=%q: process refresh hint %v:\n%s", upgraded, got, out)
		}
	}
	if out := hubHooks(t, "", "UPGRADED=\nENVF=x\n"+extract(t, "install-hub.sh", "no-service-finish")); out != "" {
		t.Errorf("the service mode prints the install-only lines:\n%s", out)
	}
}
