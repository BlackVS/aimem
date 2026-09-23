package main

// The process reference: which Git repository, commit and manifest hold
// the handbook, checklists and templates a project's agents follow
// (docs/DESIGN-kanban-docs.md). `aimem process select|clear` run on the
// hub host as the admin; `aimem process show` runs on any machine and
// prints exactly what the session-start hook injects, with the same
// availability notices, so a person can see what an agent saw.

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

	"aimem/internal/ident"
	"aimem/internal/process"
	"aimem/internal/processctx"
)

func processCmd(args []string) error {
	usage := `usage: aimem process select <repo> <commit> <manifest> [--ref <branch|tag>] [--expect <commit>] [-p <project>]
       aimem process clear --expect <commit> [-p <project>]
       aimem process show [--full] [--template <kind>] [-p <project>]

select/clear run on the hub host (admin); show runs anywhere and prints
what the session-start hook injects for this project.`
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	switch args[0] {
	case "select", "clear":
		fs := flag.NewFlagSet("process "+args[0], flag.ExitOnError)
		p := fs.String("p", "", "project id (default: current directory's project)")
		ref := fs.String("ref", "", "branch or tag to fetch when the server refuses fetch-by-hash")
		expect := fs.String("expect", "", "the commit currently selected (compare-and-swap); empty when none")
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
		proj, err := projectOrCurrent(*p)
		if err != nil {
			return err
		}
		body := map[string]any{"expected_commit": *expect}
		if args[0] == "clear" {
			body["clear"] = true
		} else {
			if len(pos) != 3 {
				return fmt.Errorf("%s", usage)
			}
			body["repo"], body["commit"], body["manifest"] = pos[0], pos[1], pos[2]
			if *ref != "" {
				body["ref"] = *ref
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
		p := fs.String("p", "", "project id (default: current directory's project)")
		tmpl := fs.String("template", "", "print this template from the process set instead of the bootstrap")
		full := fs.Bool("full", false, "print the whole unit regardless of the manifest's injection budget")
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
		return nil
	}
	return fmt.Errorf("%s", usage)
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
