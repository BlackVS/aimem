package main

// `aimem project <verb>`: a project's properties on the hub
// (docs/DESIGN-AIFORGE-PILOT-1.md §1, §5). `repo set|clear` and `show` run
// on the hub host as the admin, through the local service, like `process
// select`. `list`, `id` and `drop` are the namespace's forms of `projects`,
// `project-id` and `drop-project`, which keep working as they are.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// projectFlag registers --project with -p as its alias on one variable, so
// every project-taking command spells the long form the same way.
func projectFlag(fs *flag.FlagSet, usage string) *string {
	p := fs.String("project", "", usage)
	fs.StringVar(p, "p", "", "alias of --project")
	return p
}

const projectUsage = `usage: aimem project repo set --project <p> --kind github|gitea|gitlab --url <clone URL> [--access write|read]
       aimem project repo clear --project <p>
       aimem project show --project <p>
       aimem project list | id [dir] | drop --project <p> --yes

repo set|clear and show run on the hub host (admin). The repository is the
one repository the project's work happens in: --kind is the forge's API
dialect, the host of --url names the credential a member needs, and
--access is what members need (write, the default, or read). The hub
stores no default branch: the forge owns it.

Example:
  aimem project repo set --project example --kind github --url https://github.com/example/example.git
  aimem project show --project example`

func projectNamespaceCmd(args []string) error {
	if len(args) == 0 {
		return errors.New(projectUsage)
	}
	switch args[0] {
	case "list":
		return getJSON("/v1/projects")
	case "id":
		return projectID(args[1:])
	case "drop":
		return dropProjectCmd(args[1:])
	case "show":
		return projectShowCmd(args[1:])
	case "repo":
		if len(args) > 1 && (args[1] == "set" || args[1] == "clear") {
			return projectRepoCmd(args[1], args[2:])
		}
	}
	return errors.New(projectUsage)
}

func projectRepoCmd(verb string, args []string) error {
	fs := flag.NewFlagSet("project repo "+verb, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(fs.Output(), projectUsage) }
	p := projectFlag(fs, "project id (required)")
	kind := fs.String("kind", "", "forge API dialect: github, gitea or gitlab")
	cloneURL := fs.String("url", "", "clone URL: https://host/path, ssh://host/path or user@host:path")
	accessLevel := fs.String("access", "", "what members need: write (default) or read")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *p == "" || fs.NArg() != 0 {
		return errors.New(projectUsage)
	}
	body := map[string]any{"clear": true}
	if verb == "set" {
		if *kind == "" || *cloneURL == "" {
			return errors.New(projectUsage)
		}
		body = map[string]any{"kind": *kind, "url": *cloneURL}
		if *accessLevel != "" {
			body["access"] = *accessLevel
		}
	} else if *kind != "" || *cloneURL != "" || *accessLevel != "" {
		return errors.New("repo clear takes only --project")
	}
	raw, _ := json.Marshal(body)
	var out struct {
		Action     string          `json:"action"`
		Repository *repositoryJSON `json:"repository"`
		Previous   *repositoryJSON `json:"previous"`
	}
	if err := localJSON(http.MethodPut, "/v1/projects/"+url.PathEscape(*p)+"/repository", raw, &out); err != nil {
		return err
	}
	fmt.Printf("%s for project %q\n", out.Action, *p)
	fmt.Printf("  previous: %s\n", out.Previous.line())
	fmt.Printf("  now:      %s\n", out.Repository.line())
	return nil
}

type repositoryJSON struct {
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	Host   string `json:"host"`
	Access string `json:"access"`
	SetAt  string `json:"set_at"`
	SetBy  string `json:"set_by"`
}

func (r *repositoryJSON) line() string {
	if r == nil {
		return "(none)"
	}
	return fmt.Sprintf("%s %s (host %s, access %s)", r.Kind, r.URL, r.Host, r.Access)
}

// projectShowCmd prints what the hub holds for a project: its repository,
// its process pin and who is granted it, with names beside IDs.
func projectShowCmd(args []string) error {
	fs := flag.NewFlagSet("project show", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintln(fs.Output(), projectUsage) }
	p := projectFlag(fs, "project id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *p == "" || fs.NArg() != 0 {
		return errors.New(projectUsage)
	}
	base := "/v1/projects/" + url.PathEscape(*p)
	var repo struct {
		Repository *repositoryJSON `json:"repository"`
	}
	if err := localJSON(http.MethodGet, base+"/repository", nil, &repo); err != nil {
		return err
	}
	var proc struct {
		Current *struct {
			Repo       string `json:"repo"`
			Commit     string `json:"commit"`
			Manifest   string `json:"manifest"`
			Ref        string `json:"ref"`
			SelectedAt string `json:"selected_at"`
			SelectedBy string `json:"selected_by"`
		} `json:"current"`
	}
	// ?history=1 answers 200 with a null current when nothing is selected.
	if err := localJSON(http.MethodGet, base+"/process?history=1", nil, &proc); err != nil {
		return err
	}
	var grants struct {
		Instance string `json:"instance"`
		Grants   []struct {
			Kind      string `json:"kind"`
			ID        string `json:"id"`
			Name      string `json:"name"`
			Disabled  bool   `json:"disabled"`
			ServiceID string `json:"service_id"`
			TeamID    string `json:"team_id"`
		} `json:"grants"`
	}
	if err := localJSON(http.MethodGet, base+"/grants", nil, &grants); err != nil {
		return err
	}
	w := os.Stdout
	fmt.Fprintf(w, "project %s\n", *p)
	fmt.Fprintf(w, "repository: %s\n", repo.Repository.line())
	if r := repo.Repository; r != nil && r.SetBy != "" {
		fmt.Fprintf(w, "  set %s by %s\n", r.SetAt, r.SetBy)
	}
	if c := proc.Current; c != nil {
		fmt.Fprintf(w, "process:    %s %s %s", c.Repo, c.Commit, c.Manifest)
		if c.Ref != "" {
			fmt.Fprintf(w, " (ref %s)", c.Ref)
		}
		fmt.Fprintf(w, "\n  selected %s by %s\n", c.SelectedAt, c.SelectedBy)
	} else {
		fmt.Fprintln(w, "process:    (none selected)")
	}
	if len(grants.Grants) == 0 {
		fmt.Fprintln(w, "grants:     (none)")
		return nil
	}
	fmt.Fprintln(w, "grants:")
	for _, g := range grants.Grants {
		state := ""
		if g.Disabled {
			state = " [disabled]"
		}
		switch g.Kind {
		case "team_profile":
			// Team profiles carry no name on the hub yet: the service and
			// aicrew's team ID identify them.
			fmt.Fprintf(w, "  team profile %s (service %s, team %s)%s\n", g.ID, g.ServiceID, g.TeamID, state)
		default:
			fmt.Fprintf(w, "  %-12s %s (%s)%s\n", g.Kind, g.Name, g.ID, state)
		}
	}
	return nil
}

// localJSON calls the local service and decodes a 200 answer into out; any
// other status is an error carrying the service's message.
func localJSON(method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://aimem"+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client().Do(req)
	if err != nil {
		return fmt.Errorf("%w (is the local aimem service running?)", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}
