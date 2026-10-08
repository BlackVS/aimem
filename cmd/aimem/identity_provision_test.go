package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aimem/internal/privatefile"
)

const provisionEndpoint = "https://aicrew.example/v1/crew/introspect"

func (g *identityCLIRig) provision(t *testing.T, service, dir string, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"peer", "provision", service, "--endpoint", provisionEndpoint, "--peer-trust-dns", "--output-dir", dir}, extra...)
	return g.run(t, args...)
}

// credsOf lists a peer's credentials as "id state operation" lines.
func (g *identityCLIRig) credsOf(t *testing.T, service string) []string {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(g.mustRun(t, "cred", "list", service), "\n") {
		if f := strings.Fields(l); len(f) >= 3 && len(f[0]) > 30 {
			lines = append(lines, f[0]+" "+f[1]+" "+f[len(f)-1])
		}
	}
	return lines
}

// readCredFiles returns the four files' contents; each must be private and
// hold one peer bearer.
func readCredFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, f := range provisionFiles {
		path := filepath.Join(dir, f.name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", f.name, err)
		}
		if !strings.HasPrefix(string(b), "aimem_peer_") || strings.Count(string(b), "\n") != 1 {
			t.Errorf("%s does not hold one peer bearer", f.name)
		}
		if err := privatefile.Check(path); err != nil {
			t.Errorf("%s is not owner-only: %v", f.name, err)
		}
		got[f.name] = string(b)
	}
	return got
}

// linesWith counts the output lines that start with prefix.
func linesWith(out, prefix string) int {
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}
	return n
}

func hubIDOf(t *testing.T, g *identityCLIRig) string {
	t.Helper()
	for _, l := range strings.Split(g.mustRun(t, "peer", "list"), "\n") {
		if i := strings.Index(l, "  hub "); i >= 0 {
			return strings.TrimSpace(l[i+len("  hub "):])
		}
	}
	t.Fatal("peer list names no hub")
	return ""
}

// A fresh provision registers the peer, writes the four credentials into
// owner-only files under the fixed names and prints the hub ID; a rerun
// reports what exists and issues nothing.
func TestPeerProvisionFreshAndIdempotent(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	dir := filepath.Join(g.dir, "aicrew-creds") // created by the command
	out, err := g.provision(t, "aicrew-example", dir)
	if err != nil {
		t.Fatalf("provision: %v\n%s", err, out)
	}
	hub := hubIDOf(t, g)
	if !strings.Contains(out, "hub ID "+hub+"\n") || !strings.Contains(out, "identity peer aicrew-example registered") {
		t.Errorf("provision output:\n%s", out)
	}
	files := readCredFiles(t, dir)
	creds := g.credsOf(t, "aicrew-example")
	if len(creds) != 4 {
		t.Fatalf("want four credentials, got %v", creds)
	}
	for _, f := range provisionFiles {
		found := false
		for _, c := range creds {
			found = found || strings.HasSuffix(c, " active "+f.operation)
		}
		if !found {
			t.Errorf("no active %s credential: %v", f.operation, creds)
		}
	}

	out, err = g.provision(t, "aicrew-example", dir)
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	if !strings.Contains(out, "already registered with this endpoint and trust") || !strings.Contains(out, "nothing was issued") ||
		linesWith(out, "kept ") != 4 || !strings.Contains(out, "hub ID "+hub) {
		t.Errorf("rerun output:\n%s", out)
	}
	if again := readCredFiles(t, dir); len(again) != 4 || again["aimem-redeem.token"] != files["aimem-redeem.token"] || again["aimem-team-read.token"] != files["aimem-team-read.token"] {
		t.Error("the rerun changed a credential file")
	}
	if n := len(g.credsOf(t, "aicrew-example")); n != 4 {
		t.Errorf("the rerun issued credentials: %d", n)
	}

	// A provisioning cut short is finished by running it again: only the
	// missing file gets a credential.
	if err := os.Remove(filepath.Join(dir, "aimem-read.token")); err != nil {
		t.Fatal(err)
	}
	out, err = g.provision(t, "aicrew-example", dir)
	if err != nil || linesWith(out, "issued ") != 1 || !strings.Contains(out, "issued reservation.read credential") || linesWith(out, "kept ") != 3 {
		t.Fatalf("resume: %v\n%s", err, out)
	}
	if again := readCredFiles(t, dir); again["aimem-redeem.token"] != files["aimem-redeem.token"] {
		t.Error("the resume changed a kept file")
	}
	if n := len(g.credsOf(t, "aicrew-example")); n != 5 {
		t.Errorf("the resume issued %d credentials in all, want 5", n)
	}

	// A kept file whose credential the hub no longer holds active is named.
	for _, c := range g.credsOf(t, "aicrew-example") {
		if strings.HasSuffix(c, " team.read") {
			g.mustRun(t, "cred", "revoke", "--peer", "aicrew-example", "--credential", strings.Fields(c)[0])
		}
	}
	if out, err := g.provision(t, "aicrew-example", dir); err != nil ||
		!strings.Contains(out, "warning: aicrew-example has no active team.read credential on the hub") || linesWith(out, "warning: ") != 1 {
		t.Errorf("rerun with a revoked kept credential: %v\n%s", err, out)
	}

	// A different endpoint for the registered peer is refused.
	if out, err := g.run(t, "peer", "provision", "aicrew-example", "--endpoint", "https://other.example/v1/crew/introspect",
		"--peer-trust-dns", "--output-dir", dir); err == nil || !strings.Contains(err.Error(), "not the ones given") {
		t.Errorf("provision with another endpoint: %v\n%s", err, out)
	}
	g.assertNoSecrets(t)
}

// --replace disables the active old peer in the same step; without it the
// one-active-peer rule is reported before anything changes.
func TestPeerProvisionReplacesTheActivePeer(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	g.register(t) // aicrew-example, enabled
	dir := filepath.Join(g.dir, "renamed-creds")
	if out, err := g.provision(t, "aicrew-renamed", dir); err == nil || !strings.Contains(err.Error(), "aicrew-example is enabled; name it with --replace") {
		t.Fatalf("provision beside an active peer: %v\n%s", err, out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the refused provision wrote files: %v", entries)
	}
	if out := g.mustRun(t, "peer", "list"); strings.Contains(out, "aicrew-renamed") || !strings.Contains(out, "aicrew-example  enabled") {
		t.Fatalf("the refused provision changed the peers:\n%s", out)
	}

	out, err := g.provision(t, "aicrew-renamed", dir, "--replace", "aicrew-example")
	if err != nil {
		t.Fatalf("provision --replace: %v\n%s", err, out)
	}
	if !strings.Contains(out, "identity peer aicrew-example disabled (replaced by aicrew-renamed)") {
		t.Errorf("provision --replace output:\n%s", out)
	}
	list := g.mustRun(t, "peer", "list")
	if !strings.Contains(list, "aicrew-example  disabled") || !strings.Contains(list, "aicrew-renamed  enabled") {
		t.Errorf("after --replace:\n%s", list)
	}
	readCredFiles(t, dir)
	// The rerun of the same command is idempotent too.
	if out, err := g.provision(t, "aicrew-renamed", dir, "--replace", "aicrew-example"); err != nil || !strings.Contains(out, "nothing was issued") {
		t.Errorf("rerun of provision --replace: %v\n%s", err, out)
	}
	// After the old peer is retired, the same command still runs.
	g.mustRun(t, "peer", "retire", "aicrew-example")
	if out, err := g.provision(t, "aicrew-renamed", dir, "--replace", "aicrew-example"); err != nil ||
		!strings.Contains(out, "--replace aicrew-example: no such identity peer is registered; nothing to disable") || !strings.Contains(out, "nothing was issued") {
		t.Errorf("rerun after the old peer was retired: %v\n%s", err, out)
	}
	// A mistyped --replace does not get past the one-active-peer rule.
	if _, err := g.provision(t, "aicrew-third", filepath.Join(g.dir, "third"), "--replace", "no-such-peer"); err == nil ||
		!strings.Contains(err.Error(), "aicrew-renamed is enabled; name it with --replace") {
		t.Errorf("--replace of an unknown peer beside an enabled one: %v", err)
	}
	g.assertNoSecrets(t)
}

// A non-empty credential file is never overwritten: a new peer with one in
// its directory is refused before anything is registered or issued.
func TestPeerProvisionRefusesToOverwriteACredentialFile(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	dir := filepath.Join(g.dir, "creds")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(dir, "aimem-read.token")
	if err := os.WriteFile(existing, []byte("an older credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := g.requests.Load()
	out, err := g.provision(t, "aicrew-example", dir)
	if err == nil || !strings.Contains(err.Error(), "already holds a credential and is never overwritten") {
		t.Fatalf("provision over a credential file: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(existing); string(b) != "an older credential\n" {
		t.Errorf("the credential file changed: %q", b)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the refused provision left files: %v", entries)
	}
	// One request: the peer list. Nothing was registered or issued.
	if n := g.requests.Load() - before; n != 1 {
		t.Errorf("the refused provision made %d hub requests, want 1 (the peer list)", n)
	}
	if out := g.mustRun(t, "peer", "list"); !strings.Contains(out, "no identity peer") {
		t.Errorf("the refused provision registered a peer:\n%s", out)
	}
	// An empty file is not a credential: it is replaced.
	if err := os.WriteFile(existing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := g.provision(t, "aicrew-example", dir); err != nil {
		t.Fatalf("provision over an empty file: %v\n%s", err, out)
	}
	readCredFiles(t, dir)
	g.assertNoSecrets(t)
}

// A registration the hub refuses after --replace disabled the old peer
// enables the old peer again and leaves no file behind.
func TestPeerProvisionReplaceRollsBackAFailedRegistration(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	g.register(t)
	dir := filepath.Join(g.dir, "renamed-creds")
	_, err := g.run(t, "peer", "provision", "aicrew-renamed", "--endpoint", "http://aicrew.example/v1/crew/introspect",
		"--peer-trust-dns", "--output-dir", dir, "--replace", "aicrew-example")
	if err == nil || !strings.Contains(err.Error(), "aicrew-example is enabled again") {
		t.Fatalf("provision with an endpoint the hub refuses: %v", err)
	}
	if out := g.mustRun(t, "peer", "list"); !strings.Contains(out, "aicrew-example  enabled") || strings.Contains(out, "aicrew-renamed") {
		t.Errorf("after the failed registration:\n%s", out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the failed provision left files: %v", entries)
	}
	g.assertNoSecrets(t)
}

// An expiry the hub would refuse is refused before any hub request, so
// --replace never leaves the old peer disabled and the new one without
// credentials.
func TestPeerProvisionRefusesAnExpiryBeforeTheHubChanges(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	g.register(t)
	dir := filepath.Join(g.dir, "renamed-creds")
	const outOfRange = "must expire in the future and within 366 days; nothing changed"
	for _, c := range []struct{ expires, want string }{
		{time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), outOfRange},
		{"367d", outOfRange},
		{time.Now().Add(400 * 24 * time.Hour).UTC().Format(time.RFC3339), outOfRange},
		{"213504d", "use a positive number of days"}, // overflows a time.Duration
	} {
		expires := c.expires
		before := g.requests.Load()
		_, err := g.provision(t, "aicrew-renamed", dir, "--replace", "aicrew-example", "--expires", expires)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("--expires %s: %v", expires, err)
		}
		if n := g.requests.Load() - before; n != 0 {
			t.Errorf("--expires %s: %d hub requests, want none", expires, n)
		}
	}
	if out := g.mustRun(t, "peer", "list"); !strings.Contains(out, "aicrew-example  enabled") || strings.Contains(out, "aicrew-renamed") {
		t.Errorf("the refused provisions changed the peers:\n%s", out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the refused provisions left files: %v", entries)
	}
	// The hub's own limit is accepted.
	if out, err := g.provision(t, "aicrew-renamed", dir, "--replace", "aicrew-example", "--expires", "366d"); err != nil {
		t.Fatalf("--expires 366d: %v\n%s", err, out)
	}
	readCredFiles(t, dir)
	g.assertNoSecrets(t)
}

// A credential list that fails after the files are settled is reported as
// unread, not as every kept file holding a stale credential.
func TestPeerProvisionReportsAnUnreadableCredentialStatus(t *testing.T) {
	var listed, failAt atomic.Int32 // the credential list numbered failAt fails
	g := newIdentityCLIRig(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/credentials") && listed.Add(1) == failAt.Load() {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	dir := filepath.Join(g.dir, "creds")
	if out, err := g.provision(t, "aicrew-example", dir); err != nil {
		t.Fatalf("provision: %v\n%s", err, out)
	}
	files := readCredFiles(t, dir)
	// The rerun lists the credentials twice: before issuing, and for the
	// status of the kept files. Only the second fails.
	listed.Store(0)
	failAt.Store(2)
	out, err := g.provision(t, "aicrew-example", dir)
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	if listed.Load() != 2 {
		t.Fatalf("the rerun listed the credentials %d times, want 2", listed.Load())
	}
	if linesWith(out, "warning: the credentials of aicrew-example could not be read") != 1 ||
		strings.Contains(out, "has no active") || linesWith(out, "kept ") != 4 || !strings.Contains(out, "nothing was issued") {
		t.Errorf("rerun with an unreadable credential status:\n%s", out)
	}
	if again := readCredFiles(t, dir); again["aimem-redeem.token"] != files["aimem-redeem.token"] || again["aimem-team-read.token"] != files["aimem-team-read.token"] {
		t.Error("the rerun changed a credential file")
	}
	g.assertNoSecrets(t)
}

// The hub ID goes into the directory beside the credentials, owner-only, so
// the peer's side reads it instead of having it retyped. A rerun keeps a
// file with the same ID; one naming another hub is refused before anything
// changes.
func TestPeerProvisionWritesTheHubID(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	dir := filepath.Join(g.dir, "creds")
	idFile := filepath.Join(dir, provisionHubIDFile)
	out, err := g.provision(t, "aicrew-example", dir)
	if err != nil {
		t.Fatalf("provision: %v\n%s", err, out)
	}
	hub := hubIDOf(t, g)
	if b, err := os.ReadFile(idFile); err != nil || string(b) != hub+"\n" {
		t.Fatalf("%s holds %q (%v), want the hub ID %s on one line", provisionHubIDFile, b, err, hub)
	}
	if err := privatefile.Check(idFile); err != nil {
		t.Errorf("%s is not owner-only: %v", provisionHubIDFile, err)
	}
	if !strings.Contains(out, "wrote the hub ID into "+idFile) {
		t.Errorf("provision output:\n%s", out)
	}

	out, err = g.provision(t, "aicrew-example", dir)
	if err != nil || !strings.Contains(out, idFile+" already holds this hub ID") || !strings.Contains(out, "nothing was issued") {
		t.Fatalf("rerun: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(idFile); string(b) != hub+"\n" {
		t.Errorf("the rerun changed %s: %q", provisionHubIDFile, b)
	}

	// A file naming another hub: refused after the peer list, with nothing
	// changed.
	other := "01a00000-0000-7000-8000-000000000000\n"
	if err := os.WriteFile(idFile, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	before := g.requests.Load()
	if _, err := g.provision(t, "aicrew-example", dir); err == nil || !strings.Contains(err.Error(), "belongs to another hub; nothing changed") {
		t.Fatalf("provision with another hub's ID file: %v", err)
	}
	if n := g.requests.Load() - before; n != 1 {
		t.Errorf("the refused provision made %d hub requests, want 1 (the peer list)", n)
	}
	if b, _ := os.ReadFile(idFile); string(b) != other {
		t.Errorf("the refused provision changed %s: %q", provisionHubIDFile, b)
	}
	g.assertNoSecrets(t)
}

// On a hub with no peer yet there is no ID to check a hub ID file against,
// so one that holds an ID is refused; an empty one is written.
func TestPeerProvisionHubIDFileOnAHubWithoutPeers(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	dir := filepath.Join(g.dir, "creds")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	idFile := filepath.Join(dir, provisionHubIDFile)
	if err := os.WriteFile(idFile, []byte("01a00000-0000-7000-8000-000000000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := g.requests.Load()
	if _, err := g.provision(t, "aicrew-example", dir); err == nil || !strings.Contains(err.Error(), "no identity peer to confirm its ID against") {
		t.Fatalf("provision with an unconfirmable hub ID file: %v", err)
	}
	if n := g.requests.Load() - before; n != 1 {
		t.Errorf("the refused provision made %d hub requests, want 1 (the peer list)", n)
	}
	if out := g.mustRun(t, "peer", "list"); !strings.Contains(out, "no identity peer") {
		t.Errorf("the refused provision registered a peer:\n%s", out)
	}
	if err := os.WriteFile(idFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := g.provision(t, "aicrew-example", dir); err != nil {
		t.Fatalf("provision over an empty hub ID file: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(idFile); string(b) != hubIDOf(t, g)+"\n" {
		t.Errorf("%s holds %q after replacing the empty file", provisionHubIDFile, b)
	}
	if err := privatefile.Check(idFile); err != nil {
		t.Errorf("%s is not owner-only: %v", provisionHubIDFile, err)
	}
	g.assertNoSecrets(t)
}

// A hub ID file that holds only whitespace holds nothing, for the check and
// the write alike: it is replaced, on a fresh provision and on a rerun.
func TestPeerProvisionReplacesABlankHubIDFile(t *testing.T) {
	g := newIdentityCLIRig(t, nil)
	dir := filepath.Join(g.dir, "creds")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	idFile := filepath.Join(dir, provisionHubIDFile)
	for _, blank := range []string{"\n", " \r\n\t"} {
		if err := os.WriteFile(idFile, []byte(blank), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := g.provision(t, "aicrew-example", dir)
		if err != nil || !strings.Contains(out, "wrote the hub ID into "+idFile) {
			t.Fatalf("provision over a hub ID file holding %q: %v\n%s", blank, err, out)
		}
		if b, _ := os.ReadFile(idFile); string(b) != hubIDOf(t, g)+"\n" {
			t.Errorf("%s holds %q after replacing %q", provisionHubIDFile, b, blank)
		}
		if err := privatefile.Check(idFile); err != nil {
			t.Errorf("%s is not owner-only: %v", provisionHubIDFile, err)
		}
	}
	g.assertNoSecrets(t)
}
