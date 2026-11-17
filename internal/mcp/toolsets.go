package mcp

// The tool sets per mode (docs/DESIGN-tool-sets.md, sections 4 and 5): what
// each facade lists, the teams setting that adds the team protocol's work
// tools, and the notices a tool called by name outside its session's set
// carries. Which tools a facade serves by name, and what each does, is
// unchanged: only the lists and the notices are decided here.

import (
	"errors"
	"fmt"
	"strings"

	"aimem/internal/ident"
	"aimem/internal/store"
	"aimem/internal/teamstate"
)

// teamEntryTools are the four team tools the standalone set keeps, so a
// checkout that never joined can join through /join_team (section 4.2):
// joining needs team_setup or team_continue, the guide team_context and the
// enrollment read team_list.
var teamEntryTools = map[string]bool{"team_setup": true, "team_continue": true, "team_context": true, "team_list": true}

// isTeamWorkTool is one of the 26 tools of the teams set.
func isTeamWorkTool(name string) bool {
	return strings.HasPrefix(name, "team_") && !teamEntryTools[name]
}

// reservationHolderTools are the three the tasks-only set keeps for a
// holder whose client has no shell for `aimem reservation`.
var reservationHolderTools = map[string]bool{
	"task_reservation_status": true, "task_reservation_release": true, "task_reservation_finalize": true,
}

// teamsParam is the hub MCP URL's parameter that adds the teams set to a
// tasks-only session, chosen at connection (`/mcp?tools=teams`).
const teamsParam = "tools"

// errUnknownToolsParam refuses a value of the URL parameter the server does
// not know, instead of ignoring it.
var errUnknownToolsParam = errors.New(`the MCP URL's "tools" parameter takes one value, "teams", which adds the team protocol's tools; leave it out for the standard set`)

// teamsFromURL reads the hub MCP URL's parameter: absent is off.
func teamsFromURL(values []string) (bool, error) {
	switch {
	case len(values) == 0:
		return false, nil
	case len(values) == 1 && values[0] == "teams":
		return true, nil
	}
	return false, errUnknownToolsParam
}

// taskSetDefs is taskToolDefs as a standalone or tasks-only session lists
// them: the task, epic and feature tools and team_list, the team work tools
// only when teams is on, and of the reservation tools only those keep
// admits (none for standalone, the holder's three for tasks-only).
func taskSetDefs(teams bool, keep map[string]bool) []map[string]any {
	var out []map[string]any
	for _, d := range taskToolDefs {
		n := d["name"].(string)
		switch {
		case reservationTools[n] && !keep[n]:
		case isTeamWorkTool(n) && !teams:
		default:
			out = append(out, d)
		}
	}
	return out
}

// memberToolList is the team-mode facade's list: today's without the
// reservation tools, which a member reaches through `aimem reservation`
// (aicrew's member steps) and which stay served by name.
func memberToolList() []map[string]any {
	var out []map[string]any
	for _, d := range teamToolList() {
		if !reservationTools[d["name"].(string)] {
			out = append(out, d)
		}
	}
	return out
}

// teamsOn is the session's teams setting. On the stdio facade it is on
// exactly when the bound checkout holds a team record of its current
// project (internal/teamstate), which every join path writes and leave
// clears; it is read on each call, so a join turns it on at once. On the
// hub it is the URL parameter, fixed for the request.
func (s *srv) teamsOn() bool {
	if s.local == nil {
		return s.teamsByURL
	}
	return checkoutHasTeamRecord(s.local.dir, s.local.root)
}

// checkoutHasTeamRecord reports whether the checkout at dir holds a team
// record for its current project under the state root.
func checkoutHasTeamRecord(dir, root string) bool {
	repo, err := teamstate.Canonical(dir)
	if err != nil {
		return false
	}
	st, err := teamstate.Load(teamstate.Path(root, repo))
	if err != nil || st == nil || st.Repo != repo {
		return false
	}
	project, err := ident.ProjectID(dir)
	return err == nil && st.Project == project
}

// TeamsSetting is the checkout's teams setting as a person reads it, for a
// command that reports the checkout's state as text: on with the team, the
// role and the record's file, or off. A membership that ended on the hub
// keeps its record, so the line says how it is cleared.
func TeamsSetting(dir, root string) string {
	if !checkoutHasTeamRecord(dir, root) {
		return "off: this checkout has no team record; joining a team (/join_team, aimem teams setup) turns it on"
	}
	repo, _ := teamstate.Canonical(dir)
	path := teamstate.Path(root, repo)
	st, _ := teamstate.Load(path)
	return fmt.Sprintf("on: team %s, role %s, record %s. The MCP server lists the team protocol's tools. `aimem teams leave` clears the record; if the membership already ended on the hub (team_continue says so), join anew with /join_team or delete that file", st.Team, st.Role, path)
}

// teamsNotice is the line a team work tool called by name outside the
// teams set carries.
func (s *srv) teamsNotice() string {
	if s.local == nil {
		return `notice: this team tool is not in this session's list. Connect to the hub's MCP with ?tools=teams to list the team protocol's tools; calls by name keep working for two releases.`
	}
	return `notice: this team tool is not in this session's list because this checkout has not joined a team. Join with team_setup (or /join_team, or aimem teams setup) to list the team protocol's tools; calls by name keep working for two releases.`
}

// teamsArrivedNotice is the block team_setup and team_continue add when
// their join turned the checkout's teams setting on.
const teamsArrivedNotice = "This checkout has joined a team, so the session's list now holds the team protocol's tools (team_inbox, team_heartbeat, team_ack and the others); the server has announced the changed list. If your client does not show them, restart the client and run /resume_team: the tools arrive at the next start."

// reservationNotice is the line a reservation tool called by name outside
// the session's set carries. It has no end date: a holder must always be
// able to release or finalize.
const reservationNotice = "notice: the reservation tools are not in this session's list. Claim and record a task with update_task. A task already held by a reservation is finished with `aimem reservation release` or `aimem reservation finalize` in the checkout."

// noticeFor is the notice a call by name to the tool carries in this
// session, or "" when the tool is in the session's set. A team member's
// session carries none: its reservation calls are aicrew's.
func (s *srv) noticeFor(name string) string {
	switch {
	case s.team != nil:
		return ""
	case isTeamWorkTool(name) && !s.teamsOn():
		return s.teamsNotice()
	case reservationTools[name] && !(s.tasksOnly && reservationHolderTools[name]):
		return reservationNotice
	}
	return ""
}

// reservedHint completes the hub's refusal of a generic write on a task
// under an active reservation with the way its holder finishes it, as this
// facade offers it. The HTTP API's own text is unchanged. A team tool's
// refusal is left alone: a team's reserved attempt is finished through the
// team protocol, not by its holder's reservation command.
func (s *srv) reservedHint(name string, err error) error {
	if err == nil || s.team != nil || reservationTools[name] || strings.HasPrefix(name, "team_") ||
		!strings.Contains(err.Error(), store.ErrTaskReserved.Error()) {
		return err
	}
	if s.tasksOnly {
		return errors.New(err.Error() + "\nThe holder finishes the reservation with task_reservation_release or task_reservation_finalize; task_reservation_status shows it.")
	}
	return errors.New(err.Error() + "\nThe holder finishes the reservation with `aimem reservation release` or `aimem reservation finalize`, run in the checkout under the holding credential.")
}
