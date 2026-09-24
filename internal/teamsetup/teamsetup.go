// Package teamsetup is the verified onboarding of one checkout into an
// agent team: the checks (binding, client integration, credential
// identity, hub protocol, enrollment, process, base commit), the
// reconciliation of the saved membership (verify, resume or join, each
// with its persisted retry key), and the role entry (roster, availability,
// reserved attempt, one bounded inbox read). `aimem teams setup` and
// `aimem teams continue` run it from a shell; the checkout-bound local MCP
// facade runs the same code from the credential owner's process. There is
// one state machine; the host supplies the checkout, the state root, the
// hub session caller and the probes through Env and nothing else: no
// token, no command string, no executable, no other checkout.
package teamsetup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"aimem/internal/adapter"
	"aimem/internal/process"
	"aimem/internal/processctx"
	"aimem/internal/store"
	"aimem/internal/taskcred"
	"aimem/internal/teamstate"
	"aimem/internal/uuidv7"
	"aimem/internal/wiring"
)

// Protocol is the first hub release with the complete session, assignment
// and lifecycle-inbox loop the playbooks assume.
var Protocol = [3]int{0, 7, 0}

// HubTimeout bounds each hub round trip except the inbox read, which is
// asked with wait_seconds 0 and gets the same bound.
const HubTimeout = 10 * time.Second

// Env is what one run takes from its host: the checkout and state root it
// is bound to, the client version it reports, and the operations the host
// performs for it. The hub caller is bound to the checkout's own
// credential by whoever constructs it; the probes are fixed queries. An
// owner-context host (the MCP facade) decides which probes it runs: a nil
// Process or Git is reported as unchecked, never substituted.
type Env struct {
	Dir     string // the checkout: the CLI's working directory, the facade's configured checkout
	Root    string // the state root holding the credential store and team session state
	Version string // this client's build version
	// TeamCall performs one session operation (team_join, team_members,
	// team_heartbeat, team_resume, team_reserved, team_inbox, ...) with
	// the checkout's ordinary credential and returns the hub's status and
	// raw body; never the operator socket, never a checkpoint token.
	TeamCall func(ctx context.Context, name string, args json.RawMessage) (int, []byte, error)
	// Process resolves the process context selected for the project, with
	// the checkout's credential: processctx.Load, typed.
	Process func(dir, project string) *processctx.Result
	// Git runs one read-only git query in the checkout (rev-parse HEAD,
	// status --porcelain) and returns its trimmed output. GitOutput is the
	// shell's probe; see its note before running it under another identity.
	Git func(dir string, args ...string) (string, error)
	// Task reads one task (GET /v1/tasks/{id}) with the checkout's
	// credential: the current revision a worker's block must name. nil
	// means this host cannot read it, and no block is sent.
	Task func(ctx context.Context, id string) (int, []byte, error)
}

// Options are the request: the team and role asked for, the declared
// profile, and the explicit recovery choices. Nothing here is inferred.
type Options struct {
	Team, Role  string
	Profile     store.TeamProfile
	Resume      bool // after a restart: resume the saved session (fence the old handle) instead of only verifying it
	NewSession  bool // discard the saved handle and join afresh
	PlatformSet bool // the platform was declared, not defaulted
	ProfileSet  bool // any profile field was declared; otherwise a saved declaration is reused
	Wiring      wiring.Options
}

// Check is one verified fact of the report. Only fail blocks.
type Check struct {
	Name   string `json:"name"`
	Level  string `json:"level"` // ok | warn | fail
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Session is the public member view the hub returns.
type Session struct {
	ID                    string `json:"id"`
	TeamID                string `json:"team_id"`
	Generation            int64  `json:"generation"`
	CoordinatorGeneration int64  `json:"coordinator_generation,omitempty"`
	Role                  string `json:"role"`
	State                 string `json:"state"`
	Availability          string `json:"availability"`
	ProfileRevision       int64  `json:"profile_revision"`
	store.TeamProfile
	LastSeenAt string `json:"last_seen_at"`
	Suspect    bool   `json:"suspect"`
}

// Report is the outcome of one run: joined or blocked, with every check,
// the session and its duties, and what to do next. It never carries a
// token.
type Report struct {
	Status     string         `json:"status"` // joined | blocked
	Team       string         `json:"team"`
	Role       string         `json:"role"`
	Checkout   string         `json:"checkout"`
	RunAs      string         `json:"run_as,omitempty"` // the OS account this run held the credential as
	BaseCommit string         `json:"base_commit,omitempty"`
	Checks     []Check        `json:"checks"`
	Session    *Session       `json:"session,omitempty"`
	Roster     []Session      `json:"roster,omitempty"`
	Inbox      *Inbox         `json:"inbox,omitempty"`
	Reserved   *Reserved      `json:"reserved,omitempty"`
	Wiring     *wiring.Report `json:"integration,omitempty"`
	Handoff    string         `json:"operator_handoff,omitempty"`
	Next       []string       `json:"next"`
	StateFile  string         `json:"state_file"`
	ServerTime string         `json:"server_time,omitempty"`
	// Readiness says whether this member may start work, part by part;
	// status says only whether a verified session exists.
	Readiness *Readiness `json:"readiness"`
	// Delivered is the content this result carries in full.
	Delivered []Delivery `json:"delivered,omitempty"`
}

// Joined reports whether the run ended in a verified session.
func (r *Report) Joined() bool { return r.Status == "joined" }

// Reserved is the part of a reserved assignment the report needs: the
// hub's record of what this session owns, and what its state asks for.
type Reserved struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	State     string `json:"state"`
	Title     string `json:"title,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Next      string `json:"next,omitempty"`
}

// Message is one unacknowledged inbox message, summarized.
type Message struct {
	ID        string `json:"id"`
	Sequence  int64  `json:"sequence"`
	Kind      string `json:"kind"`
	From      string `json:"from"` // sender label, or "hub" for a lifecycle message
	Operation string `json:"operation,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	State     string `json:"state,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Excerpt   string `json:"excerpt,omitempty"`
}

// Inbox is the one bounded read of what this session has not acknowledged.
type Inbox struct {
	Unacknowledged int       `json:"unacknowledged"`
	NextCursor     int64     `json:"next_cursor"`
	HasMore        bool      `json:"has_more"`
	WaitSeconds    int       `json:"wait_seconds"`
	Note           string    `json:"note"`
	Messages       []Message `json:"messages,omitempty"`
}

// Run is the setup flow: the checks in order, then the saved membership
// reconciled against the request (verify, resume or join), then the role
// entry. The report says joined only when a verified session exists at
// the end.
func Run(env Env, opts *Options) *Report {
	s := &setup{env: env, opts: opts, report: &Report{Status: "blocked", Team: opts.Team, Role: opts.Role, RunAs: processUser(), Readiness: newReadiness()}}
	if abs, err := filepath.Abs(env.Dir); err == nil {
		s.report.Checkout = abs
	}
	if s.run() {
		s.report.Status = "joined"
	}
	return s.report
}

// Continue is the restart flow: the checks that establish who we are and
// which hub we talk to, then the saved membership verified or resumed,
// then the duties the hub holds for it. It never joins: a membership that
// has ended is reported, and setup is the only way back in. team, when
// given, must match the saved membership; fence resumes the saved session
// even when it is still heartbeating.
func Continue(env Env, team string, fence bool) *Report {
	s := &setup{env: env, opts: &Options{Resume: fence}, continueOnly: true, report: &Report{Status: "blocked", Team: team, Role: "saved", RunAs: processUser(), Readiness: newReadiness()}}
	if abs, err := teamstate.Canonical(env.Dir); err == nil {
		s.report.Checkout = abs
	}
	if s.continueSaved(team) {
		s.report.Status = "joined"
	}
	return s.report
}

// GitOutput is the shell's git probe: git from PATH, run in the checkout,
// output trimmed. Read-only queries only; the checkout's own .git/config
// still applies to them (core.fsmonitor and filter.*.clean name programs
// git status may run), so a host that runs as a different identity than
// the one that edits the checkout uses GitHeadOnly instead.
func GitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ErrNotProbed is what a host's probe returns for a query it does not run
// under its identity; the report says so instead of guessing.
var ErrNotProbed = errors.New("not probed under this identity")

// GitHeadOnly is the owner-context probe: `rev-parse HEAD` reads refs and
// nothing else, so it is run; everything else (the working-tree status,
// which consults configuration the checkout's editor controls) is
// reported as not probed.
func GitHeadOnly(dir string, args ...string) (string, error) {
	if len(args) == 2 && args[0] == "rev-parse" && args[1] == "HEAD" {
		return GitOutput(dir, args...)
	}
	return "", ErrNotProbed
}

// CheckProfile applies the declaration rules the CLI flags and the MCP
// arguments share: blank fields are unknown, a model source other than
// unknown needs a model id, a model id needs its source, and a declared
// model gets its observation time. A declaration is never inferred.
func CheckProfile(p *store.TeamProfile) error {
	for _, f := range []*string{&p.Platform, &p.PlatformVersion, &p.Model.Provider, &p.Model.ID, &p.Model.Version, &p.Model.Source} {
		if strings.TrimSpace(*f) == "" {
			*f = "unknown"
		}
	}
	switch p.Model.Source {
	case "runtime_reported", "operator_configured", "agent_reported", "unknown":
	default:
		return errors.New("model source must be runtime_reported, operator_configured, agent_reported or unknown")
	}
	if p.Model.Source != "unknown" && p.Model.ID == "unknown" {
		return errors.New("a declared model source needs a model id; an unknown model stays source unknown")
	}
	if p.Model.Source == "unknown" && p.Model.ID != "unknown" {
		return errors.New("a model id needs its source (who declared it: runtime_reported, operator_configured or agent_reported)")
	}
	if p.Model.ID != "unknown" {
		p.Model.ObservedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return nil
}

// setup carries one run: the resolved checkout, the report being built
// and the state file being reconciled.
type setup struct {
	env       Env
	opts      *Options
	sel       *taskcred.Selection
	identity  identity
	report    *Report
	state     *teamstate.State
	statePath string
	// continueOnly is the `teams continue` mode: the saved membership is
	// verified, resumed or reported as ended, but never replaced by a new
	// join (a pending join's replay is not a new join).
	continueOnly bool
	// previousBase is the base commit the saved state held before this run
	// touched it: what the reconciliation line compares HEAD with, kept
	// apart from the baseline that verifying or resuming advances.
	previousBase string
	// proc is the process context this run resolved, once.
	proc *processctx.Result
}

type identity struct {
	UserID       string `json:"user_id"`
	TokenID      string `json:"token_id"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	Scope        string `json:"scope"`
	TaskWrite    bool   `json:"task_write"`
	TasksEnabled bool   `json:"tasks_enabled"`
}

func (s *setup) check(name, level, detail, fix string) {
	s.report.Checks = append(s.report.Checks, Check{Name: name, Level: level, Detail: detail, Fix: fix})
}

func (s *setup) fail(name, detail, fix string) bool {
	s.check(name, "fail", detail, fix)
	if fix != "" {
		s.report.Next = append(s.report.Next, fix)
	}
	return false
}

// run performs the checks in order and stops at the first blocking one.
// It returns true only when a verified session exists at the end.
func (s *setup) run() bool {
	if !s.checkBinding() || !s.checkIntegration() || !s.checkIdentity() || !s.checkHub() || !s.checkEnrollment() {
		return false
	}
	s.checkProcess()
	s.checkBaseCommit()
	s.statePath = teamstate.Path(s.env.Root, s.sel.Repo)
	s.report.StateFile = s.statePath
	return s.reconcile()
}

func (s *setup) continueSaved(want string) bool {
	if !s.checkBinding() || !s.checkIdentity() || !s.checkHub() {
		return false
	}
	s.checkBaseCommit()
	s.statePath = teamstate.Path(s.env.Root, s.sel.Repo)
	s.report.StateFile = s.statePath
	saved := s.loadState()
	if saved == nil {
		s.member(memberNotVerified, "no saved membership for this checkout and credential")
		return s.fail("session", "no saved membership for this checkout and credential", "join on purpose with /join_team TEAM ROLE (aimem teams setup); a restart never joins by itself")
	}
	if want != "" && saved.Team != want && saved.TeamID != want {
		return s.fail("session", fmt.Sprintf("the saved membership is in team %q (%s), not %q", saved.Team, saved.TeamID, want), "re-run without a team, or with the saved one")
	}
	s.opts.Team, s.opts.Role, s.opts.Profile = saved.Team, saved.Role, saved.Profile
	s.report.Team, s.report.Role = saved.Team, saved.Role
	s.state, s.previousBase = saved, saved.BaseCommit
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

// checkBinding resolves the checkout's binding and credential selection
// strictly: a broken .aimem.json, a missing hub or an unusable credential
// is a stop with the step that fixes it, never a trip to another hub and
// never a replaced token for a credential this process merely cannot
// open.
func (s *setup) checkBinding() bool {
	sel, err := taskcred.Resolve(s.env.Dir, s.env.Root)
	if err != nil {
		// The state root and the account matter here: a hub entry or a
		// credential that another account installed under its own state
		// root is absent from this process's view, not missing.
		return s.fail("binding", fmt.Sprintf("%s (state root %s, running as %s)", err.Error(), s.env.Root, s.report.RunAs), CredentialFix(err, s.report.RunAs))
	}
	s.sel = sel
	s.check("binding", "ok", fmt.Sprintf("project %s, hub %s (%s), credential %s", sel.Project, sel.HubName, sel.Hub.URL, sel.Source), "")
	return true
}

// CredentialFix names the step that repairs a binding failure for the
// process running as runAs. A malformed or rebound credential is
// reinstalled. One this process cannot read or decrypt exists and may
// well be valid: it belongs to another OS account (the sandboxed shell
// under the checkout owner's credential, another user's DPAPI key), and
// the step is to run onboarding as that account, which the checkout's
// local MCP process does; replacing the token would leave the owner's
// copy in place and add a second one. A missing credential or hub entry
// gets the same caveat first, because another account's state root is
// invisible from here and looks exactly like nothing installed.
func CredentialFix(err error, runAs string) string {
	msg := err.Error()
	otherAccount := "if the checkout was set up by another account, run onboarding as that account (the local aimem MCP process of this checkout runs there) instead of installing anything as " + runAs + "; otherwise "
	switch taskcred.Classify(err) {
	case taskcred.ClassDenied:
		return "the credential exists but this process (" + runAs + ") is not allowed to read it: run onboarding as the account that installed it (the local aimem MCP process of this checkout runs there) rather than replacing the token; reinstall with `aimem task-token set` only if this account is meant to hold its own credential"
	case taskcred.ClassDecrypt:
		return "the credential is protected for another OS account than this process (" + runAs + "), or was not written on this machine: run onboarding as the account that installed it (the local aimem MCP process of this checkout runs there) rather than replacing the token; reinstall with `aimem task-token set` only if this account is meant to hold its own credential"
	case taskcred.ClassMissing:
		if strings.Contains(msg, "hub task-token") {
			return otherAccount + "run `aimem hub task-token <hub> <ordinary-token>` for the OS user, or require a project-local credential with `aimem task-token set`"
		}
		return otherAccount + "run `aimem task-token set` in this checkout with the member's project-scoped token on stdin (secret never on the command line)"
	case taskcred.ClassMalformed, taskcred.ClassRebound:
		return "run `aimem task-token set` in this checkout with the member's project-scoped token on stdin (secret never on the command line)"
	case taskcred.ClassConfig:
		return "fix the credential location named above (aimem state-root; the directory must be a private directory outside the checkout), then re-run"
	}
	switch {
	case strings.Contains(msg, "aimem hub add"):
		return otherAccount + "configure the hub named in .aimem.json with `aimem hub add`"
	case strings.Contains(msg, ".aimem.json"):
		return "repair .aimem.json (valid JSON; task_credential may only be \"local\")"
	}
	return "fix the checkout binding or credential, then re-run"
}

// processUser names the OS account this process runs as, for the report
// and for the credential diagnostics; the name only, never a secret.
func processUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	for _, k := range []string{"USER", "USERNAME"} {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return "unknown user"
}

// checkIntegration verifies the client wiring of the checkout and repairs
// only what aimem owns. Unreadable files and project-level checkpoint
// hooks block; everything else is reported and the run continues.
func (s *setup) checkIntegration() bool {
	o := s.opts.Wiring
	o.Home, _ = os.UserHomeDir()
	o.Commands = true
	rep := wiring.Check(s.sel.Repo, o)
	s.report.Wiring = &rep
	for _, f := range rep.Findings {
		detail := f.Detail
		if f.Repaired {
			detail = "repaired: " + detail
		}
		s.check("wiring "+f.File, f.Level, detail, f.Fix)
	}
	if len(rep.Clients) == 0 {
		s.check("clients", "warn", "no agent client (claude, codex, opencode) found on PATH; skill locations are reported for none", "")
	} else {
		var parts []string
		for _, c := range rep.Clients {
			parts = append(parts, c.Name+" "+c.Version)
		}
		s.check("clients", "ok", "on PATH: "+strings.Join(parts, ", "), "")
	}
	if rep.Failed() {
		s.report.Next = append(s.report.Next, "fix the blocking integration findings above, then re-run")
		return false
	}
	return true
}

// checkIdentity is the one read-only probe an ordinary token has: who the
// credential is, its scope, whether it may write this project and whether
// tasks are on. Enrollment is not visible here; join reveals it.
func (s *setup) checkIdentity() bool {
	status, body, err := s.hubGet("/v1/access/identity?project=" + url.QueryEscape(s.sel.Project))
	if err != nil {
		return s.fail("identity", "hub unreachable: "+err.Error(), "check the hub URL and network, then re-run")
	}
	switch status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return s.fail("identity", "the selected credential was rejected (HTTP 401): expired, revoked or wrong hub", "obtain a valid member token from the operator and install it with `aimem task-token set` (project-local) or `aimem hub task-token` (per hub); no fallback is attempted")
	case http.StatusNotFound:
		return s.fail("identity", fmt.Sprintf("project %s is unknown to hub %s", s.sel.Project, s.sel.HubName), "check the project in .aimem.json and the hub binding")
	default:
		return s.fail("identity", fmt.Sprintf("identity probe HTTP %d: %s", status, hubErrorText(body)), "")
	}
	if json.Unmarshal(body, &s.identity) != nil {
		return s.fail("identity", "identity response is not the expected JSON", "upgrade the hub; team setup needs the user-token model")
	}
	id := s.identity
	if id.Role != "user" {
		return s.fail("identity", fmt.Sprintf("credential %q has role %s; team sessions require an ordinary user token", id.Name, id.Role), "install the member's ordinary token; never a checkpoint or admin credential")
	}
	if s.sel.Source == "project-local" && id.Scope != "project" {
		return s.fail("identity", "a project-local credential must be project-scoped", "ask the operator for a project-scoped token (`aimem access token-issue`), then `aimem task-token set`")
	}
	if id.Scope == "read-only" {
		return s.fail("identity", "read-only tokens cannot join a team", "ask the operator for a write token for user "+id.Name)
	}
	if !id.TaskWrite {
		s.report.Handoff = Handoff(s.sel.Project, s.opts.Team, s.opts.Role, id.Name, id.UserID, true)
		return s.fail("identity", fmt.Sprintf("user %s has no current write grant on project %s", id.Name, s.sel.Project), "operator: grant the project to this user (see the operator handoff), then re-run")
	}
	if !id.TasksEnabled {
		return s.fail("identity", "tasks are not enabled for this project", "operator, on the hub host: `aimem tasks on -p "+s.sel.Project+"`, then re-run")
	}
	s.check("identity", "ok", fmt.Sprintf("user %s, scope %s, write granted, tasks enabled", id.Name, id.Scope), "")
	return true
}

// checkHub reads the unauthenticated status document for the hub version.
// A release older than the protocol loop is a stop; an undated build
// ("dev") is reported and left to the protocol check on the first team
// response.
func (s *setup) checkHub() bool {
	status, body, err := s.hubGet("/v1/status")
	if err != nil || status != http.StatusOK {
		return s.fail("hub", "status document unavailable: hub too old or unreachable", "upgrade the hub to v0.7.0 or later")
	}
	var st struct {
		Version string `json:"version"`
		HubName string `json:"hub_name"`
	}
	json.Unmarshal(body, &st)
	if st.Version != "" && st.Version != "dev" && !adapter.VersionAtLeast(st.Version, Protocol[0], Protocol[1], Protocol[2]) {
		return s.fail("hub", fmt.Sprintf("hub %s is %s; the team protocol loop needs v%d.%d.%d or later", st.HubName, st.Version, Protocol[0], Protocol[1], Protocol[2]), "upgrade the hub")
	}
	detail := fmt.Sprintf("hub %s version %s, client %s", st.HubName, orUnknown(st.Version), s.env.Version)
	if s.env.Version == "dev" || !adapter.VersionAtLeast(s.env.Version, Protocol[0], Protocol[1], Protocol[2]) {
		s.check("hub", "warn", detail+"; this client build is not a dated release at or after v0.7.0", "")
	} else {
		s.check("hub", "ok", detail, "")
	}
	return true
}

// checkEnrollment asks the hub which teams enroll this credential, the one
// read possible before a session exists, so a missing enrollment or a
// missing coordinator eligibility is named before the join instead of
// surfacing as the join's undifferentiated refusal. A hub without the
// route (older than this client) is reported and the join decides.
func (s *setup) checkEnrollment() bool {
	status, body, err := s.teamCall("team_list", map[string]any{})
	if err != nil {
		return s.fail("enrollment", "could not list this credential's teams: "+err.Error(), "retry when the hub is reachable")
	}
	// A hub older than the route answers 404, or 403 from the gate that
	// refuses ordinary tokens every route it does not list; the identity
	// check already passed, so that 403 is the route's absence, not a
	// grant refusal.
	if status == http.StatusNotFound || status == http.StatusForbidden && strings.Contains(hubErrorText(body), "not authorized for this endpoint") {
		s.check("enrollment", "warn", "this hub does not list enrolled teams (older hub); the join reports enrollment refusals", "")
		return true
	}
	if status != http.StatusOK {
		return s.fail("enrollment", "team listing refused: "+hubOutcome(status, body, nil), "")
	}
	var res struct {
		Protocol int `json:"protocol_version"`
		Teams    []struct {
			ID                string `json:"id"`
			Name              string `json:"name"`
			Coordinator       bool   `json:"coordinator"`
			CoordinatorActive bool   `json:"coordinator_active"`
		} `json:"teams"`
	}
	if json.Unmarshal(body, &res) != nil || res.Protocol != 1 {
		return s.fail("enrollment", "team listing is not team protocol 1", "upgrade to a compatible client/hub pair")
	}
	var names []string
	for _, t := range res.Teams {
		names = append(names, fmt.Sprintf("%s (%s)", t.Name, t.ID))
		if t.Name != s.opts.Team && t.ID != s.opts.Team {
			continue
		}
		if s.opts.Role == "coordinator" && !t.Coordinator {
			s.report.Handoff = Handoff(s.sel.Project, s.opts.Team, s.opts.Role, s.identity.Name, s.identity.UserID, false)
			return s.fail("enrollment", fmt.Sprintf("user %s is enrolled in team %q but not as coordinator-eligible", s.identity.Name, t.Name), "operator: set coordinator:true on this user's enrollment (see the operator handoff), or join as worker")
		}
		detail := fmt.Sprintf("user %s is enrolled in team %q (%s)", s.identity.Name, t.Name, t.ID)
		if t.Coordinator {
			detail += ", coordinator-eligible"
		}
		if t.CoordinatorActive {
			detail += "; a coordinator session is active now"
		}
		s.check("enrollment", "ok", detail, "")
		return true
	}
	s.report.Handoff = Handoff(s.sel.Project, s.opts.Team, s.opts.Role, s.identity.Name, s.identity.UserID, false)
	if len(names) == 0 {
		return s.fail("enrollment", fmt.Sprintf("user %s is not enrolled in any team of project %s", s.identity.Name, s.sel.Project), "operator: enroll this user in team "+s.opts.Team+" (see the operator handoff), then re-run; agents cannot self-enroll")
	}
	return s.fail("enrollment", fmt.Sprintf("user %s is not enrolled in team %q; enrolled in: %s", s.identity.Name, s.opts.Team, strings.Join(names, ", ")), "re-run with one of those teams, or operator: enroll this user in "+s.opts.Team+" (see the operator handoff)")
}

// checkProcess reports the selected process and its required skills. It
// never blocks the join: the skills are needed at the review and delivery
// steps, and the session-start hook repeats this notice.
func (s *setup) checkProcess() {
	if s.env.Process == nil {
		s.check("process", "warn", "process context not checked by this entry point; the session-start hook reports it", "")
		return
	}
	r := s.env.Process(s.env.Dir, s.sel.Project)
	s.proc = r
	set := r.Set
	if set == nil {
		s.check("process", "warn", fmt.Sprintf("process context %s: %s", r.State, r.Detail), r.Fix)
		return
	}
	if len(set.Manifest.Skills) == 0 {
		s.check("process", "ok", "process set selected; no required skills", "")
		return
	}
	home, _ := os.UserHomeDir()
	if s.report.Wiring == nil || len(s.report.Wiring.Clients) == 0 {
		// No client on PATH to judge for: the machine-wide locations decide.
		installed := process.SkillInstalled(s.env.Dir, home)
		var missing []string
		for _, name := range set.Manifest.Skills {
			if !installed(name) {
				missing = append(missing, name)
			}
		}
		detail := "process set selected; required skills: " + strings.Join(set.Manifest.Skills, ", ")
		if len(missing) > 0 {
			s.check("process", "warn", detail+"; NOT FOUND on this machine: "+strings.Join(missing, ", "), "install the missing skills before the step that needs them (the review gate)")
			return
		}
		s.check("process", "ok", detail, "")
		return
	}
	// Per client: a skill counts only where that client reads it, and only
	// as a directory holding SKILL.md.
	s.report.Wiring.Skills = wiring.Skills(s.sel.Repo, home, s.report.Wiring.Clients, set.Manifest.Skills)
	var missing []string
	for _, st := range s.report.Wiring.Skills {
		if st.Path == "" {
			missing = append(missing, st.Skill+" for "+st.Client)
		}
	}
	detail := "process set selected; required skills: " + strings.Join(set.Manifest.Skills, ", ")
	if len(missing) > 0 {
		s.check("process", "warn", detail+"; NOT FOUND: "+strings.Join(missing, ", "), "install the missing skills where that client reads them before the step that needs them (the review gate); see the skills list below")
		return
	}
	s.check("process", "ok", detail+" (found for every client on PATH)", "")
}

// checkBaseCommit records HEAD: the base commit a worker's attempt and a
// coordinator's offer are recorded against. Not a Git checkout is a fact,
// not a failure; so is a host that runs no git probe.
func (s *setup) checkBaseCommit() {
	if s.env.Git == nil {
		s.check("base commit", "warn", "not probed by this entry point: no base commit to record", "")
		return
	}
	head, err := s.env.Git(s.env.Dir, "rev-parse", "HEAD")
	if err != nil {
		s.check("base commit", "warn", "not a Git checkout (or git unavailable): no base commit to record", "")
		return
	}
	s.report.BaseCommit = head
	dirty, err := s.env.Git(s.env.Dir, "status", "--porcelain")
	state := "clean"
	switch {
	case err != nil:
		state = "(working tree not probed)"
	case dirty != "":
		state = "with uncommitted changes"
	}
	s.check("base commit", "ok", head[:min(12, len(head))]+" "+state, "")
}

// reconcile decides between verifying the saved session, resuming it,
// and joining afresh, then enters the role.
func (s *setup) reconcile() bool {
	saved := s.loadState()
	if saved != nil && !s.opts.ProfileSet {
		// A bare re-run keeps the declaration it made before instead of
		// downgrading a fresh join to "unknown".
		s.opts.Profile, s.opts.PlatformSet = saved.Profile, saved.Profile.Platform != "unknown"
	}
	if saved != nil && s.opts.NewSession {
		what := "saved session " + saved.SessionID
		if saved.SessionID == "" {
			what = "the unconfirmed join (if it landed, that session stays open on the hub until an operator recovers it)"
		}
		s.check("session", "ok", fmt.Sprintf("discarding %s at your request (--new-session)", what), "")
		saved = nil
	}
	if saved != nil {
		s.previousBase = saved.BaseCommit
		// The saved record, confirmed or pending, binds this checkout to one
		// team and role; a different request is a separate decision, never a
		// silent second membership.
		held := "already holds a " + saved.Role + " session (" + saved.SessionID + ")"
		if saved.SessionID == "" {
			held = "has an unconfirmed " + saved.Role + " join pending"
		}
		if saved.Role != s.opts.Role {
			return s.fail("session", fmt.Sprintf("this checkout %s and a role cannot change in place", held), "re-run with the saved role to verify or replay it, leave that session first (aimem teams leave / team_leave with the saved handle), or re-run with --new-session for a separate "+s.opts.Role+" session")
		}
		if saved.Team != s.opts.Team && saved.TeamID != s.opts.Team {
			return s.fail("session", fmt.Sprintf("this checkout %s in team %q", held, saved.Team), "re-run with that team, leave that session first, or re-run with --new-session")
		}
		s.state = saved
		switch {
		case saved.SessionID != "" && saved.ResumeKey != "":
			// A resume was sent and its result never landed here. The hub
			// replays the original result for the original key even after
			// the generation moved, so it must run before any read with the
			// old handle.
			return s.resume("replaying an unconfirmed resume")
		case saved.SessionID != "":
			return s.verifySaved()
		case saved.JoinKey != "":
			// A join was sent and its result never landed here: the identical
			// request (key, team, role, profile) replays instead of creating
			// a second session.
			return s.join(saved.JoinKey, saved.Team, saved.Profile)
		}
	}
	return s.join("", s.opts.Team, s.opts.Profile)
}

// verifySaved reads the roster with the saved handle: the only
// side-effect-free liveness check the protocol offers. 200 with our row
// active means already joined; a left row means join afresh; 409 means the
// handle is closed or another process advanced it, and nothing here takes
// that over without --resume.
func (s *setup) verifySaved() bool {
	st := s.state
	me, _, status, body, err := s.roster(st.TeamID, st.SessionID, st.Generation)
	if err != nil {
		return s.fail("session", "could not verify the saved session: "+err.Error(), "retry; the saved handle is kept")
	}
	switch {
	case status == http.StatusOK && me != nil && me.State == "active":
		// Existing membership routes to the resume flow. A suspect session
		// (no heartbeat within the hub's window) is the restart case and is
		// resumed, fencing whatever the old process left; a session that is
		// still heartbeating may belong to a live process and is only
		// verified unless --resume says otherwise. No generation churn on a
		// repeated invocation from the same live session.
		if s.opts.Resume || me.Suspect {
			why := "--resume requested"
			if !s.opts.Resume {
				why = "no heartbeat since " + me.LastSeenAt + ", so the previous process is presumed gone"
			}
			return s.resume(why)
		}
		s.check("session", "ok", fmt.Sprintf("already joined as %s: session %s, generation %d, last seen %s (no second join; --resume would fence this live handle)", me.Role, me.ID, me.Generation, me.LastSeenAt), "")
		st.VerifiedAt = time.Now().UTC().Format(time.RFC3339)
		if s.report.BaseCommit != "" {
			st.BaseCommit = s.report.BaseCommit // the baseline advances; previousBase keeps the old one for the report
		}
		s.saveState()
		return s.enterRole(me)
	case status == http.StatusOK && me != nil:
		s.check("session", "ok", fmt.Sprintf("saved session %s has %s; joining afresh", me.ID, me.State), "")
		s.state = nil
		return s.join("", s.opts.Team, s.opts.Profile)
	case status == http.StatusOK:
		// The hub serves the roster only to a bound active session, so a
		// successful read without our row means the read was incomplete,
		// never that the membership is gone.
		return s.fail("session", fmt.Sprintf("saved session %s was not found in the roster pages read; the membership is kept and nothing was joined", st.SessionID), "retry; if this persists, read the roster with the MCP handle from the state file")
	case status == http.StatusConflict:
		// The hub refuses every command on a closed session and on a stale
		// generation alike, so the saved handle cannot tell an explicit leave
		// from another live process. A coordinator may join afresh because
		// the hub arbitrates: an occupied slot means the old session is
		// live, success means it left. A worker's fresh join is never
		// refused, so a duplicate would be silent; that choice stays explicit.
		if st.Role == "coordinator" {
			s.check("session", "warn", fmt.Sprintf("saved handle (session %s, generation %d) is closed or was advanced by another process; joining afresh, which the hub refuses while that session holds the slot", st.SessionID, st.Generation), "")
			s.state = nil
			return s.join("", s.opts.Team, s.opts.Profile)
		}
		s.member(memberRefused, "the saved handle is closed or was advanced by another process")
		return s.fail("session", fmt.Sprintf("saved handle (session %s, generation %d) is closed or was advanced by another process: %s", st.SessionID, st.Generation, hubErrorText(body)), "if that process is gone (or you left explicitly outside this command), re-run with --new-session for a separate worker session; nothing was taken over")
	case status == http.StatusForbidden:
		s.report.Handoff = Handoff(s.sel.Project, s.opts.Team, s.opts.Role, s.identity.Name, s.identity.UserID, false)
		s.member(memberRefused, "the saved session is no longer authorized")
		return s.fail("session", "the saved session is no longer authorized: "+hubErrorText(body), "operator: restore the enrollment or grant (see the operator handoff); an enrollment revoked mid-session blocks even leave until an admin handoff")
	default:
		return s.fail("session", fmt.Sprintf("roster read HTTP %d: %s", status, hubErrorText(body)), "")
	}
}

// resume fences the saved handle: the same session with a new generation
// (and coordinator generation); a worker's reserved attempt follows.
func (s *setup) resume(why string) bool {
	st := s.state
	// The key is saved before the call: an unconfirmed resume is retried
	// with the same key and replays its original result.
	if st.ResumeKey == "" {
		st.ResumeKey = uuidv7.New()
		if err := s.saveState(); err != nil {
			st.ResumeKey = ""
			return s.fail("session", "cannot record the resume key before sending it: "+err.Error(), "make the state root writable (aimem state-root) and re-run; nothing was sent")
		}
	}
	status, body, err := s.teamCall("team_resume", map[string]any{"team": st.TeamID, "session_id": st.SessionID, "generation": st.Generation, "idempotency_key": st.ResumeKey})
	if err != nil {
		return s.fail("session", "resume not confirmed (hub unreachable): "+err.Error(), "re-run when the hub is reachable; the same resume is retried with its original key")
	}
	if status != http.StatusOK {
		st.ResumeKey = ""
		s.saveState()
		s.member(memberRefused, "the hub refused to resume the saved session")
		return s.fail("session", fmt.Sprintf("resume refused (HTTP %d): %s", status, hubErrorText(body)), "the saved handle is stale or closed; nothing was taken over. Re-run to verify, or with --new-session")
	}
	me, ok := s.sessionOf(body)
	if !ok {
		return false // the key stays recorded; the next run replays it
	}
	st.ResumeKey = ""
	s.recordSession(me)
	s.check("session", "ok", fmt.Sprintf("resumed session %s (%s): generation %d; the old handle is fenced. Reconcile before retrying anything: which commands the old process acknowledged, which files changed, whether a child process still runs", me.ID, why, me.Generation), "")
	return s.enterRole(me)
}

// join creates the session. The idempotency key is saved before the
// request so an uncertain result is retried with the same key, which
// replays the original session instead of creating a second one.
func (s *setup) join(key, team string, profile store.TeamProfile) bool {
	if s.continueOnly && key == "" {
		s.member(memberEnded, "the saved membership has ended")
		return s.fail("session", "the saved membership has ended (the session left or was closed); nothing was joined", "join again on purpose with /join_team "+team+" "+s.opts.Role+" (aimem teams setup); a restart never re-joins by itself")
	}
	if !s.opts.PlatformSet && profile.Platform == "unknown" {
		s.check("profile", "warn", "platform not given; declared as unknown (the entry point that knows the client should declare it)", "")
	}
	retry := key != ""
	if key == "" {
		key = uuidv7.New()
	}
	s.state = &teamstate.State{Version: 1, Repo: s.sel.Repo, Project: s.sel.Project, HubName: s.sel.HubName, HubURL: s.sel.Hub.URL, TokenID: s.identity.TokenID, User: s.identity.Name, Team: team, Role: s.opts.Role, Profile: profile, JoinKey: key}
	if err := s.saveState(); err != nil {
		return s.fail("session", "cannot record the join key before sending it: "+err.Error(), "make the state root writable (aimem state-root) and re-run; nothing was sent")
	}
	status, body, err := s.teamCall("team_join", map[string]any{"team": team, "role": s.opts.Role, "profile": profile, "idempotency_key": key})
	if err != nil {
		return s.fail("session", "join not confirmed (hub unreachable): "+err.Error(), "re-run when the hub is reachable; the same join is retried with its original key")
	}
	if status != http.StatusCreated && status != http.StatusOK {
		msg := hubErrorText(body)
		if status == http.StatusConflict && retry && strings.Contains(msg, "idempotency key") {
			// The replay was built from the saved request, so this means the
			// record no longer matches what the hub holds for that key. The
			// original session, if it exists, must not be shadowed by a new one.
			return s.fail("session", "the unconfirmed join could not be replayed: "+msg, "an operator can find the session for this credential in the team audit (aimem teams events); re-run with --new-session only to abandon it")
		}
		// A definite refusal: the hub created nothing, so the pending key is
		// dropped and the next run sends a fresh join with current flags.
		s.forgetJoin()
		s.member(memberRefused, fmt.Sprintf("the hub refused the join (HTTP %d)", status))
		switch {
		case status == http.StatusConflict && strings.Contains(msg, "coordinator slot"):
			return s.fail("session", "the coordinator slot is occupied: "+msg, "the current coordinator must leave or hand off (its own live handle, or an admin handoff with reconciliation on the hub host); a slot is never freed by a timeout. If another checkout on this machine holds that session, run setup there with --resume and leave from it")
		case status == http.StatusForbidden && strings.Contains(msg, "enrollment"):
			s.report.Handoff = Handoff(s.sel.Project, s.opts.Team, s.opts.Role, s.identity.Name, s.identity.UserID, false)
			return s.fail("session", fmt.Sprintf("user %s is not enrolled in team %q as %s (the hub does not distinguish a missing enrollment from missing coordinator eligibility)", s.identity.Name, s.opts.Team, s.opts.Role), "operator: enroll this user (see the operator handoff), then re-run; agents cannot self-enroll")
		case status == http.StatusNotFound:
			s.report.Handoff = Handoff(s.sel.Project, s.opts.Team, s.opts.Role, s.identity.Name, s.identity.UserID, false)
			return s.fail("session", fmt.Sprintf("team %q not found in project %s", s.opts.Team, s.sel.Project), "check the team name or ID; operator: create the team (see the operator handoff)")
		default:
			return s.fail("session", fmt.Sprintf("join refused (HTTP %d): %s", status, msg), "")
		}
	}
	me, ok := s.sessionOf(body)
	if !ok {
		return false
	}
	s.state.JoinKey = ""
	s.state.JoinedAt = time.Now().UTC().Format(time.RFC3339)
	s.recordSession(me)
	s.check("session", "ok", fmt.Sprintf("joined as %s: session %s, generation %d", me.Role, me.ID, me.Generation), "")
	return s.enterRole(me)
}

// enterRole is the last step: the coordinator reads the roster, the worker
// announces availability, reads its reserved attempt and its inbox once,
// bounded. Every request here is checked: the membership is already
// recorded, so a refusal is reported as the failure it is and the next run
// verifies the saved handle instead of claiming an entry that did not happen.
func (s *setup) enterRole(me *Session) bool {
	s.report.Session = me
	handle := fmt.Sprintf("team_id %s, session_id %s, generation %d", me.TeamID, me.ID, me.Generation)
	_, roster, status, body, err := s.roster(me.TeamID, me.ID, me.Generation)
	if err != nil || status != http.StatusOK {
		return s.fail("roster", "roster not read after the session was recorded: "+hubOutcome(status, body, err), "membership is saved; re-run to verify it (enrollment or grant changes show up here as a refusal)")
	}
	s.report.Roster = roster
	s.member(memberActive, fmt.Sprintf("%s session %s, generation %d, verified with the hub in this run", me.Role, me.ID, me.Generation))
	s.evaluateContext(me.Role)
	rd := s.report.Readiness
	if me.Role == "coordinator" {
		s.readinessNext(me.Role)
	} else {
		// The hub offers work to, and lets accept work from, only an
		// available worker: a worker that is not ready says unavailable.
		// A join creates the session available, so between the join and
		// this heartbeat an offer can land; it is listed below, and it is
		// declined, never accepted.
		availability := "available"
		if !rd.ReadyForWork {
			availability = "unavailable"
		}
		status, body, err = s.teamCall("team_heartbeat", map[string]any{"team": me.TeamID, "session_id": me.ID, "generation": me.Generation, "availability": availability, "idempotency_key": uuidv7.New()})
		if err != nil || status != http.StatusOK {
			if !rd.ReadyForWork {
				s.report.Next = append(s.report.Next, "NOT ready for work ("+rd.notReady()+") and the unavailable announcement was not accepted: the hub may still offer you work (a join starts available). Accept nothing and decline any offer with this reason")
			}
			return s.fail("availability", "heartbeat ("+availability+") not accepted: "+hubOutcome(status, body, err), "membership is saved, nothing was left or released; the hub still shows the availability it last accepted. Re-run to verify it")
		}
		s.readinessNext(me.Role)
		if rd.ReadyForWork {
			s.check("availability", "ok", "announced available", "")
		} else {
			s.check("availability", "warn", "announced unavailable: not ready for work ("+rd.notReady()+"), so the hub will neither offer work to this session nor let it accept any", "")
		}
		if !s.readReserved(me) {
			return false
		}
		s.handleUnreadyAttempt(me)
		s.reservedNext()
	}
	if !s.readInbox(me) {
		return false
	}
	if me.Role == "coordinator" {
		s.report.Next = append(s.report.Next,
			onceReady(rd, "coordinator playbook (docs/TEAM-PLAYBOOKS.md) step 2: select work the process allows; offer one attempt per task with suitability and cost rationale"),
			"read the roster above: availability, reported model and its source, declared capabilities; unknown stays unknown",
			"heartbeat every 30 s while active (team_heartbeat); read the inbox with a bounded wait after every command you issue",
			"MCP handle: "+handle)
		return true
	}
	if s.report.Reserved == nil {
		s.report.Next = append(s.report.Next, "worker playbook (docs/TEAM-PLAYBOOKS.md) step 2: wait for an addressed offer; never select, claim or edit backlog tasks while joined, even while the coordinator is disconnected")
	}
	s.report.Next = append(s.report.Next,
		onceReady(rd, "poll team_inbox (after the cursor above, wait_seconds up to 25), team_ack only what you have read and acted on, then accept or decline an offer with a reason"),
		"heartbeat every 30 s while active",
		"MCP handle: "+handle)
	return true
}

// readReserved reads the attempt the hub holds for this session, the
// authoritative ownership record, and the assignment behind it; the
// attempt's state, not any local note, says what comes next.
func (s *setup) readReserved(me *Session) bool {
	status, body, err := s.teamCall("team_reserved", map[string]any{"team": me.TeamID, "session_id": me.ID, "generation": me.Generation})
	switch {
	case err == nil && status == http.StatusNotFound:
		s.check("reserved", "ok", "no attempt reserved for this session", "")
		return true
	case err != nil || status != http.StatusOK:
		return s.fail("reserved", "reserved attempt not read: "+hubOutcome(status, body, err), "membership is saved; re-run to verify it")
	}
	var res struct {
		Assignment *Reserved `json:"assignment"`
		Reserved
	}
	json.Unmarshal(body, &res)
	r := res.Reserved
	if res.Assignment != nil {
		r = *res.Assignment
	}
	status, body, err = s.teamCall("team_assignment", map[string]any{"team": me.TeamID, "session_id": me.ID, "generation": me.Generation, "attempt": r.ID})
	if err != nil || status != http.StatusOK {
		s.check("assignment", "warn", "assignment "+r.ID+" not read: "+hubOutcome(status, body, err)+"; the reserved state above stands, read it with team_assignment before acting", "")
	} else {
		var full struct {
			Assignment struct {
				State        string `json:"state"`
				UpdatedAt    string `json:"updated_at"`
				Reason       string `json:"reason"`
				Requirements struct {
					Title string `json:"title"`
				} `json:"requirements"`
			} `json:"assignment"`
		}
		if json.Unmarshal(body, &full) == nil {
			r.State, r.UpdatedAt, r.Reason, r.Title = orKeep(full.Assignment.State, r.State), full.Assignment.UpdatedAt, full.Assignment.Reason, full.Assignment.Requirements.Title
		}
	}
	r.Next = AttemptNext(r.State)
	s.report.Reserved = &r
	s.check("reserved", "ok", fmt.Sprintf("attempt %s on task %s (%s) is %s for this session", r.ID, r.TaskID, orUnknown(r.Title), r.State), "")
	return true
}

// AttemptNext is the worker playbook's step for one attempt state.
func AttemptNext(state string) string {
	switch state {
	case "OFFERED":
		return "read it with team_assignment, then team_accept or team_decline with a reason; nothing is authorized until accepted"
	case "RUNNING":
		return "continue the accepted work in your isolated worktree from the recorded base commit; send progress at milestones, block (team_block) if you cannot proceed, submit (team_submit) with base commit, candidate commit, validation and evidence when done"
	case "BLOCKED":
		return "the need you named is open: read the inbox for the answer, then team_resume_work when it is met"
	case "STOP_REQUESTED":
		return "the coordinator asked you to stop: finish or abandon the current step safely, reconcile local execution, then team_stopped; never continue after acknowledging"
	case "STOPPED":
		return "you acknowledged a stop; wait for the coordinator's close-stop in the inbox and do nothing on this task"
	case "SUBMITTED":
		return "your result is under review; wait for accept or rework in the inbox (rework arrives as a new offer)"
	}
	return "read it with team_assignment and act per docs/TEAM-PLAYBOOKS.md"
}

// reconcileLine is the restart checklist: what to establish before any
// command is retried, because the hub cannot see local effects.
func (s *setup) reconcileLine() string {
	line := "reconcile before retrying anything: the attempt state above is what the hub acknowledged"
	switch {
	case s.report.BaseCommit == "":
	case s.previousBase == "":
		line += "; no base commit was recorded at the last verification (membership from an older release); HEAD " + s.report.BaseCommit[:min(12, len(s.report.BaseCommit))] + " is recorded now"
	case s.previousBase == s.report.BaseCommit:
		line += "; HEAD is still the base recorded at the last verification, " + s.previousBase[:min(12, len(s.previousBase))]
	default:
		line += "; HEAD " + s.report.BaseCommit[:min(12, len(s.report.BaseCommit))] + " differs from the base recorded at the last verification, " + s.previousBase[:min(12, len(s.previousBase))] + " (your candidate, or a change to reconcile)"
	}
	if s.env.Git != nil {
		if dirty, err := s.env.Git(s.env.Dir, "status", "--porcelain"); err == nil && dirty != "" {
			line += "; the checkout has uncommitted changes"
		}
	}
	return line + "; check yourself whether a child process of the old session still runs; retry an uncertain command only with its original key and content"
}

// readInbox lists what this session has not acknowledged (one bounded
// read, cursor 0, no wait). Nothing is acknowledged here.
func (s *setup) readInbox(me *Session) bool {
	status, body, err := s.teamCall("team_inbox", map[string]any{"team": me.TeamID, "session_id": me.ID, "generation": me.Generation, "after": 0, "limit": 50, "wait_seconds": 0})
	if err != nil || status != http.StatusOK {
		return s.fail("inbox", "inbox not read: "+hubOutcome(status, body, err), "membership is saved; re-run to verify it")
	}
	var page struct {
		Messages []struct {
			ID        string `json:"id"`
			Sequence  int64  `json:"sequence"`
			Kind      string `json:"kind"`
			TaskID    string `json:"task_id"`
			AttemptID string `json:"attempt_id"`
			CreatedAt string `json:"created_at"`
			Profile   struct {
				Label string `json:"label"`
			} `json:"profile"`
			Lifecycle *struct {
				Operation string `json:"operation"`
				TaskID    string `json:"task_id"`
				AttemptID string `json:"attempt_id"`
				State     string `json:"state"`
			} `json:"lifecycle"`
			Payload struct {
				Text string `json:"text"`
			} `json:"payload"`
		} `json:"messages"`
		NextCursor int64 `json:"next_cursor"`
		HasMore    bool  `json:"has_more"`
	}
	json.Unmarshal(body, &page)
	inbox := &Inbox{Unacknowledged: len(page.Messages), NextCursor: page.NextCursor, HasMore: page.HasMore, Note: "one bounded read at cursor 0; nothing wakes an idle member and nothing here acknowledges. Poll team_inbox with wait_seconds up to 25 and team_ack only what you have read and acted on"}
	for _, m := range page.Messages {
		sm := Message{ID: m.ID, Sequence: m.Sequence, Kind: m.Kind, From: m.Profile.Label, TaskID: m.TaskID, AttemptID: m.AttemptID, CreatedAt: m.CreatedAt, Excerpt: excerpt(m.Payload.Text, 160)}
		if m.Lifecycle != nil {
			sm.From, sm.Operation, sm.State = "hub", m.Lifecycle.Operation, m.Lifecycle.State
			if sm.TaskID == "" {
				sm.TaskID = m.Lifecycle.TaskID
			}
			if sm.AttemptID == "" {
				sm.AttemptID = m.Lifecycle.AttemptID
			}
		}
		if sm.From == "" {
			sm.From = "unknown"
		}
		inbox.Messages = append(inbox.Messages, sm)
	}
	s.report.Inbox = inbox
	if len(page.Messages) == 0 {
		s.check("inbox", "ok", "no unacknowledged messages", "")
		return true
	}
	more := ""
	if page.HasMore {
		more = " (more pages after cursor " + fmt.Sprint(page.NextCursor) + ")"
	}
	s.check("inbox", "ok", fmt.Sprintf("%d unacknowledged message(s) waiting%s; listed below, none acknowledged", len(page.Messages), more), "")
	s.report.Next = append(s.report.Next, fmt.Sprintf("act on the %d unacknowledged message(s) above (offers, cancellations, reviews and questions are among them), then team_ack the ones you consumed; next cursor %d", len(page.Messages), page.NextCursor))
	return true
}

func orKeep(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func excerpt(s string, n int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= n {
		return string(r)
	}
	return string(r[:n]) + "..."
}

// roster pages the member list with the given handle until our own row is
// found or the pages end. It returns the row, the whole roster and the
// last HTTP result.
func (s *setup) roster(teamID, sessionID string, generation int64) (*Session, []Session, int, []byte, error) {
	var all []Session
	var me *Session
	after := ""
	for range 20 {
		args := map[string]any{"team": teamID, "session_id": sessionID, "generation": generation, "limit": 100}
		if after != "" {
			args["after"] = after
		}
		status, body, err := s.teamCall("team_members", args)
		if err != nil || status != http.StatusOK {
			return nil, nil, status, body, err
		}
		var res struct {
			Protocol   int       `json:"protocol_version"`
			Members    []Session `json:"members"`
			NextCursor string    `json:"next_cursor"`
			ServerTime string    `json:"server_time"`
		}
		if json.Unmarshal(body, &res) != nil || res.Protocol != 1 {
			return nil, nil, status, body, errors.New("unsupported team protocol; upgrade the compatible client/hub")
		}
		s.report.ServerTime = res.ServerTime
		for i := range res.Members {
			if res.Members[i].ID == sessionID {
				m := res.Members[i]
				me = &m
			}
		}
		all = append(all, res.Members...)
		if res.NextCursor == "" || len(res.Members) == 0 {
			return me, all, http.StatusOK, nil, nil
		}
		after = res.NextCursor
	}
	// A cursor is still open: the read is incomplete, and nothing may be
	// decided from a roster that has not shown every row.
	return nil, nil, 0, nil, errors.New("roster read incomplete: more pages than this command reads; nothing was decided from it")
}

// hubOutcome renders a team request's result for a failure line: the
// transport error, or the status with the hub's message.
func hubOutcome(status int, body []byte, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("HTTP %d: %s", status, hubErrorText(body))
}

func (s *setup) sessionOf(body []byte) (*Session, bool) {
	var res struct {
		Protocol int      `json:"protocol_version"`
		Session  *Session `json:"session"`
	}
	if json.Unmarshal(body, &res) != nil || res.Protocol != 1 || res.Session == nil || res.Session.ID == "" {
		s.fail("session", "the hub's session response is not team protocol 1", "upgrade to a compatible client/hub pair; do not emulate coordination with task writes")
		return nil, false
	}
	return res.Session, true
}

// forgetJoin drops a pending join record after a definite refusal; only
// an unconfirmed result (transport failure, unreadable reply) keeps the
// key for a replay.
func (s *setup) forgetJoin() {
	s.state = nil
	s.saveState()
}

func (s *setup) recordSession(me *Session) {
	st := s.state
	st.TeamID, st.SessionID, st.Generation, st.CoordinatorGeneration, st.ProfileRevision = me.TeamID, me.ID, me.Generation, me.CoordinatorGeneration, me.ProfileRevision
	st.VerifiedAt = time.Now().UTC().Format(time.RFC3339)
	if s.report.BaseCommit != "" {
		st.BaseCommit = s.report.BaseCommit
	}
	if err := s.saveState(); err != nil {
		s.check("state", "warn", "session state not saved: "+err.Error()+"; keep the handle from this report", "")
	}
}

// teamCall sends one session operation through the host's credential
// path, always for the bound project.
func (s *setup) teamCall(name string, args map[string]any) (int, []byte, error) {
	if s.env.TeamCall == nil {
		return 0, nil, errors.New("no hub session caller configured for this entry point")
	}
	args["project"] = s.sel.Project
	raw, _ := json.Marshal(args)
	ctx, cancel := context.WithTimeout(context.Background(), HubTimeout)
	defer cancel()
	return s.env.TeamCall(ctx, name, raw)
}

// hubGet is a bounded GET with the selected credential and its redirect
// policy (a project-local credential is bound to one exact URL).
func (s *setup) hubGet(path string) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), HubTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.sel.Hub.URL, "/")+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.sel.Token)
	client := s.sel.Hub.HTTPClient()
	if s.sel.Source == "project-local" {
		client = s.sel.Client()
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}

func hubErrorText(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	if t := strings.TrimSpace(string(body)); t != "" && len(t) < 200 {
		return t
	}
	return "no detail"
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// Handoff is the bounded operator request printed when authority is
// missing: exact commands for the hub host, with the identifiers this run
// observed and placeholders for the rest. It never carries a secret.
func Handoff(project, team, role, user, userID string, grantMissing bool) string {
	var b strings.Builder
	b.WriteString("Operator handoff (run on the hub host as the aimem service user; nothing below can be done from an agent checkout):\n")
	if user == "" {
		user = "<member-user>"
	}
	if userID == "" {
		userID = "<USER_ID>"
	}
	// This credential already exists and answered the identity probe, so
	// the member needs enrollment (and the grant, when missing), not a new
	// token: --no-token keeps the guided command from issuing one.
	fmt.Fprintf(&b, "  aimem teams provision add %s --team %q --member %s --role %s --no-token\n", project, team, userID, role)
	fmt.Fprintf(&b, "      # user %s (id %s): sets the project grant%s, enrolls it in team %q as %s; existing users, grants and enrollments are reused\n", user, userID, map[bool]string{true: " (missing now)", false: ""}[grantMissing], team, role)
	fmt.Fprintf(&b, "  aimem teams provision create %s --team %q --coordinator <coordinator-user> --expiry <RFC3339>   # only if the team does not exist yet\n", project, team)
	fmt.Fprintf(&b, "Then, in this checkout: aimem teams setup %q %s\n", team, role)
	return b.String()
}

// loadState returns the saved state only when it is bound to this exact
// repo, project, hub, URL and credential; anything else is ignored, not
// revived.
func (s *setup) loadState() *teamstate.State {
	st, err := teamstate.Load(s.statePath)
	if err != nil {
		s.check("state", "warn", "ignoring an unreadable session state file", "")
		return nil
	}
	if st == nil {
		return nil
	}
	if st.Repo != s.sel.Repo || st.Project != s.sel.Project || st.HubName != s.sel.HubName || st.HubURL != s.sel.Hub.URL || st.TokenID != s.identity.TokenID {
		s.check("state", "warn", "ignoring saved session state bound to another checkout, project, hub or credential", "")
		return nil
	}
	return st
}

func (s *setup) saveState() error {
	if s.state == nil {
		return teamstate.Clear(s.statePath)
	}
	return teamstate.Save(s.statePath, s.state)
}
