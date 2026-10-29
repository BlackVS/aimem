package main

// `aimem project <verb>`: a project's properties on the hub
// (docs/DESIGN-AIFORGE-PILOT-1.md §1, §5). `repo set|clear` and `show` run
// on the hub host as the admin, through the local service, like `process
// select`. `list` prints each project's tasks and process state; `id` and
// `drop` are the namespace's forms of `project-id` and `drop-project`, which
// keep working as they are, as does `projects` (the raw id list).

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

	"aimem/internal/process"
	"aimem/internal/skills"
	"aimem/internal/store"
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

repo set|clear, show and list run on the hub host (admin). list prints
each project's tasks and process state and marks a project with tasks on
and no process as not ready for work, with the command that selects one;
it marks a project whose process requires skills the hub lacks the same
way, with the command that imports each. show warns of the same.
The repository is the one repository the project's work happens in:
--kind is the forge's API dialect, the host of --url names the credential
a member needs, and --access is what members need (write, the default, or
read). The hub stores no default branch: the forge owns it.

Examples, one per command:
  aimem project repo set --project example --kind github --url https://github.com/example/example.git
  aimem project repo clear --project example
  aimem project show --project example
  aimem project list
  aimem project id /path/to/checkout
  aimem project drop --project example --yes`

func projectNamespaceCmd(args []string) error {
	if len(args) == 0 {
		return errors.New(projectUsage)
	}
	switch args[0] {
	case "list":
		return projectListCmd(args[1:])
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

// processSelected reports whether the hub holds a process selection for
// the project.
func processSelected(project string) (bool, error) {
	var proc struct {
		Current *json.RawMessage `json:"current"`
	}
	// ?history=1 answers 200 with a null current when nothing is selected.
	err := localJSON(http.MethodGet, "/v1/projects/"+url.PathEscape(project)+"/process?history=1", nil, &proc)
	return proc.Current != nil, err
}

// processesInUse reads the process selections this hub already uses, or
// nil when it cannot say (a hub older than the route).
func processesInUse() []process.InUse {
	var out struct {
		Processes []process.InUse `json:"processes"`
	}
	if localJSON(http.MethodGet, "/v1/processes", nil, &out) != nil {
		return nil
	}
	return out.Processes
}

// hubLacksSkills marks a project whose selected process requires skills
// the hub's sets do not hold: a member's `aimem skills install` refuses
// them until the admin imports them on the hub host.
const hubLacksSkills = "the hub lacks required skills of the project's process"

// hubSkillsIndex reads this host's skills index, as the admin reads it.
func hubSkillsIndex() (*skills.IndexAnswer, error) {
	var ix skills.IndexAnswer
	if err := localJSON(http.MethodGet, "/v1/skills", nil, &ix); err != nil {
		return nil, err
	}
	return &ix, nil
}

// missingHubSkills lists each required skill of the project's selected
// process that the hub cannot serve, with the command that imports it.
// None when the project has no selection with a stored set.
func missingHubSkills(proj string, ix *skills.IndexAnswer) ([]string, error) {
	var sel struct {
		Current *process.Ref `json:"current"`
	}
	if err := localJSON(http.MethodGet, "/v1/projects/"+url.PathEscape(proj)+"/process", nil, &sel); err != nil {
		return nil, err
	}
	if sel.Current == nil || sel.Current.Digest == "" {
		return nil, nil
	}
	bundle, err := storedSet(proj, *sel.Current)
	if err != nil {
		return nil, err
	}
	set, err := bundle.Set(*sel.Current)
	if err != nil {
		return nil, fmt.Errorf("the stored set does not load: %w", err)
	}
	_, problems := skills.Resolve(ix, set.Manifest.Skills)
	return problems, nil
}

// hubSkillsGap is what select and show print about the hub's skills for
// the project: hubLacksSkills and one indented line per missing skill,
// why the check could not run, or nothing.
func hubSkillsGap(proj string) string {
	ix, err := hubSkillsIndex()
	var missing []string
	if err == nil {
		missing, err = missingHubSkills(proj, ix)
	}
	if err != nil {
		return fmt.Sprintf("could not check the hub's skills for the project's process: %v\n", err)
	}
	if len(missing) == 0 {
		return ""
	}
	return hubLacksSkills + ":\n  " + strings.Join(missing, "\n  ") + "\n"
}

// notReady is the line that marks a project with tasks on and no process.
const notReady = "not ready for work: " + process.NotSelected + "\n"

// tasksOnGap is what `tasks on` adds for a project left without a process:
// the hub's own answer to the toggle, or, from a hub too old to give one,
// the same text from the CLI's own reads. Empty when a process is selected.
func tasksOnGap(project string, answer []byte) string {
	var a struct {
		NextStep string `json:"next_step"`
	}
	if json.Unmarshal(answer, &a) == nil && a.NextStep != "" {
		return notReady + a.NextStep
	}
	selected, err := processSelected(project)
	if err != nil {
		return fmt.Sprintf("could not check the project's process selection: %v\n", err)
	}
	if selected {
		return ""
	}
	return notReady + process.NextStep(project, processesInUse())
}

// projectListCmd prints every project on the hub with its tasks and
// process state. A project with tasks on and no process is marked not
// ready, with the command that selects one.
func projectListCmd(args []string) error {
	if len(args) != 0 {
		return errors.New(projectUsage)
	}
	var all, on struct {
		Projects []string `json:"projects"`
	}
	if err := localJSON(http.MethodGet, "/v1/projects", nil, &all); err != nil {
		return err
	}
	if err := localJSON(http.MethodGet, "/v1/projects?tasks_enabled=true", nil, &on); err != nil {
		return err
	}
	tasksOn := map[string]bool{}
	width := 0
	for _, p := range on.Projects {
		tasksOn[p] = true
	}
	for _, p := range all.Projects {
		width = max(width, len(p))
	}
	var inUse []process.InUse
	inUseRead := false
	var ix *skills.IndexAnswer
	var ixErr error
	ixRead := false
	for _, p := range all.Projects {
		fmt.Printf("%-*s  ", width, p)
		switch {
		case store.IsReservedProject(p):
			fmt.Println("memory store, no tasks")
		case !tasksOn[p]:
			fmt.Println("tasks off")
		default:
			selected, err := processSelected(p)
			if err != nil {
				fmt.Printf("tasks on, process selection unknown: %v\n", err)
				continue
			}
			if selected {
				if !ixRead {
					ix, ixErr = hubSkillsIndex()
					ixRead = true
				}
				var missing []string
				err := ixErr
				if err == nil {
					missing, err = missingHubSkills(p, ix)
				}
				switch {
				case err != nil:
					fmt.Printf("tasks on, process selected; could not check the hub's skills: %v\n", err)
				case len(missing) > 0:
					fmt.Println("not ready for work: " + hubLacksSkills)
					for _, line := range missing {
						fmt.Println("  " + line)
					}
				default:
					fmt.Println("tasks on, process selected")
				}
				continue
			}
			if !inUseRead {
				inUse, inUseRead = processesInUse(), true
			}
			fmt.Print(notReady)
			for _, line := range strings.SplitAfter(process.NextStep(p, inUse), "\n") {
				if line != "" {
					fmt.Print("  " + line)
				}
			}
		}
	}
	return nil
}

// projectShowCmd prints what the hub holds for a project: its repository,
// whether its tasks are on, its process pin and who is granted it, with
// names beside IDs. Tasks on with no process is an unfinished setup and
// says so.
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
	var tasks struct {
		Value string `json:"value"`
	}
	if err := localJSON(http.MethodGet, base+"/meta/"+store.TasksMetaKey, nil, &tasks); err != nil {
		return err
	}
	tasksOn := tasks.Value == "on"
	w := os.Stdout
	defer printReadiness(w, *p)
	fmt.Fprintf(w, "project %s\n", *p)
	fmt.Fprintf(w, "repository: %s\n", repo.Repository.line())
	if r := repo.Repository; r != nil && r.SetBy != "" {
		fmt.Fprintf(w, "  set %s by %s\n", r.SetAt, r.SetBy)
	}
	if tasksOn {
		fmt.Fprintln(w, "tasks:      on")
	} else {
		fmt.Fprintln(w, "tasks:      off")
	}
	if c := proc.Current; c != nil {
		fmt.Fprintf(w, "process:    %s %s %s", c.Repo, c.Commit, c.Manifest)
		if c.Ref != "" {
			fmt.Fprintf(w, " (ref %s)", c.Ref)
		}
		fmt.Fprintf(w, "\n  selected %s by %s\n", c.SelectedAt, c.SelectedBy)
		if gap := hubSkillsGap(*p); gap != "" {
			fmt.Fprint(w, "warning:    "+gap)
		}
	} else {
		fmt.Fprintln(w, "process:    (none selected)")
		if tasksOn {
			fmt.Fprint(w, "warning:    "+notReady)
			fmt.Fprint(w, process.NextStep(*p, processesInUse()))
		}
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
			// The name is the one aicrewd registered (team.register); the
			// service and aicrew's team ID identify the profile either way.
			name := g.Name
			if name == "" {
				name = "(no name registered)"
			}
			fmt.Fprintf(w, "  team         %s (profile %s, service %s, team %s)%s\n", name, g.ID, g.ServiceID, g.TeamID, state)
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
