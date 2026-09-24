package teamsetup

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"aimem/internal/process"
	"aimem/internal/processctx"
	"aimem/internal/store"
	"aimem/internal/teamguide"
	"aimem/internal/teamstate"
)

var workerOpts = func() *Options {
	return &Options{Team: "Pilot", Role: "worker", Profile: store.TeamProfile{Label: "w", Platform: "codex", PlatformVersion: "1", Model: store.TeamModel{Provider: "unknown", ID: "unknown", Version: "unknown", Source: "unknown"}}, PlatformSet: true, ProfileSet: true}
}

// availabilityCaller is sessionCaller that also records the availability
// of every heartbeat it answers.
type availabilityCaller struct {
	sessionCaller
	sent []string
}

func (c *availabilityCaller) call(ctx context.Context, name string, raw json.RawMessage) (int, []byte, error) {
	if name == "team_heartbeat" {
		var a struct {
			Availability string `json:"availability"`
		}
		json.Unmarshal(raw, &a)
		c.sent = append(c.sent, a.Availability)
	}
	return c.sessionCaller.call(ctx, name, raw)
}

// A last-observed process is delivered, marked, and never makes the member
// ready: the hub has not confirmed that selection.
func TestLastObservedProcessIsDeliveredButNeverReady(t *testing.T) {
	ts := identityHub(t)
	repo, root := checkout(t, ts)
	caller := &availabilityCaller{}
	ref := process.Ref{Repo: "https://127.0.0.1:1/p.git", Commit: strings.Repeat("ab", 20), Manifest: "m.json"}
	last := &processctx.Result{State: processctx.LastObserved, Project: "alpha", Ref: &ref, Source: processctx.SourceCache,
		ObservedAt: "2026-09-24T00:00:00Z", FromLastObserved: true, Set: &process.Set{}, Unit: "# Handbook\n\ncached rules\n",
		Detail: "the hub is unreachable; exact cached commit"}
	env := Env{Dir: repo, Root: root, Version: "v0.7.3", TeamCall: caller.call, Process: func(string, string) *processctx.Result { return last }}
	rep := Run(env, workerOpts())
	rd := rep.Readiness
	if !rep.Joined() || rd.ReadyForWork || rd.ProjectProcess.State != "last_observed" || rd.RoleContext.State != "delivered" || rd.Membership.State != "active" {
		t.Fatalf("readiness: %+v", rd)
	}
	if len(caller.sent) != 1 || caller.sent[0] != "unavailable" {
		t.Fatalf("heartbeats: %v", caller.sent)
	}
	if len(rep.Delivered) != 2 || !strings.Contains(rep.Delivered[1].Text, "NOTE: the hub is unreachable") || !strings.Contains(rep.Delivered[1].Text, "cached rules") {
		t.Fatalf("deliveries: %+v", rep.Delivered)
	}
	if !strings.Contains(rep.Next[0], "NOT ready for work (project_process last_observed)") {
		t.Fatalf("next: %q", rep.Next)
	}
}

// A host that does not read the process cannot make a member ready.
func TestUncheckedProcessIsNotReady(t *testing.T) {
	ts := identityHub(t)
	repo, root := checkout(t, ts)
	caller := &availabilityCaller{}
	rep := Run(Env{Dir: repo, Root: root, Version: "v0.7.3", TeamCall: caller.call}, workerOpts())
	if rd := rep.Readiness; rd.ReadyForWork || rd.ProjectProcess.State != "not_checked" || rd.Execution.State != "not_verified" {
		t.Fatalf("readiness: %+v", rd)
	}
	if len(caller.sent) != 1 || caller.sent[0] != "unavailable" {
		t.Fatalf("heartbeats: %v", caller.sent)
	}
}

// A changed guidance digest since the last delivery is reported, and the
// new one is saved; an unchanged one says nothing.
func TestRoleGuidanceChangeIsReported(t *testing.T) {
	ts := identityHub(t)
	repo, root := checkout(t, ts)
	caller := &availabilityCaller{}
	env := Env{Dir: repo, Root: root, Version: "v0.7.3", TeamCall: caller.call}
	Run(env, workerOpts())
	canon, _ := filepath.EvalSymlinks(repo)
	path := teamstate.Path(root, canon)
	st, err := teamstate.Load(path)
	u, _ := teamguide.Embedded()
	if err != nil || st == nil || st.RoleDigest != u.Digest || st.RoleVersion != "v0.7.3" {
		t.Fatalf("saved digest: %+v %v", st, err)
	}
	changed := func(rep *Report) bool {
		for _, c := range rep.Checks {
			if c.Name == "role context" && c.Level == "warn" && strings.Contains(c.Detail, "changed since it was last delivered") {
				return true
			}
		}
		return false
	}
	if changed(Run(env, workerOpts())) {
		t.Fatal("an unchanged digest was reported as changed")
	}
	st.RoleDigest, st.RoleVersion = "sha256:0000", "v0.7.2"
	if err := teamstate.Save(path, st); err != nil {
		t.Fatal(err)
	}
	rep := Run(env, workerOpts())
	if !changed(rep) {
		t.Fatalf("the change was not reported: %+v", rep.Checks)
	}
	if st, _ := teamstate.Load(path); st.RoleDigest != u.Digest {
		t.Fatalf("new digest not saved: %s", st.RoleDigest)
	}
}
