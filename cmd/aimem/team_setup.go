package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"aimem/internal/mcp"
	"aimem/internal/process"
	"aimem/internal/processctx"
	"aimem/internal/teamsetup"
	"aimem/internal/wiring"
)

const teamSetupUsage = `usage: aimem teams setup TEAM <worker|coordinator> [flags]

Run from a configured checkout. Verifies the project binding, the ordinary
credential's identity, the hub's team protocol, the selected process and its
required skills, then joins TEAM in the given role (or recognizes the session
this checkout already holds) and ends in the role entry: the roster for a
coordinator, an availability heartbeat plus one bounded inbox read for a
worker. Nonsecret session state is kept under the state root, keyed by this
checkout, project, hub and credential; the token is never printed or stored
there. Nothing here falls back to a broader credential.

Flags (all optional; a declaration is never inferred from the client):
  --label L                readable member label (default <role>-<host>)
  --platform NAME          agent client name (claude-code, codex, opencode, ...)
  --platform-version V     client version, or unknown
  --model-provider P       reported provider, or unknown
  --model-id ID            reported model identifier, or unknown
  --model-version V        reported model version, or unknown
  --model-source S         runtime_reported|operator_configured|agent_reported|unknown
  --capabilities a,b       declared capabilities
  --resume                 after a restart: resume the saved session (new generation;
                           the old handle is fenced) instead of only verifying it
  --new-session            discard the saved handle and join afresh
  --no-repair              report the client integration only; add nothing
  --allow-project-stop-hooks
                           do not block on project-level Stop/StopFailure/PreCompact
                           hooks (they journal every turn twice next to the user-level ones)
  --client-versions        run <client> --version for each client found on PATH
  --json                   machine-readable report on stdout
The client integration step checks docs/SESSION-STATE.md, the SessionStart
handoff hook in .claude/settings.json and .codex/hooks.json, mcpServers.aimem
in .mcp.json and the handoff instruction plus mcp.aimem in opencode.json; it
adds only what is missing, in the installers' shapes, and never replaces an
entry that differs. Exit status 0 means joined and verified; 1 means blocked,
with the missing prerequisite or the operator handoff in the report.`

// errTeamSetupBlocked is the exit-1 outcome; the report already said why.
var errTeamSetupBlocked = errors.New("team setup blocked; see the report above")

func teamSetupCmd(args []string) error {
	return runTeamSetup(args, ".", stateRoot(), os.Stdout)
}

// teamCommandsCmd writes or refreshes the /join_team entry points of a
// checkout (the installers call it after wiring a project; setup repeats
// it on every run). --check reports without writing.
func teamCommandsCmd(args []string) error {
	dir, check := ".", false
	for _, a := range args {
		switch {
		case a == "--check":
			check = true
		case strings.HasPrefix(a, "-"):
			return errors.New("usage: aimem teams commands [DIR] [--check]")
		default:
			dir = a
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if canon, err := filepath.EvalSymlinks(abs); err == nil {
		abs = canon
	}
	home, _ := os.UserHomeDir()
	failed := false
	for _, f := range wiring.InstallCommands(abs, home, !check) {
		detail := f.Detail
		if f.Repaired {
			detail = "written: " + detail
		}
		fmt.Printf("  %-5s %s: %s\n", f.Level, f.File, detail)
		if f.Fix != "" {
			fmt.Printf("        fix: %s\n", f.Fix)
		}
		failed = failed || f.Level == "fail"
	}
	if failed {
		return errors.New("some entry points could not be written")
	}
	return nil
}

// teamSetupOptions is the parsed command line: the onboarding request
// plus the one flag that concerns only this shell (the output format).
type teamSetupOptions struct {
	teamsetup.Options
	jsonOut bool
}

func parseTeamSetupArgs(args []string) (*teamSetupOptions, error) {
	if len(args) < 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		return nil, errors.New(teamSetupUsage)
	}
	o := &teamSetupOptions{Options: teamsetup.Options{Team: args[0], Role: args[1]}}
	if o.Role != "worker" && o.Role != "coordinator" {
		return nil, fmt.Errorf("role must be worker or coordinator, got %q\n%s", o.Role, teamSetupUsage)
	}
	if strings.TrimSpace(o.Team) == "" {
		return nil, errors.New(teamSetupUsage)
	}
	fs := flag.NewFlagSet("teams setup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	host, _ := os.Hostname()
	if host == "" {
		host = "host"
	}
	var caps string
	fs.StringVar(&o.Profile.Label, "label", o.Role+"-"+host, "")
	fs.StringVar(&o.Profile.Platform, "platform", "unknown", "")
	fs.StringVar(&o.Profile.PlatformVersion, "platform-version", "unknown", "")
	fs.StringVar(&o.Profile.Model.Provider, "model-provider", "unknown", "")
	fs.StringVar(&o.Profile.Model.ID, "model-id", "unknown", "")
	fs.StringVar(&o.Profile.Model.Version, "model-version", "unknown", "")
	fs.StringVar(&o.Profile.Model.Source, "model-source", "unknown", "")
	fs.StringVar(&caps, "capabilities", "", "")
	fs.BoolVar(&o.Resume, "resume", false, "")
	fs.BoolVar(&o.NewSession, "new-session", false, "")
	fs.BoolVar(&o.jsonOut, "json", false, "")
	noRepair := false
	fs.BoolVar(&noRepair, "no-repair", false, "")
	fs.BoolVar(&o.Wiring.AllowProjectStopHooks, "allow-project-stop-hooks", false, "")
	fs.BoolVar(&o.Wiring.ClientVersions, "client-versions", false, "")
	if err := fs.Parse(args[2:]); err != nil {
		return nil, fmt.Errorf("%v\n%s", err, teamSetupUsage)
	}
	o.Wiring.Repair = !noRepair
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("unexpected argument %q\n%s", fs.Arg(0), teamSetupUsage)
	}
	if err := teamsetup.CheckProfile(&o.Profile); err != nil {
		return nil, fmt.Errorf("%v\n%s", err, teamSetupUsage)
	}
	if o.Resume && o.NewSession {
		return nil, errors.New("--resume and --new-session exclude each other")
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "platform":
			o.PlatformSet, o.ProfileSet = true, true
		case "label", "platform-version", "model-provider", "model-id", "model-version", "model-source", "capabilities":
			o.ProfileSet = true
		}
	})
	for _, c := range strings.Split(caps, ",") {
		if c = strings.TrimSpace(c); c != "" {
			o.Profile.Capabilities = append(o.Profile.Capabilities, c)
		}
	}
	return o, nil
}

// teamSetupEnv is the shell's host environment for the shared onboarding
// core: the checkout the command runs in, this process's state root, the
// session caller bound to that checkout's credential (the same path the
// MCP tools and `aimem teams` commands use), the session-start process
// bootstrap and the git probe.
func teamSetupEnv(dir, root string) teamsetup.Env {
	return teamsetup.Env{
		Dir:     dir,
		Root:    root,
		Version: version,
		TeamCall: func(ctx context.Context, name string, raw json.RawMessage) (int, []byte, error) {
			return mcp.TeamRequestIn(ctx, dir, root, name, raw)
		},
		Process: func(d, project string) *processctx.Result { return processctx.Load(d, root, project) },
		ProcessAt: func(d, project string, ref process.Ref) *processctx.Result {
			return processctx.LoadRef(d, root, project, ref)
		},
		Git: teamsetup.GitOutput,
		Task: func(ctx context.Context, id string) (int, []byte, error) {
			return mcp.TaskGetIn(ctx, dir, root, id)
		},
	}
}

func runTeamSetup(args []string, dir, root string, stdout io.Writer) error {
	opts, err := parseTeamSetupArgs(args)
	if err != nil {
		return err
	}
	rep := teamsetup.Run(teamSetupEnv(dir, root), &opts.Options)
	rep.Print(stdout, opts.jsonOut)
	if !rep.Joined() {
		return errTeamSetupBlocked
	}
	return nil
}
