package mcp

// Team onboarding over the checkout-bound local MCP: team_setup and
// team_continue run the same code as `aimem teams setup` and `aimem teams
// continue` (internal/teamsetup), from this process, which the client
// started in the checkout as the credential's owner. A shell the client
// sandboxes under another account cannot read or decrypt that credential;
// this process can, and it is bound to one checkout and one state root by
// construction. The tools accept the team, the role, a declared profile
// and explicit recovery choices: no token, no checkout, no command, no
// executable. The hub facade has no checkout and does not list them.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"aimem/internal/processctx"
	"aimem/internal/server"
	"aimem/internal/store"
	"aimem/internal/teamsetup"
	"aimem/internal/wiring"
)

// localCheckout is what binds the stdio facade: the working directory the
// client started it in and this process's state root. nil on the hub.
type localCheckout struct {
	dir, root string
}

// env is the onboarding host environment of this process. The session
// caller is the one every team_* tool uses; the process context is the
// session-start bootstrap for the same state root; the git probe reads
// HEAD only, because `git status` consults configuration the checkout's
// editor controls and this process runs as someone else.
func (l *localCheckout) env() teamsetup.Env {
	v := server.Version
	if v == "" {
		v = "dev"
	}
	return teamsetup.Env{
		Dir:     l.dir,
		Root:    l.root,
		Version: v,
		TeamCall: func(ctx context.Context, name string, raw json.RawMessage) (int, []byte, error) {
			return TeamRequestIn(ctx, l.dir, l.root, name, raw)
		},
		Process: func(dir, project string) *processctx.Result {
			return processctx.Load(dir, l.root, project)
		},
		Git: teamsetup.GitHeadOnly,
	}
}

const onboardCommon = " Runs in this MCP process, as the account that installed this checkout's credential, bound to this checkout and its state root: the same code and the same saved session state as the aimem CLI, so a shell that cannot read the credential is not needed. Accepts no token, checkout, command or executable. The result is the report (the first text block): status joined or blocked, every check with its fix, the session handle for the team_* tools, a readiness object and next steps. For a verified session, the role's team guidance and the project's selected process follow as their own text blocks, each ending with a terminator line (a block without it was cut). joined is not permission to work: only readiness.ready_for_work is, and execution (your shell, build tools, runner) is never verified here. A worker that is not ready is announced unavailable and must accept nothing; a coordinator that is not ready issues no offers. On blocked, show the failing checks and the operator handoff to the user and stop (never work around a refusal, change credentials or join through raw tools). Nothing here wakes an idle agent: poll team_inbox afterwards."

var onboardToolDefs = []map[string]any{
	{
		"name":        "team_setup",
		"description": "Verified onboarding of this checkout into a team as worker or coordinator (/join_team): checks the project binding, the client wiring (report only unless repair_integration), the credential's identity, the hub, the enrollment, the selected process and the base commit, then joins or recognizes the session this checkout already holds (never a second membership; an unconfirmed join or resume replays with its saved key; a different team or role than the saved one is refused, not switched) and enters the role: the roster for a coordinator, availability plus the reserved attempt and one bounded inbox read for a worker." + onboardCommon,
		"inputSchema": objSchema(map[string]any{
			"team":                     prop("string", "team readable name (exact) or ID"),
			"role":                     propEnum("requested role; coordinator needs enrollment eligibility", "worker", "coordinator"),
			"profile":                  teamProfileSchema(),
			"resume":                   prop("boolean", "after a restart: resume the saved session (new generation; the old handle is fenced) instead of only verifying a live one; a suspect session is resumed regardless"),
			"new_session":              prop("boolean", "discard the saved handle and join afresh; only when the old session is known to be gone or left"),
			"repair_integration":       prop("boolean", "add the missing aimem-owned client wiring (SessionStart hook, MCP registration, entry points) instead of only reporting it; default false"),
			"allow_project_stop_hooks": prop("boolean", "do not block on project-level Stop/StopFailure/PreCompact hooks"),
		}, "team", "role"),
	},
	{
		"name":        "team_continue",
		"description": "Continue the team membership this checkout already holds after a client restart or a compaction (/resume_team): verifies the binding, the credential and the hub, verifies or resumes the saved session (live: verified as is; suspect: resumed, fencing the old handle; refused: reported), then reports the duties the hub holds for it: the reserved attempt and its state, the unacknowledged inbox, the roster. Never joins: an ended membership is reported and team_setup is the only way back in." + onboardCommon,
		"inputSchema": objSchema(map[string]any{
			"team":  prop("string", "optional; when given it must be the saved team"),
			"fence": prop("boolean", "resume the saved session even when it is still heartbeating (fences a handle another process of yours may hold); only when that process is known to be gone"),
		}),
	},
}

var onboardToolNames = func() map[string]bool {
	m := map[string]bool{}
	for _, d := range onboardToolDefs {
		m[d["name"].(string)] = true
	}
	return m
}()

func isOnboardTool(name string) bool { return onboardToolNames[name] }

// onboardTool runs one onboarding tool for the bound checkout and returns
// the report as indented JSON, then each delivered text as its own block. A
// blocked report is a result, not a tool error: the checks say why and what
// to do.
func (s *srv) onboardTool(name string, raw json.RawMessage) (string, []string, error) {
	rep, err := s.onboardReport(name, raw)
	if err != nil {
		return "", nil, err
	}
	var blocks []string
	for _, d := range rep.Delivered {
		blocks = append(blocks, d.Text)
	}
	out := *rep
	out.Delivered = nil // carried as blocks, not repeated inside the JSON
	text, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return "", nil, err
	}
	return string(text), blocks, nil
}

func (s *srv) onboardReport(name string, raw json.RawMessage) (*teamsetup.Report, error) {
	if s.local == nil {
		return nil, errors.New("team onboarding tools exist only on the checkout-bound local MCP server (aimem mcp, started by the client in the checkout), never on the hub")
	}
	var a struct {
		Team                  string             `json:"team"`
		Role                  string             `json:"role"`
		Profile               *store.TeamProfile `json:"profile"`
		Resume                bool               `json:"resume"`
		NewSession            bool               `json:"new_session"`
		RepairIntegration     bool               `json:"repair_integration"`
		AllowProjectStopHooks bool               `json:"allow_project_stop_hooks"`
		Fence                 bool               `json:"fence"`
	}
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if len(raw) > 64<<10 {
		return nil, errors.New("arguments must be a JSON object at most 64 KiB")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("expected one JSON object")
	}
	var rep *teamsetup.Report
	switch name {
	case "team_setup":
		if strings.TrimSpace(a.Team) == "" {
			return nil, errors.New("team is required")
		}
		if a.Role != "worker" && a.Role != "coordinator" {
			return nil, fmt.Errorf("role must be worker or coordinator, got %q", a.Role)
		}
		if a.Fence {
			return nil, errors.New("fence belongs to team_continue; team_setup takes resume")
		}
		if a.Resume && a.NewSession {
			return nil, errors.New("resume and new_session exclude each other")
		}
		opts := &teamsetup.Options{Team: a.Team, Role: a.Role, Resume: a.Resume, NewSession: a.NewSession, Wiring: wiring.Options{Repair: a.RepairIntegration, AllowProjectStopHooks: a.AllowProjectStopHooks}}
		if a.Profile != nil {
			// The schema requires label, platform and platform version once a
			// profile is given; the core reuses a saved declaration when none is.
			if a.Profile.Label == "" || a.Profile.Platform == "" || a.Profile.PlatformVersion == "" {
				return nil, errors.New("profile needs label, platform and platform_version (use unknown, never a guess)")
			}
			opts.Profile, opts.ProfileSet, opts.PlatformSet = *a.Profile, true, a.Profile.Platform != "unknown"
		} else {
			host, _ := os.Hostname()
			if host == "" {
				host = "host"
			}
			opts.Profile.Label = a.Role + "-" + host
		}
		if err := teamsetup.CheckProfile(&opts.Profile); err != nil {
			return nil, err
		}
		rep = teamsetup.Run(s.local.env(), opts)
	case "team_continue":
		if a.Role != "" || a.Profile != nil || a.Resume || a.NewSession || a.RepairIntegration || a.AllowProjectStopHooks {
			return nil, errors.New("team_continue takes only team and fence: the role and profile come from the saved membership, and a restart never joins")
		}
		rep = teamsetup.Continue(s.local.env(), a.Team, a.Fence)
	default:
		return nil, errors.New("unknown onboarding tool")
	}
	return rep, nil
}
