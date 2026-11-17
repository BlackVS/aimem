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
// exactly when the bound checkout holds the record of a confirmed join of
// its current project (confirmedTeamRecord); it is read on each call, so a
// join turns it on at once. On the hub it is the URL parameter, fixed for
// the request.
func (s *srv) teamsOn() bool {
	if s.local == nil {
		return s.teamsByURL
	}
	st, _, _ := confirmedTeamRecord(s.local.dir, s.local.root)
	return st != nil
}

// confirmedTeamRecord is the checkout's team record when it is the record
// of a confirmed join of the checkout's current project, with its path; nil
// otherwise. A join saves its record with only the retry key before it is
// sent, and the session id once the hub confirms it, so a join that was not
// confirmed holds no session id and does not turn the setting on. A
// membership that ended on the hub keeps its session id. The error is an
// unreadable record, which reads as off.
func confirmedTeamRecord(dir, root string) (*teamstate.State, string, error) {
	repo, err := teamstate.Canonical(dir)
	if err != nil {
		return nil, "", err
	}
	path := teamstate.Path(root, repo)
	st, err := teamstate.Load(path)
	if err != nil || st == nil || st.Repo != repo || st.SessionID == "" {
		return nil, path, err
	}
	// A checkout whose project cannot be resolved, or that was re-pointed
	// at another project, holds no record of its current project: off.
	if project, err := ident.ProjectID(dir); err != nil || st.Project != project {
		return nil, path, nil
	}
	return st, path, nil
}

// TeamsSetting is the checkout's teams setting as a person reads it, for a
// command that reports the checkout's state as text: on with the team, the
// role and the record's file, or off. It is written from one read of the
// record. A membership that ended on the hub keeps its record, so the line
// says how it is cleared.
func TeamsSetting(dir, root string) string {
	st, path, err := confirmedTeamRecord(dir, root)
	switch {
	case err != nil:
		return "off: this checkout's team record cannot be read (" + err.Error() + "); joining a team (/join_team, aimem teams setup) writes it anew"
	case st == nil:
		return "off: this checkout holds no record of a confirmed join; joining a team (/join_team, aimem teams setup) turns it on"
	}
	return fmt.Sprintf("on: team %s, role %s, record %s. The MCP server lists the team protocol's tools. `aimem teams leave` clears the record; if the membership already ended on the hub (team_continue says so), join anew with /join_team or delete that file", st.Team, st.Role, path)
}

// servedByNamePeriod is how long a team tool called by name outside the
// teams set keeps answering. The length is stated in one place, the
// changelog's entry on the tool sets, so that a changed answer is one edit.
const servedByNamePeriod = "calls by name keep working for the period the aimem changelog states in its entry on the tool sets"

// teamsNotice is the line a team work tool called by name outside the
// teams set carries.
func (s *srv) teamsNotice() string {
	if s.local == nil {
		return "notice: this team tool is not in this session's list. Connect to the hub's MCP with ?tools=teams to list the team protocol's tools; " + servedByNamePeriod + "."
	}
	return "notice: this team tool is not in this session's list because this checkout has not joined a team. Join with team_setup (or /join_team, or aimem teams setup) to list the team protocol's tools; " + servedByNamePeriod + "."
}

// teamsArrivedNotice is the step team_setup and team_continue add when
// their join turned the checkout's teams setting on.
const teamsArrivedNotice = "This checkout has joined a team, so the session's list now holds the team protocol's tools (team_inbox, team_heartbeat, team_ack and the others); the server has announced the changed list. If your client does not show them, restart the client and run /resume_team: the tools arrive at the next start."

// teamsWithdrawnNotice is the step they add when a new join replaced a
// confirmed record and was not confirmed, or was refused (which drops the
// record): the setting went off within the call, and the list with it.
const teamsWithdrawnNotice = "This checkout holds no confirmed membership now: the new join was not confirmed or was refused (the session check above says which). The session's list no longer holds the team protocol's work tools, and the server has announced the changed list. They come back once a join is confirmed."

// reservationNotice is the line a reservation tool called by name outside
// the session's set carries, per facade. It has no end date: a holder must
// always be able to release or finalize.
func (s *srv) reservationNotice() string {
	if s.tasksOnly {
		return "notice: this reservation tool is not in this session's list. Claim and record a task with update_task. A task already held by a reservation is finished with task_reservation_release or task_reservation_finalize, which this session lists; task_reservation_status shows it."
	}
	return "notice: the reservation tools are not in this session's list. Claim and record a task with update_task. A task already held by a reservation is finished with `aimem reservation release` or `aimem reservation finalize` in the checkout."
}

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
		return s.reservationNotice()
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
