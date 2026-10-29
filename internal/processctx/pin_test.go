package processctx

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pin is the hub's current selection with the manifest's own digest,
// read with no Git and no route to the repository; every way it cannot
// be vouched for is an error, and nothing is cached.
func TestReadPin(t *testing.T) {
	noGit(t)
	b := bundle("# Handbook\n")
	sel := selection
	sel.Digest = b.Digest()
	h := &fakeHub{}
	h.change(func(h *fakeHub) { h.tasksEnabled, h.ref, h.set = true, &sel, b })
	dir, root, _ := checkout(t, h, true)

	pin, err := ReadPin(dir, root, "")
	sum := sha256.Sum256(b.Files[selection.Manifest])
	want := Pin{Project: "alpha", Repository: selection.Repo, Commit: selection.Commit, Manifest: selection.Manifest,
		SetDigest: b.Digest(), InstructionDigest: "sha256:" + hex.EncodeToString(sum[:])}
	if err != nil || *pin != want {
		t.Fatalf("pin: %+v %v\nwant %+v", pin, err, want)
	}
	// A known manifest's digest, independently of the code under test.
	if known := sha256.Sum256([]byte(`{"version":1,"handbook":"proc/handbook.md","checklists":{"READY":"proc/ready.json"},"templates":{"task":"proc/task.json"},"skills":["oh-code-review"]}`)); pin.InstructionDigest != "sha256:"+hex.EncodeToString(known[:]) || len(pin.InstructionDigest) != 71 {
		t.Fatalf("instruction digest %s", pin.InstructionDigest)
	}
	if _, err := os.Stat(filepath.Join(root, "process", "sets")); err == nil {
		t.Fatal("reading the pin cached the set")
	}

	fails := func(name, want string) {
		t.Helper()
		if pin, err := ReadPin(dir, root, ""); err == nil || pin != nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %+v %v (want %q)", name, pin, err, want)
		}
	}
	// No grant: the hub refuses the reference, or the set.
	h.change(func(h *fakeHub) { h.processStatus = http.StatusForbidden })
	fails("reference refused", "refused to read project alpha's process selection")
	h.change(func(h *fakeHub) { h.processStatus, h.setStatus = 0, http.StatusForbidden })
	fails("set refused", "refused to serve project alpha's process set")
	h.change(func(h *fakeHub) { h.setStatus = http.StatusNotFound })
	fails("no stored set", "serves no stored set for project alpha")
	h.change(func(h *fakeHub) { h.setStatus = 0 })

	// The set does not match the selection's digest.
	h.change(func(h *fakeHub) { h.setDigest = sel.Digest; h.set = bundle("# Another handbook\n") })
	fails("digest mismatch", "digest mismatch")
	h.change(func(h *fakeHub) { h.setDigest, h.set = "", b })

	// A selection from before stored sets carries no digest.
	old := selection
	h.change(func(h *fakeHub) { h.ref = &old })
	fails("before stored sets", "predates stored sets")

	// No selection at all.
	h.change(func(h *fakeHub) { h.ref = nil })
	fails("no selection", "no process is selected for project alpha")

	// The checkout's required credential is refused: nothing is read.
	h.change(func(h *fakeHub) { h.ref, h.identityStatus = &sel, http.StatusUnauthorized })
	fails("credential refused", "the hub refused the task credential")
}
