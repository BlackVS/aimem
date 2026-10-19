package main

// `aimem process list` and `aimem process refresh` on the hub host. A
// selection made before the hub stored sets (before 0.10.7) names its
// repository, commit, manifest and ref but carries no set, and a member
// without Git access is not ready for its project until the set is
// stored. refresh stores it from those recorded values, with the same
// compare-and-swap a re-select makes, so an upgrade needs no manual step;
// install-hub.sh runs it after an upgrade. list shows every selection and
// whether its set is stored.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"aimem/internal/process"
	"aimem/internal/store"
)

// projectSelection is one project's current selection.
type projectSelection struct {
	Project string
	Ref     process.Ref
}

// processSelections returns the current selection of the named project,
// or of every ordinary project on this hub that has one, in the hub's
// project order.
func processSelections(only string) ([]projectSelection, error) {
	ids := []string{only}
	if only == "" {
		var all struct {
			Projects []string `json:"projects"`
		}
		if err := localJSON(http.MethodGet, "/v1/projects", nil, &all); err != nil {
			return nil, err
		}
		ids = all.Projects
	}
	var out []projectSelection
	for _, p := range ids {
		if store.IsReservedProject(p) {
			continue
		}
		ref, err := currentSelection(p)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", p, err)
		}
		if ref == nil {
			if only != "" {
				return nil, fmt.Errorf("project %s has no process selected", p)
			}
			continue
		}
		out = append(out, projectSelection{Project: p, Ref: *ref})
	}
	return out, nil
}

// currentSelection reads a project's selection; nil when it has none.
func currentSelection(proj string) (*process.Ref, error) {
	resp, err := client().Get("http://aimem/v1/projects/" + url.PathEscape(proj) + "/process")
	if err != nil {
		return nil, fmt.Errorf("%w (is the local aimem service running?)", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var got struct {
		Current *process.Ref `json:"current"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		return nil, err
	}
	return got.Current, nil
}

func processListCmd(args []string) error {
	fs := flag.NewFlagSet("process list", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 0 {
		return errors.New(processUsage)
	}
	sels, err := processSelections("")
	if err != nil {
		return err
	}
	if len(sels) == 0 {
		fmt.Println("no project on this hub has a process selected")
		return nil
	}
	width := len("PROJECT")
	for _, s := range sels {
		width = max(width, len(s.Project))
	}
	fmt.Printf("%-*s  %-12s  %-40s  %s\n", width, "PROJECT", "SET", "COMMIT", "REF  MANIFEST")
	missing := 0
	for _, s := range sels {
		set := "stored"
		if s.Ref.Digest == "" {
			set, missing = "not stored", missing+1
		}
		ref := s.Ref.Ref
		if ref == "" {
			ref = "-"
		}
		fmt.Printf("%-*s  %-12s  %-40s  %s  %s\n", width, s.Project, set, s.Ref.Commit, ref, s.Ref.Manifest)
	}
	if missing > 0 {
		fmt.Printf("\n%d selection(s) without a stored set: run `aimem process refresh` on this host.\n", missing)
	}
	return nil
}

func processRefreshCmd(args []string) error {
	fs := flag.NewFlagSet("process refresh", flag.ExitOnError)
	p := projectFlag(fs, "refresh this project only (default: every project with a selection)")
	archive := fs.String("from-archive", "", "import the set from this tar.gz instead of Git (needs -p)")
	sums := fs.String("sums", "", "the SHA256SUMS file that names the archive (default: SHA256SUMS beside it)")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return errors.New(processUsage)
	}
	if *archive != "" && *p == "" {
		return errors.New("--from-archive holds one selection's set: name its project with -p")
	}
	sels, err := processSelections(*p)
	if err != nil {
		return err
	}
	var refreshed, stored, failed int
	for _, s := range sels {
		line, outcome := refreshOne(s, *archive, *sums)
		fmt.Printf("%s: %s\n", s.Project, line)
		switch outcome {
		case refreshDone:
			refreshed++
		case refreshStored:
			stored++
		default:
			failed++
		}
	}
	fmt.Printf("process refresh: %d refreshed, %d already stored, %d need attention\n", refreshed, stored, failed)
	return nil
}

type refreshOutcome int

const (
	refreshDone refreshOutcome = iota
	refreshStored
	refreshFailed
)

// refreshOne stores the set of one selection that has none, from the
// archive when one is named, else from Git at the recorded commit, and
// says what it did. A set it cannot read is reported with the commands
// that supply it, never as a failure of the whole run.
func refreshOne(s projectSelection, archive, sums string) (string, refreshOutcome) {
	sel := s.Ref.Key()
	if s.Ref.Digest != "" {
		return fmt.Sprintf("already stored (set %s, commit %s)", s.Ref.Digest[:12], sel.Commit[:12]), refreshStored
	}
	if err := sel.Validate(); err != nil {
		return "the recorded selection is not valid (" + err.Error() + "); select it again with aimem process select", refreshFailed
	}
	body := map[string]any{"expected_commit": sel.Commit, "repo": sel.Repo, "commit": sel.Commit, "manifest": sel.Manifest}
	if sel.Ref != "" {
		body["ref"] = sel.Ref
	}
	var from string
	if kept := keptSetFor(s.Project, sel); kept != "" {
		body["digest"] = kept
		from = "a set this hub keeps"
	} else {
		if archive != "" && !strings.Contains(filepath.Base(archive), sel.Commit[:12]) {
			return fmt.Sprintf("the archive %s is not named for commit %s (aimem process export names it <set>-%s.tar.gz); nothing stored", filepath.Base(archive), sel.Commit[:12], sel.Commit[:12]), refreshFailed
		}
		b, err := importBundle(sel, archive, sums)
		if err != nil {
			export := fmt.Sprintf("aimem process export %s %s %s", sel.Repo, sel.Commit, sel.Manifest)
			if sel.Ref != "" {
				export += " --ref " + sel.Ref
			}
			return fmt.Sprintf("needs an archive: %v. On a machine with access: %s; then here: aimem process refresh -p %s --from-archive <file>", err, export, s.Project), refreshFailed
		}
		body["files"] = b.Files
		from = fmt.Sprintf("%d files", len(b.Files))
		if archive != "" {
			from += " from " + filepath.Base(archive)
		}
	}
	raw, _ := json.Marshal(body)
	var out struct {
		Current process.Ref `json:"current"`
	}
	if err := localJSON(http.MethodPut, "/v1/projects/"+url.PathEscape(s.Project)+"/process", raw, &out); err != nil {
		return "not stored: " + err.Error(), refreshFailed
	}
	if out.Current.Digest == "" {
		return "not stored: the hub answered without a set", refreshFailed
	}
	return fmt.Sprintf("refreshed (set %s, commit %s, %s)", out.Current.Digest[:12], sel.Commit[:12], from), refreshDone
}
