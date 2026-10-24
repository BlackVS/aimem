package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"aimem/internal/wiring"
)

const wireUsage = "usage: aimem wire [DIR] [-p|--project ID] [--hub NAME] [--groups a,b] [--check]"

// wireCmd wires a directory as a project from the installed binary: the
// project part of install.ps1 and install.sh, which call it.
func wireCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("wire", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	project := projectFlag(fs, "pin this project id in .aimem.json")
	hub := fs.String("hub", "", "bind this named hub in .aimem.json")
	groups := fs.String("groups", "", "set the knowledge groups in .aimem.json (comma-separated; empty for none)")
	check := fs.Bool("check", false, "report what is wired and what is missing; write nothing")
	var dirs []string
	for {
		if err := fs.Parse(args); err != nil {
			return errors.New(wireUsage)
		}
		if fs.NArg() == 0 {
			break
		}
		dirs = append(dirs, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(dirs) > 1 {
		return errors.New(wireUsage)
	}
	dir := "."
	if len(dirs) == 1 {
		dir = dirs[0]
	}
	o := wiring.ProjectOptions{Project: *project, Hub: *hub, Check: *check, StateRoot: stateRoot()}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "groups" {
			o.Groups = []string{}
			for _, g := range strings.Split(*groups, ",") {
				if g = strings.TrimSpace(g); g != "" {
					o.Groups = append(o.Groups, g)
				}
			}
		}
	})
	o.Home, _ = os.UserHomeDir()
	findings, err := wiring.Wire(dir, o)
	if err != nil {
		return err
	}
	failed, missing := false, false
	for _, f := range findings {
		detail := f.Detail
		if f.Repaired {
			detail = "written: " + detail
		}
		fmt.Fprintf(w, "  %-5s %s: %s\n", f.Level, f.File, detail)
		if f.Fix != "" {
			fmt.Fprintf(w, "        fix: %s\n", f.Fix)
		}
		failed = failed || f.Level == "fail"
		missing = missing || strings.Contains(f.Fix, "aimem wire")
	}
	switch {
	case failed:
		return errors.New("the project is not fully wired; see the failures above")
	case *check && missing:
		return errors.New("the project is not fully wired; run `aimem wire` here")
	case *check:
		fmt.Fprintln(w, "project wired")
	default:
		fmt.Fprintln(w, "project wired. Commit the new files if the project is tracked.")
	}
	return nil
}
