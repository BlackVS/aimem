package main

// `aimem process default set|show|clear`: the hub's default process, which
// `aimem tasks on` applies to a project that has none. set resolves the
// tag and imports the set exactly as `process select` does, so a tag the
// repository lacks, or a set that does not validate, is refused before
// anything is stored; the hub keeps the tag, the commit it held, the
// manifest and the set's digest (internal/server/process_default.go).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"aimem/internal/process"
)

const processDefaultUsage = `usage: aimem process default set <repo> <tag> [<manifest>]
       aimem process default show
       aimem process default clear`

func processDefaultCmd(args []string) error {
	if len(args) == 0 {
		return errors.New(processDefaultUsage)
	}
	switch args[0] {
	case "set":
		if len(args) != 3 && len(args) != 4 {
			return errors.New(processDefaultUsage)
		}
		sel := process.Ref{Repo: args[1], Manifest: process.DefaultManifest}
		if len(args) == 4 {
			sel.Manifest = args[3]
		}
		if process.IsCommit(args[2]) {
			return fmt.Errorf("%s is a commit; a default names a tag of the repository", args[2])
		}
		commit, err := resolveRef(sel.Repo, args[2])
		if err != nil {
			return fmt.Errorf("resolving %s: %w", args[2], err)
		}
		sel.Ref, sel.Commit = args[2], commit
		if err := sel.Validate(); err != nil {
			return err
		}
		bundle, source, err := importBundle(sel, "", "")
		if err != nil {
			return err
		}
		sel.Digest = bundle.Digest()
		fmt.Fprintf(os.Stderr, "%s holds commit %s; set %s read from %s\n", sel.Ref, sel.Commit, sel.Digest[:12], source)
		raw, _ := json.Marshal(sel)
		var got struct {
			Default process.Ref `json:"default"`
		}
		if err := localJSON(http.MethodPut, "/v1/process-default", raw, &got); err != nil {
			return err
		}
		fmt.Print(processDefaultText(&got.Default))
		return nil
	case "show":
		if len(args) != 1 {
			return errors.New(processDefaultUsage)
		}
		def, err := hubProcessDefault()
		if err != nil {
			return err
		}
		if def == nil {
			fmt.Println("no default process is set (aimem process default set <repo> <tag> [<manifest>])")
			return nil
		}
		fmt.Print(processDefaultText(def))
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

// processDefaultText is what set and show print.
func processDefaultText(d *process.Ref) string {
	var b strings.Builder
	fmt.Fprintf(&b, "default process %s\n", d.Name)
	fmt.Fprintf(&b, "  repository  %s\n", d.Repo)
	fmt.Fprintf(&b, "  tag         %s\n", d.Ref)
	fmt.Fprintf(&b, "  commit      %s\n", d.Commit)
	fmt.Fprintf(&b, "  manifest    %s\n", d.Manifest)
	if d.SelectedAt != "" {
		fmt.Fprintf(&b, "  set         %s by %s\n", d.SelectedAt, d.SelectedBy)
	}
	return b.String()
}

// hubProcessDefault reads the hub's default, nil when none is set or the
// hub is older than the route (it answers 404 or 405), so `tasks on`
// against such a hub behaves as before.
func hubProcessDefault() (*process.Ref, error) {
	resp, err := client().Get("http://aimem/v1/process-default")
	if err != nil {
		return nil, fmt.Errorf("%w (is the local aimem service running?)", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return nil, nil
	default:
		return nil, fmt.Errorf("reading the hub's default process: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var got struct {
		Default *process.Ref `json:"default"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		return nil, fmt.Errorf("reading the hub's default process: %w", err)
	}
	return got.Default, nil
}

// applyProcessDefault selects the hub's default for a project that has no
// process, as `tasks on` does: the default's commit with its tag as ref,
// the set read again from that commit and checked against the default's
// digest, sent with expected commit "" so a selection made meanwhile wins.
// It returns the line to print, or "" when no default is set.
func applyProcessDefault(proj string) (string, error) {
	def, err := hubProcessDefault()
	if err != nil || def == nil {
		return "", err
	}
	sel := process.Ref{Repo: def.Repo, Ref: def.Ref, Commit: def.Commit, Manifest: def.Manifest}
	body := map[string]any{"expected_commit": "", "repo": sel.Repo, "ref": sel.Ref, "commit": sel.Commit, "manifest": sel.Manifest}
	bundle, _, err := importBundle(sel, "", "")
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
