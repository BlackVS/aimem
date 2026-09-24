package teamsetup

import (
	"encoding/json"
	"fmt"
	"io"
)

// Print renders the report: indented JSON, or the human layout the CLI
// prints (checks, roster, reserved attempt, inbox, skills, handoff, next
// steps, status).
func (r *Report) Print(w io.Writer, jsonOut bool) {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.Encode(r)
		return
	}
	fmt.Fprintf(w, "aimem teams: team %q, role %s, checkout %s\n", r.Team, r.Role, r.Checkout)
	for _, c := range r.Checks {
		fmt.Fprintf(w, "  %-5s %-12s %s\n", c.Level, c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Fprintf(w, "        fix: %s\n", c.Fix)
		}
	}
	if len(r.Roster) > 0 {
		fmt.Fprintf(w, "Roster (%d):\n", len(r.Roster))
		for _, m := range r.Roster {
			flags := m.State + "/" + m.Availability
			if m.Suspect {
				flags += "/suspect"
			}
			fmt.Fprintf(w, "  %-28s %-12s %-24s platform %s %s; model %s (%s)\n", m.Label, m.Role, flags, m.Platform, m.PlatformVersion, m.Model.ID, m.Model.Source)
		}
	}
	if r.Reserved != nil {
		fmt.Fprintf(w, "Reserved attempt %s on task %s (%s): %s", r.Reserved.ID, r.Reserved.TaskID, orUnknown(r.Reserved.Title), r.Reserved.State)
		if r.Reserved.Reason != "" {
			fmt.Fprintf(w, "; reason: %s", r.Reserved.Reason)
		}
		fmt.Fprintln(w)
	}
	if r.Inbox != nil {
		fmt.Fprintf(w, "Inbox: %d unacknowledged (next cursor %d); %s\n", r.Inbox.Unacknowledged, r.Inbox.NextCursor, r.Inbox.Note)
		for _, m := range r.Inbox.Messages {
			line := fmt.Sprintf("  #%d %s %s from %s", m.Sequence, m.ID, m.Kind, m.From)
			if m.Operation != "" {
				line += " " + m.Operation
			}
			if m.AttemptID != "" {
				line += " attempt " + m.AttemptID
			}
			if m.TaskID != "" {
				line += " task " + m.TaskID
			}
			if m.State != "" {
				line += " -> " + m.State
			}
			if m.Excerpt != "" {
				line += ": " + m.Excerpt
			}
			fmt.Fprintln(w, line)
		}
	}
	if r.Wiring != nil && len(r.Wiring.Skills) > 0 {
		fmt.Fprintln(w, "Skills per client:")
		for _, st := range r.Wiring.Skills {
			if st.Path != "" {
				fmt.Fprintf(w, "  %-24s %-9s %s\n", st.Skill, st.Client, st.Path)
			} else {
				fmt.Fprintf(w, "  %-24s %-9s NOT FOUND: %s\n", st.Skill, st.Client, st.Fix)
			}
		}
	}
	if r.Handoff != "" {
		fmt.Fprintln(w, r.Handoff)
	}
	if len(r.Next) > 0 {
		fmt.Fprintln(w, "Next:")
		for _, n := range r.Next {
			fmt.Fprintf(w, "  - %s\n", n)
		}
	}
	if rd := r.Readiness; rd != nil {
		fmt.Fprintf(w, "Readiness: ready_for_work %v\n", rd.ReadyForWork)
		for _, p := range []struct {
			name string
			part Part
		}{{"membership", rd.Membership}, {"role_context", rd.RoleContext}, {"project_process", rd.ProjectProcess}, {"execution", rd.Execution}} {
			line := fmt.Sprintf("  %-16s %s", p.name, p.part.State)
			if p.part.Version != "" {
				line += " version " + p.part.Version
			}
			if p.part.Digest != "" {
				line += " " + p.part.Digest
			}
			if p.part.Detail != "" {
				line += ": " + p.part.Detail
			}
			fmt.Fprintln(w, line)
			if p.part.Fix != "" {
				fmt.Fprintf(w, "        fix: %s\n", p.part.Fix)
			}
		}
		fmt.Fprintf(w, "  acknowledged: %s\n", rd.Acknowledged)
	}
	fmt.Fprintf(w, "Status: %s (state file %s)\n", r.Status, r.StateFile)
	for _, d := range r.Delivered {
		fmt.Fprintf(w, "\n%s", d.Text)
	}
}
