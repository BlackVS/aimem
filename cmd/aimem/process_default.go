package main

// `aimem process default set|show|clear`: the hub's default process, which
// `aimem tasks on` applies to a project that has none. set resolves the
// tag and imports the set exactly as `process select` does, so a tag the
// repository lacks, or a set that does not validate, is refused before
// anything is stored; the hub keeps the tag, the commit it held, the
// manifest, the set's digest and the set itself
// (internal/server/process_default.go), so `tasks on` applies it without
// Git. With --from-archive, set reads nothing over the network: the set
// is the verified archive's, and the commit is the one the archive
// records.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"aimem/internal/process"
)

const processDefaultUsage = `usage: aimem process default set <repo> <tag> [<manifest>] [--from-archive <file>] [--sums <file>]
       aimem process default show
       aimem process default clear`

func processDefaultCmd(args []string) error {
	if len(args) == 0 {
		return errors.New(processDefaultUsage)
	}
	switch args[0] {
	case "set":
		fs := flag.NewFlagSet("process default set", flag.ExitOnError)
		archive := fs.String("from-archive", "", "read the set from this tar.gz, which records its commit, with no network")
		sums := fs.String("sums", "", "the SHA256SUMS file that names the archive (default: SHA256SUMS beside it)")
		var pos []string
		for rest := args[1:]; len(rest) > 0; { // flags after the positionals too
			if strings.HasPrefix(rest[0], "-") {
				fs.Parse(rest)
				rest = fs.Args()
				continue
			}
			pos = append(pos, rest[0])
			rest = rest[1:]
		}
		if len(pos) != 2 && len(pos) != 3 {
			return errors.New(processDefaultUsage)
		}
		sel := process.Ref{Repo: pos[0], Ref: pos[1], Manifest: process.DefaultManifest}
		if len(pos) == 3 {
			sel.Manifest = pos[2]
		}
		if process.IsCommit(sel.Ref) {
			return fmt.Errorf("%s is a commit; a default names a tag of the repository", sel.Ref)
		}
		var bundle *process.Bundle
		var source string
		var err error
		if *archive != "" {
			bundle, sel.Commit, err = archiveDefaultSet(sel, *archive, *sums)
			source = filepath.Base(*archive)
		} else if sel.Commit, err = resolveRef(sel.Repo, sel.Ref); err != nil {
			err = fmt.Errorf("resolving %s: %w", sel.Ref, err)
		}
		if err != nil {
			return err
		}
		if err := sel.Validate(); err != nil {
			return err
		}
		if bundle == nil {
			if bundle, source, err = importBundle(sel, "", ""); err != nil {
				return err
			}
		}
		sel.Digest = bundle.Digest()
		fmt.Fprintf(os.Stderr, "%s holds commit %s; set %s read from %s\n", sel.Ref, sel.Commit, sel.Digest[:12], source)
		raw, _ := json.Marshal(struct {
			process.Ref
			Files map[string][]byte `json:"files"`
		}{sel, bundle.Files})
		var got processDefaultState
		if err := localJSON(http.MethodPut, "/v1/process-default", raw, &got); err != nil {
			return err
		}
		fmt.Print(processDefaultText(got.Default, got.SetKept))
		return nil
	case "show":
		if len(args) != 1 {
			return errors.New(processDefaultUsage)
		}
		got, err := hubProcessDefaultState()
		if err != nil {
			return err
		}
		if got.Default == nil {
			fmt.Println("no default process is set (aimem process default set <repo> <tag> [<manifest>])")
			return nil
		}
		fmt.Print(processDefaultText(got.Default, got.SetKept))
		return nil
	case "clear":
		if len(args) != 1 {
			return errors.New(processDefaultUsage)
		}
		var got map[string]any
		if err := localJSON(http.MethodDelete, "/v1/process-default", nil, &got); err != nil {
			return err
		}
		fmt.Println("default process cleared; projects that applied it keep their selections")
		return nil
	}
	return errors.New(processDefaultUsage)
}

// archiveDefaultSet reads the set of a default from an archive verified
// against its SHA256SUMS, and the commit the archive records: what
// `aimem process export` and a forge's source tarball write. It reads
// nothing else, so a hub host with no network sets its default.
func archiveDefaultSet(sel process.Ref, archive, sums string) (*process.Bundle, string, error) {
	bundle, _, err := importBundle(sel, archive, sums)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		return nil, "", err
	}
	commit, err := process.ArchiveCommit(data)
	if err != nil {
		return nil, "", fmt.Errorf("archive %s: %w", archive, err)
	}
	if commit == "" {
		return nil, "", fmt.Errorf("archive %s records no commit; write it with aimem process export <repo> <commit> <manifest> on a machine with access to the repository, or use the forge's source tarball of the tag's commit", archive)
	}
	return bundle, commit, nil
}

// processDefaultText is what set and show print.
func processDefaultText(d *process.Ref, setKept bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "default process %s\n", d.Name)
	fmt.Fprintf(&b, "  repository  %s\n", d.Repo)
	fmt.Fprintf(&b, "  tag         %s\n", d.Ref)
	fmt.Fprintf(&b, "  commit      %s\n", d.Commit)
	fmt.Fprintf(&b, "  manifest    %s\n", d.Manifest)
	if d.SelectedAt != "" {
		fmt.Fprintf(&b, "  set         %s by %s\n", d.SelectedAt, d.SelectedBy)
	}
	if setKept {
		fmt.Fprintf(&b, "  files       kept on the hub (set %s)\n", d.Digest[:12])
	} else {
		fmt.Fprintf(&b, "  files       not kept on the hub: tasks on reads them from the repository (set the default again to keep them)\n")
	}
	return b.String()
}

// processDefaultState is the hub's answer about its default.
type processDefaultState struct {
	Default *process.Ref `json:"default"`
	SetKept bool         `json:"set_kept"`
}

// hubProcessDefault reads the hub's default, nil when none is set or the
// hub is older than the route (it answers 404 or 405), so `tasks on`
// against such a hub behaves as before.
func hubProcessDefault() (*process.Ref, error) {
	got, err := hubProcessDefaultState()
	return got.Default, err
}

// hubProcessDefaultState is hubProcessDefault with whether the hub keeps
// the default's set.
func hubProcessDefaultState() (processDefaultState, error) {
	var got processDefaultState
	resp, err := client().Get("http://aimem/v1/process-default")
	if err != nil {
		return got, fmt.Errorf("%w (is the local aimem service running?)", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return got, nil
	default:
		return got, fmt.Errorf("reading the hub's default process: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if err := json.Unmarshal(data, &got); err != nil {
		return processDefaultState{}, fmt.Errorf("reading the hub's default process: %w", err)
	}
	return got, nil
}

// hubProcessDefaultSet reads the set the hub keeps for the default def
// and checks it is def's: a set that validates, of def's manifest, with
// def's digest. nil when the hub keeps none (a default stored by an older
// command, or a hub older than the route).
func hubProcessDefaultSet(def *process.Ref) (*process.Bundle, error) {
	resp, err := client().Get("http://aimem/v1/process-default/set")
	if err != nil {
		return nil, fmt.Errorf("%w (is the local aimem service running?)", err)
	}
	defer resp.Body.Close()
	const max = 2 * process.MaxSetBytes // base64 in JSON, with room for the envelope
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound, resp.StatusCode == http.StatusMethodNotAllowed:
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("reading the default's kept set: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	case len(data) > max:
		return nil, fmt.Errorf("the default's kept set is larger than %d bytes", max)
	}
	var got struct {
		Set process.Bundle `json:"set"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		return nil, fmt.Errorf("the default's kept set is not a set: %w", err)
	}
	if err := got.Set.Validate(); err != nil {
		return nil, fmt.Errorf("the default's kept set: %w", err)
	}
	if got.Set.Manifest != def.Manifest || got.Set.Digest() != def.Digest {
		return nil, fmt.Errorf("the hub's kept set is not the default's set %s", def.Digest[:12])
	}
	return &got.Set, nil
}

// applyProcessDefault selects the hub's default for a project that has no
// process, as `tasks on` does: the default's commit with its tag as ref,
// and the set the hub keeps for it, or, when the hub keeps none, the set
// read again from that commit, either checked against the default's
// digest; sent with expected commit "" so a selection made meanwhile wins.
// It returns the line to print, or "" when no default is set.
func applyProcessDefault(proj string) (string, error) {
	def, err := hubProcessDefault()
	if err != nil || def == nil {
		return "", err
	}
	sel := process.Ref{Repo: def.Repo, Ref: def.Ref, Commit: def.Commit, Manifest: def.Manifest}
	body := map[string]any{"expected_commit": "", "repo": sel.Repo, "ref": sel.Ref, "commit": sel.Commit, "manifest": sel.Manifest}
	bundle, err := hubProcessDefaultSet(def)
	if err == nil && bundle == nil {
		bundle, _, err = importBundle(sel, "", "")
	}
	if err != nil {
		return "", fmt.Errorf("the default process %s (tag %s): %w", def.Name, def.Ref, err)
	}
	if d := bundle.Digest(); d != def.Digest {
		return "", fmt.Errorf("the default process %s (tag %s) now reads as set %s, not the %s it was set with; set the default again", def.Name, def.Ref, d[:12], def.Digest[:12])
	}
	body["files"] = bundle.Files
	raw, _ := json.Marshal(body)
	var got struct {
		Current *process.Ref `json:"current"`
	}
	if err := localJSON(http.MethodPut, "/v1/projects/"+url.PathEscape(proj)+"/process", raw, &got); err != nil {
		return "", fmt.Errorf("selecting the default process %s: %w", def.Name, err)
	}
	if got.Current == nil {
		return "", errors.New("selecting the default process: the hub answered no selection")
	}
	return fmt.Sprintf("applied the hub's default process: %s (tag %s, commit %s)\n", got.Current.Name, got.Current.Ref, got.Current.Commit), nil
}
