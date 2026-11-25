package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"aimem/internal/store"
)

// The ssh sync's legs run under the real bash, against stand-ins: an `ssh`
// first on PATH that runs the remote command locally, a peer binary behind
// it, and a local binary in place of this one (selfPathOverride). Each
// stand-in writes part of its stream before it can fail, so a cut stream
// reaches the other side as a clean end of input, as it does over a real
// connection. The stand-in ssh logs each call, so a row shows that its legs
// reached it and not an ssh found earlier on PATH.
const sshStandIn = `#!/bin/sh
for a; do last=$a; done
echo "$last" >>"$SSH_LOG"
exec bash -c "$last"
`

const aimemStandIn = `#!/bin/sh
side=$1
shift
case "$1" in
export-events)
  echo '{"event":1}'
  if [ "$FAIL" = "$side-export-events" ]; then echo "$side export failed" >&2; exit 1; fi
  echo '{"event":2}' ;;
export-memories)
  echo '{"memory":1}'
  if [ "$FAIL" = "$side-export-memories" ]; then echo "$side export failed" >&2; exit 1; fi ;;
export-group-config) echo '{}' ;;
import-*) cat >/dev/null ;;
esac
`

// sshSyncBash puts a bash that runs the legs first on PATH: the system bash,
// and on Windows the Git Bash beside git, which PATH may not reach before
// another bash (WSL's launcher among them). It is Git's usr\bin\bash.exe,
// which keeps PATH as given: Git\bin\bash.exe puts Git's own directories
// first, and where Git ships an ssh, that ssh answers instead of the
// stand-in. In CI a missing bash fails the test: the platform's run is the evidence.
func sshSyncBash(t *testing.T) string {
	t.Helper()
	missing := func(why string) {
		if os.Getenv("CI") != "" {
			t.Fatal(why)
		}
		t.Skip(why)
	}
	if runtime.GOOS != "windows" {
		p, err := exec.LookPath("bash")
		if err != nil {
			missing("no bash")
		}
		return filepath.Dir(p)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		missing("no git, so no Git Bash")
	}
	// git.exe sits in Git\cmd or Git\mingw64\bin; bash.exe in Git\usr\bin.
	for dir := filepath.Dir(git); filepath.Dir(dir) != dir; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "usr", "bin", "bash.exe")); err == nil {
			return filepath.Join(dir, "usr", "bin")
		}
	}
	missing("no Git Bash above " + git)
	return ""
}

// setupSSHSync gives the test a state root with one local event (so a
// successful leg has a cursor to write), the stand-ins, and both cursors
// set to "old". It returns the local event's id and the stand-in ssh's log.
func setupSSHSync(t *testing.T) (string, string) {
	t.Helper()
	bashDir := sshSyncBash(t)
	root := t.TempDir()
	t.Setenv("AIMEM_STATE_DIR", root)
	t.Setenv("AIMEM_EMBED_MODEL", "")
	t.Setenv("AIMEM_SSH_OPTS", "")
	reg, err := store.NewRegistry(root)
	if err != nil {
		t.Fatal(err)
	}
	db, err := reg.Open("proj-ssh")
	if err != nil {
		t.Fatal(err)
	}
	seedEvent(t, db, "local-e1")
	max, err := db.MaxEventID()
	reg.Close()
	if err != nil || max == "" {
		t.Fatalf("seeded event id %q: %v", max, err)
	}

	bin := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(bin, name)
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("ssh", sshStandIn)
	stand := write("aimem-stand-in", aimemStandIn)
	local := write("local-aimem", "#!/bin/sh\nexec \""+filepath.ToSlash(stand)+"\" local \"$@\"\n")
	peer := write("peer-aimem", "#!/bin/sh\nexec \""+filepath.ToSlash(stand)+"\" peer \"$@\"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+bashDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AIMEM_REMOTE_BIN", filepath.ToSlash(peer))
	log := filepath.Join(bin, "ssh.log")
	t.Setenv("SSH_LOG", filepath.ToSlash(log))
	selfPathOverride = local
	t.Cleanup(func() { selfPathOverride = "" })

	writeCursor(sshPeer, "push", "old")
	writeCursor(sshPeer, "pull", "old")
	return max, log
}

const sshPeer = "peer.example"

// A leg's cursor moves only when both sides of it succeeded. Each case
// fails without pipefail: the leg would take the right side's clean end.
func TestSSHSyncMovesACursorOnlyAfterAWholeStream(t *testing.T) {
	for _, c := range []struct {
		name, fail, wantErr string
		wantStderr          string // the failing side's own line
		push, pull          bool   // whether each cursor moves
		missingLocal        bool
	}{
		{name: "whole streams", push: true, pull: true},
		{name: "the local export ends early", fail: "local-export-events", wantErr: "push failed", wantStderr: "local export failed"},
		{name: "the peer's export ends early", fail: "peer-export-events", wantErr: "pull failed", wantStderr: "peer export failed", push: true},
		{name: "the local memory export ends early", fail: "local-export-memories", wantErr: "memory push failed", wantStderr: "local export failed", push: true, pull: true},
		// As under a bash that cannot run this binary's Windows path (WSL's
		// launcher): the push used to send nothing and move its cursor.
		{name: "the local binary is not found", missingLocal: true, wantErr: "push failed", wantStderr: "no-such-aimem"},
	} {
		t.Run(c.name, func(t *testing.T) {
			max, log := setupSSHSync(t)
			t.Setenv("FAIL", c.fail)
			if c.missingLocal {
				selfPathOverride = filepath.Join(t.TempDir(), "no-such-aimem")
			}
			_, stderr, err := captureOut(t, func() error { return syncOne(sshPeer, "", "") })
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("sync failed: %v\n%s", err, stderr)
			case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
				t.Fatalf("want %q, got %v\n%s", c.wantErr, err, stderr)
			case !strings.Contains(stderr, c.wantStderr):
				t.Fatalf("stderr lacks %q\n%s", c.wantStderr, stderr)
			}
			if b, err := os.ReadFile(log); err != nil || len(b) == 0 {
				t.Fatalf("the stand-in ssh was not reached (%v)\n%s", err, stderr)
			}
			for dir, moves := range map[string]bool{"push": c.push, "pull": c.pull} {
				want := "old"
				if moves {
					want = max
				}
				if got := readCursor(sshPeer, dir); got != want {
					t.Errorf("%s cursor %q, want %q\n%s", dir, got, want, stderr)
				}
			}
		})
	}
}
