package teamsetup

// Readiness (docs/DESIGN-portable-team-context.md, decisions 3-5): four
// independent parts, of which only three can be established here. A
// joined session is not permission to work; the role guidance and the
// project process are delivered in the same result, and the agent's own
// execution environment is never inferred from this process's health.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"aimem/internal/processctx"
	"aimem/internal/teamguide"
)

// Part is one readiness part: its state and, when it is not ready, why
// and who does what about it.
type Part struct {
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
	Fix     string `json:"fix,omitempty"`
	Version string `json:"version,omitempty"` // role guidance: build version; project process: commit
	Digest  string `json:"digest,omitempty"`  // SHA-256 of what was delivered
}

// Readiness is the report's answer to "may this member start work".
type Readiness struct {
	ReadyForWork   bool   `json:"ready_for_work"`
	Membership     Part   `json:"membership"`
	RoleContext    Part   `json:"role_context"`
	ProjectProcess Part   `json:"project_process"`
	Execution      Part   `json:"execution"`
	Acknowledged   string `json:"acknowledged"`
}

// Delivery is content carried in full by this result, ending with its
// own terminator line. The MCP host sends each as its own text block; the
// CLI prints it after the report.
type Delivery struct {
	Kind string `json:"kind"` // role_context or project_process
	Text string `json:"text"`
}

// The membership states: active is a session verified, joined or resumed
// in this run; ended, refused and not_verified say why there is none.
const (
	memberActive      = "active"
	memberEnded       = "ended"
	memberRefused     = "refused"
	memberNotVerified = "not_verified"
)

// hubTeamProtocol is the session protocol_version every session response
// is checked against (sessionOf, roster); the guidance must describe it.
const hubTeamProtocol = 1

const acknowledgedNote = "not recorded: delivery in this result is not reading. What the agent followed is its own declaration: name the role guidance digest and the process commit in the evidence of a submitted result"

var executionPart = Part{
	State:  "not_verified",
	Detail: "nothing here can observe the agent's own shell, build tools or coding runner: a live MCP process, a join and a readable HEAD do not show that they work",
	Fix:    "check them yourself before accepting a coding attempt; decline or block with the reason when they fail",
}

// notEvaluated is the context part of a report without a verified session.
var notEvaluated = Part{State: "not_evaluated", Detail: "evaluated only for a verified session"}

// newReadiness is the starting point of every run: nothing established.
func newReadiness() *Readiness {
	return &Readiness{
		Membership:     Part{State: memberNotVerified},
		RoleContext:    notEvaluated,
		ProjectProcess: notEvaluated,
		Execution:      executionPart,
		Acknowledged:   acknowledgedNote,
	}
}

// member records why the run holds no session; a later success overrides it.
func (s *setup) member(state, detail string) {
	s.report.Readiness.Membership = Part{State: state, Detail: detail}
}

// evaluateContext delivers the role guidance for role and the project
// process, fills their readiness parts, and reports a changed guidance
// digest. It runs only for a verified session, before availability is
// announced, so the announcement can follow from it.
func (s *setup) evaluateContext(role string) {
	rd := s.report.Readiness
	rd.RoleContext = s.roleContext(role)
	rd.ProjectProcess = s.projectProcess()
	rd.ReadyForWork = rd.Membership.State == memberActive && rd.RoleContext.State == "delivered" && rd.ProjectProcess.State == string(processctx.Ready)
}

func (s *setup) roleContext(role string) Part {
	u, err := teamguide.Embedded()
	if err != nil {
		return Part{State: "unavailable", Detail: "this binary's team guidance is invalid: " + err.Error(), Fix: "a packaging defect: install a released aimem build"}
	}
	if teamguide.TeamProtocol != hubTeamProtocol {
		return Part{State: "incompatible", Detail: fmt.Sprintf("the guidance describes team protocol %d; this hub speaks %d", teamguide.TeamProtocol, hubTeamProtocol), Fix: "upgrade the client and hub to a matching release"}
	}
	text, err := u.Role(role, s.env.Version)
	if err != nil {
		return Part{State: "unavailable", Detail: err.Error()}
	}
	s.report.Delivered = append(s.report.Delivered, Delivery{Kind: "role_context", Text: text})
	part := Part{State: "delivered", Version: orKeep(s.env.Version, "dev"), Digest: u.Digest,
		Detail: "the complete " + role + " guidance follows this report, ending with its terminator line; team_context re-reads it"}
	if st := s.state; st != nil {
		if st.RoleDigest != "" && st.RoleDigest != u.Digest {
			s.check("role context", "warn", fmt.Sprintf("the team guidance changed since it was last delivered to this membership: %s (version %s) is now %s (version %s); read it again before acting", st.RoleDigest, orKeep(st.RoleVersion, "unknown"), u.Digest, part.Version), "")
		}
		if st.RoleDigest != u.Digest || st.RoleVersion != part.Version {
			st.RoleDigest, st.RoleVersion = u.Digest, part.Version
			if err := s.saveState(); err != nil {
				s.check("state", "warn", "the delivered guidance digest was not saved: "+err.Error(), "")
			}
		}
	}
	return part
}

func (s *setup) projectProcess() Part {
	if s.env.Process == nil {
		return Part{State: "not_checked", Detail: "this entry point does not read the project process", Fix: "use team_setup or team_continue on the checkout-bound local MCP, or aimem teams setup"}
	}
	r := s.proc
	if r == nil {
		r = s.env.Process(s.env.Dir, s.sel.Project)
		s.proc = r
	}
	part := Part{State: string(r.State), Detail: r.Detail, Fix: r.Fix}
	if r.Ref != nil {
		part.Version = r.Ref.Commit
	}
	if !r.Complete() {
		return part
	}
	text, err := r.Deliver()
	if err != nil {
		return Part{State: "unavailable", Detail: err.Error()}
	}
	s.report.Delivered = append(s.report.Delivered, Delivery{Kind: "project_process", Text: text})
	sum := sha256.Sum256([]byte(r.Unit))
	part.Digest = "sha256:" + hex.EncodeToString(sum[:])
	if r.State == processctx.Ready {
		part.Detail = "the complete selected process follows this report, ending with its terminator line; process_context re-reads it"
	} else {
		// Delivered, marked, and never current authority.
		part.Detail = "delivered with its notice, but not current: " + r.Detail
		part.Fix = "retry when the hub is reachable; until then this is not a ready process context"
	}
	return part
}

// notReady names the parts that keep ready_for_work false, for the
// availability check and the next steps.
func (r *Readiness) notReady() string {
	var parts []string
	if r.Membership.State != memberActive {
		parts = append(parts, "membership "+r.Membership.State)
	}
	if r.RoleContext.State != "delivered" {
		parts = append(parts, "role_context "+r.RoleContext.State)
	}
	if r.ProjectProcess.State != string(processctx.Ready) {
		parts = append(parts, "project_process "+r.ProjectProcess.State)
	}
	return strings.Join(parts, ", ")
}

// readinessNext puts the readiness steps first in the next steps: what the
// member may and may not do before anything else in the report.
func (s *setup) readinessNext(role string) {
	rd := s.report.Readiness
	var lines []string
	switch {
	case rd.ReadyForWork:
		lines = append(lines, "ready_for_work: read the "+role+" guidance and the project process delivered after this report before acting; name the guidance digest and the process commit in the evidence of a submitted result")
	default:
		var fixes []string
		for _, p := range []Part{rd.RoleContext, rd.ProjectProcess} {
			if p.Fix != "" {
				fixes = append(fixes, p.Fix)
			}
		}
		fix := ""
		if len(fixes) > 0 {
			fix = "; to fix: " + strings.Join(fixes, "; ")
		}
		if role == "coordinator" {
			lines = append(lines, "NOT ready for work ("+rd.notReady()+"): issue no offers until team_continue reports ready_for_work; reading the roster and the inbox is fine"+fix)
		} else {
			lines = append(lines, "NOT ready for work ("+rd.notReady()+"): you announced unavailable. Accept nothing; decline any offer listed in this report with this reason; keep every heartbeat unavailable until team_continue reports ready_for_work"+fix)
		}
	}
	lines = append(lines, "execution is not verified here: check your own shell, build tools and coding runner before accepting a coding attempt")
	s.report.Next = append(lines, s.report.Next...)
}

// onceReady qualifies a role step that starts work, so the steps never
// contradict a member that is not ready.
func onceReady(rd *Readiness, step string) string {
	if rd.ReadyForWork {
		return step
	}
	return "only once team_continue reports ready_for_work: " + step
}
