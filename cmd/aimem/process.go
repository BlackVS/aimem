package main

// The process selection: which Git repository, commit and manifest hold
// the handbook, checklists and templates a project's agents follow
// (docs/DESIGN-kanban-docs.md), and the files themselves, stored on the
// hub with the selection (docs/DESIGN-portable-team-context.md, increment
// 1d). `aimem process select|clear` run on the hub host as the admin:
// select reads the set from Git, or from an archive verified by its
// SHA256SUMS, and sends it with the selection, so the hub never runs Git
// and a member never needs access to the repository. `aimem process
// export` writes that archive on a machine that has the access, for a hub
// that has none. `aimem process list|refresh` on the hub host show which
// selections lack a stored set and store them (process_refresh.go).
// `aimem process show` runs on any machine and prints
// exactly what the session-start hook injects, with the same availability
// notices, so a person can see what an agent saw; in a directory with no
// hub binding on a machine with no hub, the hub host's admin shell, it
// reads the selection and the set this host stores.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	"aimem/internal/adapter"
	"aimem/internal/ident"
	"aimem/internal/process"
	"aimem/internal/processctx"
)

const processUsage = `usage: aimem process select <repo> <tag> [<manifest>] [--from-archive <file>] [--sums <file>] [--expect <tag|commit>] [-p <project>]
       aimem process select <repo> <commit> [<manifest>] --commit [--ref <branch|tag>] [...]
       aimem process default set <repo> <tag> [<manifest>]
       aimem process default show
       aimem process default clear
       aimem process export <repo> <commit> <manifest> [--ref <branch|tag>] [--out <dir>]
       aimem process clear --expect <tag|commit> [-p <project>]
       aimem process list
       aimem process refresh [-p <project>] [--from-archive <file>] [--sums <file>]
       aimem process show [--full] [--template <kind>] [-p <project>]
       aimem process pin [--json] [-p <project>]

select, clear, default, list and refresh run on the hub host (admin).
select takes a tag (or a branch): it is resolved to the commit it holds
now (git ls-remote on this host, or without git the forge's ref
advertisement over https), and the selection stores both and stays
pinned to that commit. A full 40-hex commit is taken only with --commit,
for a repository with no tag for it. The manifest defaults to
processes/aimem/manifest.json. --expect names the current selection by
its commit or its tag; re-selecting the commit and manifest already
selected needs none. select reads the set at the commit from Git (without
git, from the forge's tarball of the commit over https), or with
--from-archive from a tar.gz verified against the SHA256SUMS beside it
(--sums names another file), and stores it on the hub with the
selection. Without git, a private repository needs
AIMEM_PROCESS_FORGE_TOKEN in the hub's env file: a token the forge
accepts, sent only to the repository's host (AIMEM_PROCESS_FORGE_TOKENS,
host=token entries, gives each forge host its own). A set the hub already keeps
(the last 5 selections) is re-selected without a new import. export writes that
archive and its SHA256SUMS line on a machine with access to the
repository. list prints every project's selection and whether its set is
stored. refresh stores the set of every selection that has none (made
before the hub stored sets) from its recorded repository, commit, manifest
and ref, and reports each project; a set it cannot read from Git is
reported as needing an archive, which refresh -p <project> --from-archive
then imports. show runs anywhere and prints what the session-start hook
injects for this project; in a directory with no hub binding on a machine
with no hub configured (the hub host), it reads this host's stored
selection and set. --full prints the whole unit, even one over the
manifest's injection budget, and adds the repository URL, for the admin.
default set stores the hub's default process (resolved and imported as
select does); aimem tasks on applies it to a project with no process.
default show prints it, and default clear removes it.
pin runs where show does, with this checkout's credential, and prints the
project's current pin as an offer names it: the repository, the full
commit, the manifest, the set's digest, and the instruction digest
(sha256: and the SHA-256 of the manifest file's bytes at that commit). It
reads the hub's stored set, checks it against the selection's digest, and
needs no Git; --json prints one JSON object. A project with no selection,
a selection without a stored set, a set that does not match, or a
credential without the project grant is an error.`

func processCmd(args []string) error {
	usage := processUsage
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	switch args[0] {
	case "list":
		return processListCmd(args[1:])
	case "refresh":
		return processRefreshCmd(args[1:])
	case "default":
		return processDefaultCmd(args[1:])
	case "select", "clear", "export":
		fs := flag.NewFlagSet("process "+args[0], flag.ExitOnError)
		p := projectFlag(fs, "project id (default: current directory's project)")
		ref := fs.String("ref", "", "with a commit: the branch or tag to fetch when the server refuses fetch-by-hash")
		expect := fs.String("expect", "", "the commit or tag currently selected (compare-and-swap); empty when none or when re-selecting the same commit")
		archive := fs.String("from-archive", "", "import the set from this tar.gz instead of Git")
		sums := fs.String("sums", "", "the SHA256SUMS file that names the archive (default: SHA256SUMS beside it)")
		out := fs.String("out", ".", "export: the directory to write the archive and SHA256SUMS into")
		byCommit := fs.Bool("commit", false, "select: take a full 40-hex commit, for a repository with no tag for it")
		var pos []string
		rest := args[1:]
		for len(rest) > 0 { // flags after the positionals too
			if strings.HasPrefix(rest[0], "-") {
				fs.Parse(rest)
				rest = fs.Args()
				continue
			}
			pos = append(pos, rest[0])
			rest = rest[1:]
		}
		if args[0] == "export" {
			if len(pos) != 3 {
				return fmt.Errorf("%s", usage)
			}
			return processExport(process.Ref{Repo: pos[0], Commit: pos[1], Manifest: pos[2], Ref: *ref}, *out)
		}
		proj, err := projectOrCurrent(*p)
		if err != nil {
			return err
		}
		body := map[string]any{}
		if args[0] == "clear" {
			body["clear"] = true
			expected, err := expectedCommit(proj, *expect, nil)
			if err != nil {
				return err
			}
			body["expected_commit"] = expected
		} else {
			if len(pos) != 2 && len(pos) != 3 {
				return fmt.Errorf("%s", usage)
			}
			manifest := ""
			if len(pos) == 3 {
				manifest = pos[2]
			}
			if body, err = selectRequest(proj, pos[0], pos[1], manifest, *ref, *expect, *archive, *sums, *byCommit); err != nil {
				return err
			}
		}
		raw, _ := json.Marshal(body)
		req, err := http.NewRequest(http.MethodPut, "http://aimem/v1/projects/"+url.PathEscape(proj)+"/process", bytes.NewReader(raw))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client().Do(req)
		if err != nil {
			return fmt.Errorf("%w (is the local aimem service running?)", err)
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
		}
		fmt.Println(strings.TrimSpace(string(data)))
		if gap := hubSkillsGap(proj); gap != "" {
			fmt.Fprint(os.Stderr, "warning: "+gap)
		}
		return nil
	case "pin":
		fs := flag.NewFlagSet("process pin", flag.ExitOnError)
		p := projectFlag(fs, "project id (default: current directory's project)")
		asJSON := fs.Bool("json", false, "print one JSON object")
		fs.Parse(args[1:])
		if fs.NArg() != 0 {
			return fmt.Errorf("%s", usage)
		}
		pin, err := processctx.ReadPin(".", stateRoot(), *p)
		if err != nil {
			return err
		}
		return printPin(os.Stdout, pin, *asJSON)
	case "show":
		fs := flag.NewFlagSet("process show", flag.ExitOnError)
		p := projectFlag(fs, "project id (default: current directory's project)")
		tmpl := fs.String("template", "", "print this template from the process set instead of the bootstrap")
		full := fs.Bool("full", false, "print the whole unit regardless of the manifest's injection budget, and the repository URL")
		fs.Parse(args[1:])
		text, set, err := processShowText(".", *p, *full)
		if err != nil {
			return err
		}
		if *tmpl != "" {
			if set == nil {
				return errors.New("process set unavailable: " + strings.TrimSpace(text))
			}
			raw, ok := set.Templates[*tmpl]
			if !ok {
				return fmt.Errorf("no template %q in the process set", *tmpl)
			}
			os.Stdout.Write(raw)
			fmt.Println()
			return nil
		}
		fmt.Println(strings.TrimSpace(text))
		if *full && set != nil {
			// The admin's view, after the unit: the unit itself is what the
			// MCP tools deliver to an agent, and it never names the URL.
			fmt.Printf("\nRepository (admin view, not part of the agent's context): %s\n", set.Ref.Repo)
		}
		return nil
	}
	return fmt.Errorf("%s", usage)
}

// selectRequest is the body of PUT /v1/projects/{p}/process for selecting
// repo at tag (or, with byCommit, at a full commit) and manifest (the
// default when empty): the tag resolved to its commit, the expected
// commit for the compare-and-swap, and the set, imported unless the hub
// already keeps it.
func selectRequest(proj, repo, tag, manifest, ref, expect, archive, sums string, byCommit bool) (map[string]any, error) {
	sel := process.Ref{Repo: repo, Commit: tag, Manifest: process.DefaultManifest, Ref: ref}
	if manifest != "" {
		sel.Manifest = manifest
	}
	switch {
	case process.IsCommit(tag) && !byCommit:
		return nil, fmt.Errorf("%s is a commit; select by a tag of the repository (aimem process select <repo> <tag>), or pass --commit when the repository has no tag for it", tag)
	case byCommit && !process.IsCommit(tag):
		return nil, fmt.Errorf("--commit takes a full 40-character lowercase hex commit, not %s", tag)
	}
	if !process.IsCommit(tag) {
		if ref != "" {
			return nil, errors.New("--ref goes with a commit; a tag or branch is resolved to its commit and stored as the ref")
		}
		commit, err := resolveRef(sel.Repo, tag)
		if err != nil {
			if looksLikeCommit(tag) {
				return nil, fmt.Errorf("resolving %s: %w (a commit must be the full 40-character lowercase hex id)", tag, err)
			}
			return nil, fmt.Errorf("resolving %s: %w", tag, err)
		}
		sel.Ref, sel.Commit = tag, commit
		fmt.Fprintf(os.Stderr, "%s holds commit %s: selecting it\n", tag, commit)
	}
	if err := sel.Validate(); err != nil {
		return nil, err
	}
	expected, err := expectedCommit(proj, expect, &sel)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"expected_commit": expected, "repo": sel.Repo, "commit": sel.Commit, "manifest": sel.Manifest}
	if sel.Ref != "" {
		body["ref"] = sel.Ref
	}
	if kept := keptSetFor(proj, sel); kept != "" {
		fmt.Fprintf(os.Stderr, "set %s is already kept on this hub: re-selecting it without an import\n", kept[:12])
		body["digest"] = kept
		return body, nil
	}
	bundle, source, err := importBundle(sel, archive, sums)
	if err != nil {
		return nil, err
	}
	body["files"] = bundle.Files
	fmt.Fprintf(os.Stderr, "importing set %s (%d files, %d bytes, from %s)\n", bundle.Digest()[:12], len(bundle.Files), bundle.Bytes(), source)
	return body, nil
}

// printPin prints a process pin as one JSON object, or as labelled lines.
func printPin(w io.Writer, pin *processctx.Pin, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(pin)
	}
	_, err := fmt.Fprintf(w, "project             %s\nrepository          %s\ncommit              %s\nmanifest            %s\nset_digest          %s\ninstruction_digest  %s\n",
		pin.Project, pin.Repository, pin.Commit, pin.Manifest, pin.SetDigest, pin.InstructionDigest)
	return err
}

// looksLikeCommit reports whether s could be meant as a commit id that is
// not in the full lowercase form select takes: an abbreviation, or
// uppercase hex.
func looksLikeCommit(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, c := range strings.ToLower(s) {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// resolveRef resolves a tag or branch to its commit with git ls-remote on
// this host when it has git, and otherwise (or when git cannot) from the
// forge's ref advertisement over https, as fetchBundle reads a set. A
// repository or name no selection could hold is refused once, before
// either is asked. Tests replace it.
var resolveRef = func(repo, name string) (string, error) {
	if err := process.CheckResolvable(repo, name); err != nil {
		return "", err
	}
	gitErr := errors.New("git is not installed on this host")
	if gitPresent() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*process.FetchTimeout)
		defer cancel()
		commit, err := process.Resolve(ctx, repo, name)
		if err == nil {
			return commit, nil
		}
		gitErr = err
	}
	token, err := forgeTokenFor(repo)
	commit := ""
	if err == nil {
		commit, err = process.ResolveForge(context.Background(), forgeClient, repo, name, token)
	}
	if err != nil {
		return "", fmt.Errorf("with git: %v; from the forge: %v", gitErr, err)
	}
	return commit, nil
}

// The hub host's settings (in the env file, as every AIMEM_ setting) for
// the token a forge accepts for a process repository, sent on the forge
// tarball and ref reads and never stored: one token for every forge, and
// host=token entries (separated by spaces or commas) for hubs whose
// projects use more than one.
const (
	forgeTokenEnv  = "AIMEM_PROCESS_FORGE_TOKEN"
	forgeTokensEnv = "AIMEM_PROCESS_FORGE_TOKENS"
)

// forgeTokenFor is the token for repo's forge: the entry for its host
// (compared without case, :443 implied), else the one token, else none.
// An error names a malformed entry by its position, never its text.
func forgeTokenFor(repo string) (string, error) {
	host := ""
	if u, err := url.Parse(repo); err == nil && u.Host != "" {
		host = forgeHost(u.Host)
	}
	sep := func(r rune) bool { return r == ',' || unicode.IsSpace(r) }
	token := ""
	for i, entry := range strings.FieldsFunc(os.Getenv(forgeTokensEnv), sep) {
		h, t, ok := strings.Cut(entry, "=")
		if !ok || h == "" || t == "" || strings.Contains(h, "/") {
			return "", fmt.Errorf("%s: entry %d is not host=token (a host such as github.com, not a URL)", forgeTokensEnv, i+1)
		}
		if token == "" && host != "" && forgeHost(h) == host {
			token = t
		}
	}
	if token == "" {
		token = strings.TrimSpace(os.Getenv(forgeTokenEnv))
	}
	return token, nil
}

func forgeHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ":443") }

// expectedCommit turns --expect into the commit the hub's compare-and-swap
// needs. A full commit passes as given. A tag or branch must be the
// current selection's ref, and stands for its commit. Without --expect,
// re-selecting the same process version (next: the same commit and
// manifest as the current selection) needs no confirmation, so the
// current commit is sent; anything else sends nothing, and the hub refuses
// a change of an existing selection. A clear passes next nil.
func expectedCommit(proj, expect string, next *process.Ref) (string, error) {
	if process.IsCommit(expect) {
		return expect, nil
	}
	cur, err := currentSelection(proj)
	if err != nil {
		if expect == "" {
			return "", nil // the hub decides; a read failure here hides nothing
		}
		return "", fmt.Errorf("--expect %s: reading the current selection: %w", expect, err)
	}
	if expect == "" {
		if cur != nil && next != nil && cur.Commit == next.Commit && cur.Manifest == next.Manifest {
			return cur.Commit, nil
		}
		return "", nil
	}
	if cur == nil {
		return "", fmt.Errorf("--expect %s: project %s has no process selected; leave --expect out", expect, proj)
	}
	if cur.Ref == "" || cur.Ref != expect {
		ref := cur.Ref
		if ref == "" {
			ref = "none"
		}
		return "", fmt.Errorf("--expect %s is neither the selected commit nor the selected ref (selected: ref %s, commit %s)", expect, ref, cur.Commit)
	}
	return cur.Commit, nil
}

// keptSetFor asks the hub whether it still keeps a set for exactly this
// selection (repository, commit and manifest of an earlier selection, with
// its set among the kept ones) and returns its digest, or "" when a new
// import is needed. Any failure to ask means importing, which is safe.
func keptSetFor(proj string, sel process.Ref) string {
	resp, err := client().Get("http://aimem/v1/projects/" + url.PathEscape(proj) + "/process?history=1")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var got struct {
		History []process.Ref `json:"history"`
		Sets    []struct {
			Digest string `json:"digest"`
		} `json:"sets"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&got) != nil {
		return ""
	}
	kept := map[string]bool{}
	for _, s := range got.Sets {
		kept[s.Digest] = true
	}
	for _, h := range got.History {
		if h.Repo == sel.Repo && h.Commit == sel.Commit && h.Manifest == sel.Manifest && h.Digest != "" && kept[h.Digest] {
			return h.Digest
		}
	}
	return ""
}

// importBundle builds the set to send: from the archive when one is named
// (verified against its SHA256SUMS first), otherwise as fetchBundle reads
// it. It also says where the set came from.
func importBundle(sel process.Ref, archive, sums string) (*process.Bundle, string, error) {
	if archive != "" {
		if sums == "" {
			sums = filepath.Join(filepath.Dir(archive), "SHA256SUMS")
		}
		b, err := process.ReadArchiveFile(archive, sums, sel.Manifest)
		if err != nil {
			return nil, "", fmt.Errorf("archive %s: %w", archive, err)
		}
		return b, filepath.Base(archive), nil
	}
	return fetchBundle(sel)
}

// The sources fetchBundle reads, as seams for tests: whether this host has
// git, the Git read, and the client for the forge's tarball.
var (
	gitPresent  = func() bool { _, err := exec.LookPath("git"); return err == nil }
	fetchGit    = fetchGitBundle
	forgeClient = http.DefaultClient
)

// fetchBundle reads the set at the selection's commit from Git when this
// host has it, and otherwise (or when Git cannot read it) from the forge's
// tarball of that commit over https, so a hub host without git still reads
// a set from the forge (a private one with the forge token). It says which
// source it read.
func fetchBundle(sel process.Ref) (*process.Bundle, string, error) {
	gitErr := errors.New("git is not installed on this host")
	if gitPresent() {
		b, err := fetchGit(sel)
		if err == nil {
			return b, "Git", nil
		}
		gitErr = err
	}
	token, err := forgeTokenFor(sel.Repo)
	var b *process.Bundle
	if err == nil {
		b, err = process.FetchForgeArchive(context.Background(), forgeClient, sel, token)
	}
	if err != nil {
		return nil, "", fmt.Errorf("reading the set from Git: %v; from the forge tarball: %v", gitErr, err)
	}
	return b, "the forge tarball", nil
}

// fetchGitBundle reads the set at the selection's commit from Git, through
// the same bounded fetch a member used to make, into a directory that is
// removed afterwards.
func fetchGitBundle(sel process.Ref) (*process.Bundle, error) {
	tmp, err := os.MkdirTemp("", "aimem-process-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	ctx, cancel := context.WithTimeout(context.Background(), 3*process.FetchTimeout)
	defer cancel()
	res := process.Fetch(ctx, tmp, sel)
	if res.Set == nil {
		return nil, fmt.Errorf("%s: %v", res.Status, res.Err)
	}
	return process.BundleFromDir(res.Set.Dir, sel)
}

// processExport reads the set as fetchBundle does and writes it as
// <name>-<commit12>.tar.gz under out, with its line in out/SHA256SUMS
// (added, or replaced when the file already names that archive), so the
// pair can be carried to a hub host without access and imported there with
// --from-archive.
func processExport(sel process.Ref, out string) error {
	if err := sel.Validate(); err != nil {
		return err
	}
	b, _, err := fetchBundle(sel)
	if err != nil {
		return err
	}
	archivePath, sumsPath, err := writeExport(b, sel, out)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d files, %d bytes, set %s) and its line in %s\n", archivePath, len(b.Files), b.Bytes(), b.Digest()[:12], sumsPath)
	fmt.Printf("on the hub host: aimem process select %s %s %s --from-archive %s -p <project>\n", sel.Repo, sel.Commit, sel.Manifest, filepath.Base(archivePath))
	return nil
}

// writeExport writes the bundle's archive and its SHA256SUMS line under
// out and returns both paths.
func writeExport(b *process.Bundle, sel process.Ref, out string) (archivePath, sumsPath string, err error) {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", "", err
	}
	name := fmt.Sprintf("%s-%s.tar.gz", process.SetName(sel.Repo, sel.Manifest), sel.Commit[:12])
	var buf bytes.Buffer
	if err := process.WriteArchiveAt(&buf, b, sel.Commit); err != nil {
		return "", "", err
	}
	archivePath = filepath.Join(out, name)
	if err := os.WriteFile(archivePath, buf.Bytes(), 0o644); err != nil {
		return "", "", err
	}
	sumsPath = filepath.Join(out, "SHA256SUMS")
	var lines []string
	if old, err := os.ReadFile(sumsPath); err == nil {
		for _, l := range strings.Split(strings.TrimRight(string(old), "\n"), "\n") {
			if f := strings.Fields(l); len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
				continue
			}
			if l != "" {
				lines = append(lines, l)
			}
		}
	}
	lines = append(lines, strings.TrimRight(process.SumsLine(buf.Bytes(), name), "\n"))
	if err := os.WriteFile(sumsPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return archivePath, sumsPath, nil
}

func projectOrCurrent(p string) (string, error) {
	if p != "" {
		return p, nil
	}
	return ident.ProjectID(".")
}

// processBootstrap is processctx.Bootstrap for this process's state root.
func processBootstrap(dir, projectID string, full bool) (string, *process.Set) {
	return processctx.Bootstrap(dir, stateRoot(), projectID, full)
}

// processShowText is the bootstrap `process show` prints. A directory
// bound to a hub, or any directory on a machine with a default hub, gets
// what the session-start hook builds there: a named project is still
// resolved through that hub, where its reference lives. A directory with
// neither, such as the hub host's admin shell where select runs, reads
// the selection and the set this host's service stores, as list and
// select do. An outcome with nothing to print is an error, never an
// empty success.
func processShowText(dir, projectID string, full bool) (string, *process.Set, error) {
	if name, err := ident.ProjectHubNameStrict(dir); err == nil && name == "" {
		if _, hub := adapter.ResolveHub(stateRoot(), ""); hub == nil {
			return storedProcessBootstrap(dir, projectID, full)
		}
	}
	r := processctx.Load(dir, stateRoot(), projectID)
	text, set := r.Bootstrap(dir, full)
	if strings.TrimSpace(text) == "" {
		if err := r.Problem(); err != nil {
			return "", nil, err
		}
		return "", nil, fmt.Errorf("process context for project %s: nothing to show", r.Project)
	}
	return text, set, nil
}

// storedProcessBootstrap builds the bootstrap from this host's service:
// the project's selection and its stored set, verified against the
// selection's digest, rendered as a member of the project receives it.
func storedProcessBootstrap(dir, projectID string, full bool) (string, *process.Set, error) {
	proj, err := projectOrCurrent(projectID)
	if err != nil {
		return "", nil, err
	}
	unbound := func(format string, a ...any) error {
		return fmt.Errorf("this directory has no hub binding and this machine has no hub configured, so show read this host's store: "+format+"; in a checkout bound to the project's hub, show prints what a session there receives", a...)
	}
	var sel struct {
		Current *process.Ref `json:"current"`
	}
	if err := localJSON(http.MethodGet, "/v1/projects/"+url.PathEscape(proj)+"/process", nil, &sel); err != nil {
		return "", nil, unbound("project %s: %v", proj, err)
	}
	if sel.Current == nil {
		return "", nil, unbound("project %s has no process selected (aimem process select on the hub host)", proj)
	}
	ref := *sel.Current
	if ref.Digest == "" {
		return "", nil, unbound("project %s's selection has no stored set (aimem process refresh on the hub host)", proj)
	}
	bundle, err := storedSet(proj, ref)
	if err != nil {
		return "", nil, unbound("project %s: %v", proj, err)
	}
	set, err := bundle.Set(ref)
	if err != nil {
		return "", nil, unbound("project %s: the stored set does not load: %v", proj, err)
	}
	home, _ := os.UserHomeDir()
	if full {
		return process.BootstrapFull(set, proj, process.SkillStateOn(dir, home)), set, nil
	}
	text, err := process.Bootstrap(set, proj, process.SkillStateOn(dir, home))
	if err != nil {
		return "process context unavailable: " + err.Error(), set, nil
	}
	return text, set, nil
}

// storedSet reads the stored set of the selection ref from this host's
// service and checks that its files digest to the selection's digest.
func storedSet(proj string, ref process.Ref) (*process.Bundle, error) {
	resp, err := client().Get("http://aimem/v1/projects/" + url.PathEscape(proj) + "/process/set")
	if err != nil {
		return nil, fmt.Errorf("%w (is the local aimem service running?)", err)
	}
	defer resp.Body.Close()
	const max = 2 * process.MaxSetBytes // base64 in JSON, with room for the envelope
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return nil, fmt.Errorf("the stored set's answer is larger than %d bytes", max)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reading the stored set: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var got struct {
		Selection struct {
			Digest string `json:"digest"`
		} `json:"selection"`
		Set process.Bundle `json:"set"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		return nil, fmt.Errorf("the stored set's answer is not a set: %w", err)
	}
	if got.Selection.Digest != ref.Digest || got.Set.Digest() != ref.Digest {
		return nil, fmt.Errorf("the stored set does not match the selection's digest %s", ref.Digest)
	}
	return &got.Set, nil
}

// processNotice is the session-start hook's slice: the bootstrap, or the
// availability notice, as its own block after the handoff.
func processNotice() string {
	text, _ := processBootstrap(".", "", false)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return "\n\n--- Process context (docs/DESIGN-kanban-docs.md) ---\n" + strings.TrimSpace(text)
}
