package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"strings"

	"aimem/internal/teamsetup"
)

const teamContinueUsage = `usage: aimem teams continue [TEAM] [--fence] [--json]

Continue the team membership this checkout already holds, after a client
restart or a compaction: verifies the binding, the credential and the hub,
verifies or resumes the saved session, then reports the duties the hub
holds for it (the reserved attempt and its state, the unacknowledged inbox,
the roster) so the agent picks up exactly where the hub says it is. Never
joins: a membership that has ended is reported, and /join_team is the only
way back in. TEAM, when given, must match the saved membership.

  --fence   resume the saved session even when it is still heartbeating
            (fences a handle another process of yours may hold); without
            it a suspect session is resumed and a live one only verified
  --json    machine-readable report on stdout
Nothing here wakes an idle agent: the command reads once, bounded, and the
agent polls team_inbox itself afterwards. Exit status 0 means the membership
is verified; 1 means it is not, with the reason in the report.`

func teamContinueCmd(args []string) error {
	return runTeamContinue(args, ".", stateRoot(), os.Stdout)
}

func runTeamContinue(args []string, dir, root string, stdout io.Writer) error {
	fs := flag.NewFlagSet("teams continue", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var fence, jsonOut bool
	fs.BoolVar(&fence, "fence", false, "")
	fs.BoolVar(&jsonOut, "json", false, "")
	// Flags may follow the optional TEAM positional.
	var positional []string
	rest := args
	for len(rest) > 0 {
		if strings.HasPrefix(rest[0], "-") {
			break
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || len(positional) > 1 {
		return errors.New(teamContinueUsage)
	}
	want := ""
	if len(positional) == 1 {
		want = positional[0]
	}
	rep := teamsetup.Continue(teamSetupEnv(dir, root), want, fence)
	rep.Print(stdout, jsonOut)
	if !rep.Joined() {
		return errTeamSetupBlocked
	}
	return nil
}
