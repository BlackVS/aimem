package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"aimem/internal/teamstate"
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
	opts := &teamSetupOptions{}
	fs.BoolVar(&opts.resume, "fence", false, "")
	fs.BoolVar(&opts.jsonOut, "json", false, "")
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
	s := &teamSetup{dir: dir, root: root, opts: opts, continueOnly: true, report: &teamSetupReport{Status: "blocked", Team: want, Role: "saved"}}
	if abs, err := teamstate.Canonical(dir); err == nil {
		s.report.Checkout = abs
	}
	ok := s.continueSaved(want)
	if ok {
		s.report.Status = "joined"
	}
	s.report.print(stdout, opts.jsonOut)
	if !ok {
		return errTeamSetupBlocked
	}
	return nil
}

// continueSaved is the restart flow: the checks that establish who we are
// and which hub we talk to, then the saved membership, then the duties.
func (s *teamSetup) continueSaved(want string) bool {
	if !s.checkBinding() || !s.checkIdentity() || !s.checkHub() {
		return false
	}
	s.checkBaseCommit()
	s.statePath = teamstate.Path(s.root, s.sel.Repo)
	s.report.StateFile = s.statePath
	saved := s.loadState()
	if saved == nil {
		return s.fail("session", "no saved membership for this checkout and credential", "join on purpose with /join_team TEAM ROLE (aimem teams setup); a restart never joins by itself")
	}
	if want != "" && saved.Team != want && saved.TeamID != want {
		return s.fail("session", fmt.Sprintf("the saved membership is in team %q (%s), not %q", saved.Team, saved.TeamID, want), "re-run without a team, or with the saved one")
	}
	s.opts.team, s.opts.role, s.opts.profile = saved.Team, saved.Role, saved.Profile
	s.report.Team, s.report.Role = saved.Team, saved.Role
	s.state = saved
	switch {
	case saved.SessionID != "" && saved.ResumeKey != "":
		return s.resume("replaying an unconfirmed resume")
	case saved.SessionID != "":
		return s.verifySaved()
	case saved.JoinKey != "":
		// An unconfirmed join replays with its own key: it is the same
		// request, never a second membership.
		return s.join(saved.JoinKey, saved.Team, saved.Profile)
	}
	return s.fail("session", "the saved record holds no session", "join on purpose with /join_team")
}
