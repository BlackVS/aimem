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
// notices, so a person can see what an agent saw.

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

	"aimem/internal/ident"
	"aimem/internal/process"
	"aimem/internal/processctx"
)

const processUsage = `usage: aimem process select <repo> <tag|commit> [<manifest>] [--ref <branch|tag>] [--from-archive <file>] [--sums <file>] [--expect <tag|commit>] [-p <project>]
       aimem process export <repo> <commit> <manifest> [--ref <branch|tag>] [--out <dir>]
       aimem process clear --expect <tag|commit> [-p <project>]
       aimem process list
       aimem process refresh [-p <project>] [--from-archive <file>] [--sums <file>]
       aimem process show [--full] [--template <kind>] [-p <project>]

select, clear, list and refresh run on the hub host (admin). select takes
a tag (or a branch) or a full commit: a tag is resolved to the commit it
holds now (git ls-remote on this host, or without git the forge's ref
advertisement over https), and the selection stores both and stays
pinned to that commit. The manifest defaults to
processes/aimem/manifest.json. --expect names the current selection by
its commit or its tag; re-selecting the commit and manifest already
selected needs none. select reads the set at the commit from Git (without
git, from the forge's tarball of the commit over https), or with
--from-archive from a tar.gz verified against the SHA256SUMS beside it
(--sums names another file), and stores it on the hub with the
selection. Without git, a private repository needs
AIMEM_PROCESS_FORGE_TOKEN in the hub's env file: a token the forge
accepts, sent only to the repository's host. A set the hub already keeps
(the last 5 selections) is re-selected without a new import. export writes that
archive and its SHA256SUMS line on a machine with access to the
repository. list prints every project's selection and whether its set is
stored. refresh stores the set of every selection that has none (made
before the hub stored sets) from its recorded repository, commit, manifest
and ref, and reports each project; a set it cannot read from Git is
reported as needing an archive, which refresh -p <project> --from-archive
then imports. show runs anywhere and prints what the session-start hook
injects for this project; --full adds the repository URL, for the admin.`

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
	case "select", "clear", "export":
		fs := flag.NewFlagSet("process "+args[0], flag.ExitOnError)
		p := projectFlag(fs, "project id (default: current directory's project)")
		ref := fs.String("ref", "", "with a commit: the branch or tag to fetch when the server refuses fetch-by-hash")
		expect := fs.String("expect", "", "the commit or tag currently selected (compare-and-swap); empty when none or when re-selecting the same commit")
		archive := fs.String("from-archive", "", "import the set from this tar.gz instead of Git")
		sums := fs.String("sums", "", "the SHA256SUMS file that names the archive (default: SHA256SUMS beside it)")
		out := fs.String("out", ".", "export: the directory to write the archive and SHA256SUMS into")
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
			sel := process.Ref{Repo: pos[0], Commit: pos[1], Manifest: process.DefaultManifest, Ref: *ref}
			if len(pos) == 3 {
				sel.Manifest = pos[2]
			}
			if !process.IsCommit(pos[1]) {
				if *ref != "" {
					return errors.New("--ref goes with a commit; a tag or branch is resolved to its commit and stored as the ref")
				}
				commit, err := resolveRef(sel.Repo, pos[1])
				if err != nil {
					if looksLikeCommit(pos[1]) {
						return fmt.Errorf("resolving %s: %w (a commit must be the full 40-character lowercase hex id)", pos[1], err)
					}
					return fmt.Errorf("resolving %s: %w", pos[1], err)
				}
				sel.Ref, sel.Commit = pos[1], commit
				fmt.Fprintf(os.Stderr, "%s holds commit %s: selecting it\n", pos[1], commit)
			}
			if err := sel.Validate(); err != nil {
				return err
			}
			expected, err := expectedCommit(proj, *expect, &sel)
			if err != nil {
				return err
			}
			body["expected_commit"] = expected
			body["repo"], body["commit"], body["manifest"] = sel.Repo, sel.Commit, sel.Manifest
			if sel.Ref != "" {
				body["ref"] = sel.Ref
			}
			if kept := keptSetFor(proj, sel); kept != "" {
				fmt.Fprintf(os.Stderr, "set %s is already kept on this hub: re-selecting it without an import\n", kept[:12])
				body["digest"] = kept
			} else {
				bundle, source, err := importBundle(sel, *archive, *sums)
				if err != nil {
					return err
				}
				body["files"] = bundle.Files
				fmt.Fprintf(os.Stderr, "importing set %s (%d files, %d bytes, from %s)\n", bundle.Digest()[:12], len(bundle.Files), bundle.Bytes(), source)
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
		return nil
	case "show":
		fs := flag.NewFlagSet("process show", flag.ExitOnError)
		p := projectFlag(fs, "project id (default: current directory's project)")
		tmpl := fs.String("template", "", "print this template from the process set instead of the bootstrap")
		full := fs.Bool("full", false, "print the whole unit regardless of the manifest's injection budget, and the repository URL")
		fs.Parse(args[1:])
		// A named project is still resolved through this directory's hub
		// binding: the reference lives on the hub the project is bound to.
		text, set := processBootstrap(".", *p, *full)
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
	commit, err := process.ResolveForge(context.Background(), forgeClient, repo, name, forgeToken())
	if err != nil {
		return "", fmt.Errorf("with git: %v; from the forge: %v", gitErr, err)
	}
	return commit, nil
}

// forgeTokenEnv names the hub host's setting (in the env file, as every
// AIMEM_ setting) for a token the forge accepts for the process
// repository: sent on the forge tarball and ref reads, never stored.
const forgeTokenEnv = "AIMEM_PROCESS_FORGE_TOKEN"

func forgeToken() string { return strings.TrimSpace(os.Getenv(forgeTokenEnv)) }

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
	b, err := process.FetchForgeArchive(context.Background(), forgeClient, sel, forgeToken())
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
	if err := process.WriteArchive(&buf, b); err != nil {
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

// processNotice is the session-start hook's slice: the bootstrap, or the
// availability notice, as its own block after the handoff.
func processNotice() string {
	text, _ := processBootstrap(".", "", false)
	if strings.TrimSpace(text) == "" {
		return ""
	}
	return "\n\n--- Process context (docs/DESIGN-kanban-docs.md) ---\n" + strings.TrimSpace(text)
}
