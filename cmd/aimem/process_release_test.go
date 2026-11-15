package main

// The release publishes the reference process set (processes/aimem) as
// aimem-process.tar.gz, built by scripts/process-archive.sh from the tag's
// checkout; a hub host without access to the repository imports it with
// `aimem process select --from-archive`. The set's texts name no host
// outside a short allow-list.

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"aimem/internal/process"
)

const (
	publicRepo     = "https://github.com/BlackVS/aimem"
	referenceSet   = "../../processes/aimem"
	referenceMfest = "processes/aimem/manifest.json"
)

// The archive the release workflow writes, built by the same script from a
// local repository with no network, imports with the sums check and names
// the public repository and the commit.
func TestReleaseProcessArchiveImports(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the release builds the archive on Linux; the script is POSIX sh")
	}
	for _, tool := range []string{"sh", "git"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s", tool)
		}
	}
	tmp := t.TempDir()
	run := func(dir string, env []string, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	// A repository holding this checkout's reference set, committed.
	repo := filepath.Join(tmp, "repo")
	dst := filepath.Join(repo, "processes", "aimem")
	if err := os.CopyFS(dst, os.DirFS(referenceSet)); err != nil {
		t.Fatal(err)
	}
	gitEnv := []string{"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid"}
	run(repo, gitEnv, "git", "init", "-q")
	run(repo, gitEnv, "git", "add", "-A")
	run(repo, gitEnv, "git", "commit", "-qm", "set")
	commit := run(repo, gitEnv, "git", "rev-parse", "HEAD")

	// The workflow's command, with this test binary as aimem (its
	// TestHelperMain runs main) in place of the release build.
	bin := filepath.Join(tmp, "aimem")
	wrapper := "#!/bin/sh\nIFS=$(printf '\\037')\nAIMEM_TEST_MAIN=1 AIMEM_TEST_ARGS=\"$*\" exec \"$AIMEM_TEST_BINARY\" -test.run='^TestHelperMain$'\n"
	if err := os.WriteFile(bin, []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	dist := filepath.Join(tmp, "dist")
	if err := os.Mkdir(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	exportEnv := append(gitEnv, "AIMEM_TEST_BINARY="+os.Args[0], "AIMEM_STATE_DIR="+filepath.Join(tmp, "state"),
		"XDG_STATE_HOME="+filepath.Join(tmp, "state"), "HOME="+filepath.Join(tmp, "home"), "AIMEM_HUB_URL=", "AIMEM_HUB_TOKEN=", "AIMEM_SOCKET=")
	run(".", exportEnv, "sh", "../../scripts/process-archive.sh", bin, repo, dist)
	archive := filepath.Join(dist, "aimem-process.tar.gz")
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := process.ArchiveCommit(data); err != nil || got != commit {
		t.Fatalf("the archive records commit %q (%v), want %s", got, err, commit)
	}
	if entries, _ := os.ReadDir(dist); len(entries) != 1 {
		t.Fatalf("dist holds more than the archive: %v", entries)
	}
	// The workflow's SHA256SUMS lists it with the other assets.
	sums := filepath.Join(dist, "SHA256SUMS")
	if err := os.WriteFile(sums, []byte(process.SumsLine([]byte("binary\n"), "aimem-linux-amd64")+process.SumsLine(data, "aimem-process.tar.gz")), 0o644); err != nil {
		t.Fatal(err)
	}

	// The archive holds exactly this checkout's set: the set git archive
	// gives for the commit, read the way a forge's tarball is.
	tarball := filepath.Join(tmp, "source.tar.gz")
	run(repo, gitEnv, "git", "archive", "--format=tar.gz", "--prefix=aimem-src/", "-o", tarball, "HEAD")
	src, err := os.ReadFile(tarball)
	if err != nil {
		t.Fatal(err)
	}
	want, err := process.ReadArchive(bytes.NewReader(src), referenceMfest)
	if err != nil {
		t.Fatal(err)
	}

	reg := projectService(t)
	if _, err := reg.Open("example"); err != nil {
		t.Fatal(err)
	}
	selectOut := func(args ...string) (string, error) {
		return stdoutOf(t, func() error {
			return processCmd(append([]string{"select", publicRepo, commit, referenceMfest, "--commit", "-p", "example"}, args...))
		})
	}
	// A sums file that does not vouch for the archive is refused first.
	bad := filepath.Join(tmp, "SHA256SUMS.bad")
	if err := os.WriteFile(bad, []byte(process.SumsLine([]byte("other"), "aimem-process.tar.gz")), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := selectOut("--from-archive", archive, "--sums", bad); err == nil || !strings.Contains(err.Error(), "does not match its line") {
		t.Fatalf("a sums file that does not match: %v", err)
	}
	text, err := selectOut("--from-archive", archive, "--sums", sums)
	var out struct {
		Current  process.Ref `json:"current"`
		Imported bool        `json:"imported"`
	}
	if err != nil || json.Unmarshal([]byte(text), &out) != nil {
		t.Fatalf("import: %v\n%s", err, text)
	}
	if !out.Imported || out.Current.Repo != publicRepo || out.Current.Commit != commit || out.Current.Name != "aimem" || out.Current.Digest != want.Digest() {
		t.Fatalf("the hub shows %+v, imported %v; want %s at %s, set %s", out.Current, out.Imported, publicRepo, commit, want.Digest())
	}
}

// Hosts a text of the reference set may name: the public forge and the
// names reserved for examples.
var allowedHosts = []string{"github.com", "example.com", "example.org", "example.net", "localhost"}
var allowedHostSuffixes = []string{".example", ".invalid", ".test", ".localhost"}

// Dotted words that are not hosts: file names by their extension, and the
// ids of the set's checklist items and templates by their first label.
var fileExtensions = []string{"md", "json", "py", "sh", "ps1", "txt", "yml", "yaml", "go", "ts", "js", "toml", "gz", "zip", "git"}

var (
	urlHost    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://([^\s/?#"'<>()\[\]` + "`" + `]+)`)
	atHost     = regexp.MustCompile(`[\w.+-]+@([A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+)`)
	ipv4       = regexp.MustCompile(`(?:^|[^\w.])(\d{1,3}(?:\.\d{1,3}){3})(?:$|[^\w.]|\.(?:[^\w]|$))`)
	dottedWord = regexp.MustCompile(`[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*\.[A-Za-z][A-Za-z0-9-]+`)
)

// Addresses a text may name: loopback and the documentation networks
// (RFC 5737).
var allowedNets = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
}

// unlistedHosts is every host-like name or IPv4 address in text that the
// allow-list does not admit. It fails closed: a dotted word is a host
// unless it is a file name or exactly one of the set's ids, so a new kind
// of word fails here until it is classified.
func unlistedHosts(text string, ids []string) []string {
	allowed := func(h string) bool {
		h = strings.ToLower(h)
		if a, err := netip.ParseAddr(h); err == nil {
			for _, n := range allowedNets {
				if n.Contains(a) {
					return true
				}
			}
			return false
		}
		if slices.Contains(allowedHosts, h) {
			return true
		}
		for _, s := range allowedHostSuffixes {
			if strings.HasSuffix(h, s) {
				return true
			}
		}
		return false
	}
	var bad []string
	add := func(h string) {
		if !allowed(h) && !slices.Contains(bad, h) {
			bad = append(bad, h)
		}
	}
	for _, m := range urlHost.FindAllStringSubmatch(text, -1) {
		h := m[1]
		if i := strings.LastIndex(h, "@"); i >= 0 {
			h = h[i+1:]
		}
		if i := strings.LastIndex(h, ":"); i >= 0 {
			h = h[:i]
		}
		add(h)
	}
	text = urlHost.ReplaceAllString(text, " ")
	for _, m := range atHost.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	text = atHost.ReplaceAllString(text, " ")
	for _, m := range ipv4.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, w := range dottedWord.FindAllString(text, -1) {
		labels := strings.Split(w, ".")
		if slices.Contains(fileExtensions, strings.ToLower(labels[len(labels)-1])) || slices.Contains(ids, w) {
			continue
		}
		add(w)
	}
	return bad
}

// setItemIDs is the ids of the items of the set's checklists (ready.plan,
// done.review): the only dotted words the check takes as the set's own.
func setItemIDs(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(referenceSet, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Checklists map[string]string `json:"checklists"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, p := range m.Checklists {
		raw, err := os.ReadFile(filepath.Join(referenceSet, "..", "..", filepath.FromSlash(p)))
		if err != nil {
			t.Fatal(err)
		}
		var c struct {
			Items []struct {
				ID string `json:"id"`
			} `json:"items"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			t.Fatal(err)
		}
		for _, it := range c.Items {
			ids = append(ids, it.ID)
		}
	}
	if len(ids) == 0 {
		t.Fatal("the set's checklists name no items")
	}
	return ids
}

func TestReferenceProcessSetNamesNoUnlistedHost(t *testing.T) {
	ids := setItemIDs(t)
	n := 0
	err := filepath.WalkDir(referenceSet, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		n++
		if bad := unlistedHosts(string(raw), ids); len(bad) > 0 {
			t.Errorf("%s names hosts that are not on the allow-list: %v", filepath.ToSlash(p), bad)
		}
		return nil
	})
	if err != nil || n == 0 {
		t.Fatalf("walking the reference set: %v (%d files)", err, n)
	}
}

// The check itself: what it refuses and what it lets through.
func TestUnlistedHostsFailsClosed(t *testing.T) {
	ids := []string{"ready.plan", "done.review"}
	for _, c := range []struct {
		text string
		want []string
	}{
		{"see https://github.com/BlackVS/aimem and https://x.example/a", nil},
		{"handbook.md, task.json, forge_gates.py, ready.plan, done.review, v0.6.0-compatible, e.g. this", nil},
		{"mail bob@team.example.invalid or git@github.com:o/r.git", nil},
		{"clone https://git.corp.lan:3000/x", []string{"git.corp.lan"}},
		{"push to ssh://deploy@build.internal.net/r", []string{"build.internal.net"}},
		{"ask ops@mail.acme.co", []string{"mail.acme.co"}},
		{"the hub at hub.office.lan serves it", []string{"hub.office.lan"}},
		{"a new word like widget.frobnicate", []string{"widget.frobnicate"}},
		// Only an exact id of the set is exempt, never a word that starts with a kind.
		{"the hub task.office.lan and done.corp.example.com and ready.planner", []string{"task.office.lan", "done.corp.example.com", "ready.planner"}},
		// A bare address is a host too, unless loopback or a documentation address.
		{"the hub is 10.0.12.7, or 10.0.12.8:8443.", []string{"10.0.12.7", "10.0.12.8"}},
		{"listen on 127.0.0.1 or 192.0.2.10; see http://203.0.113.5/x and http://172.16.0.4/y", []string{"172.16.0.4"}},
		{"a version 1.2.3 is no address", nil},
	} {
		if got := unlistedHosts(c.text, ids); !slices.Equal(got, c.want) {
			t.Errorf("%q: got %v, want %v", c.text, got, c.want)
		}
	}
}
