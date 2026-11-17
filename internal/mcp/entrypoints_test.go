package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"aimem/internal/teamsetup/teamsetuptest"
)

// The /join_team and /resume_team entry points (internal/wiring/assets,
// rendered into the committed client assets) instruct an agent in terms of
// this server's tools and report. These tests hold them to it.

var entryPointFiles = []string{
	filepath.Join("..", "wiring", "assets", "join_team.md"),
	filepath.Join("..", "wiring", "assets", "resume_team.md"),
	filepath.Join("..", "..", ".claude", "skills", "join_team", "SKILL.md"),
	filepath.Join("..", "..", ".claude", "skills", "resume_team", "SKILL.md"),
}

// Every tool an entry point names is one the checkout-bound local server
// lists in a checkout that never joined (the standalone set), so step 0's
// check passes there; or one of the teams set, named only after the step
// that calls team_setup or team_continue, whose answer turns the setting on
// (docs/DESIGN-tool-sets.md §5.3). A pre-approved tool is a standalone one.
func TestEntryPointsNameOnlyToolsTheLocalServerLists(t *testing.T) {
	var listed struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	s := stdioFor(t.TempDir(), t.TempDir())
	json.Unmarshal(s.handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)), &listed)
	standalone := map[string]bool{}
	for _, d := range listed.Result.Tools {
		standalone[d["name"].(string)] = true
	}
	teams := map[string]bool{}
	for _, d := range taskSetDefs(true, nil) {
		if n := d["name"].(string); isTeamWorkTool(n) {
			teams[n] = true
		}
	}
	if len(teams) != 26 || standalone["team_inbox"] || !standalone["team_setup"] || !standalone["team_continue"] {
		t.Fatalf("sets: %d teams tools; standalone %v", len(teams), standalone)
	}
	named := regexp.MustCompile("`(team_[a-z_]+|process_context)`")
	approved := regexp.MustCompile(`mcp__aimem__([a-z_]+)`)
	callStep := regexp.MustCompile("Call `team_(setup|continue)`")
	for _, f := range entryPointFiles {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		call := callStep.FindStringIndex(text)
		if call == nil {
			t.Fatalf("%s has no step that calls team_setup or team_continue", f)
		}
		seen := map[string]bool{}
		for _, m := range named.FindAllStringSubmatchIndex(text, -1) {
			name := text[m[2]:m[3]]
			seen[name] = true
			switch {
			case standalone[name]:
			case teams[name] && m[0] > call[0]:
			case teams[name]:
				t.Errorf("%s names %s before the step that joins: a checkout that never joined does not list it", f, name)
			default:
				t.Errorf("%s names %s, which the local server does not serve", f, name)
			}
		}
		for _, m := range approved.FindAllStringSubmatch(text, -1) {
			seen[m[1]] = true
			if !standalone[m[1]] {
				t.Errorf("%s pre-approves %s, which a checkout that never joined does not list", f, m[1])
			}
		}
		for _, want := range []string{"team_context", "process_context"} {
			if !seen[want] {
				t.Errorf("%s does not name %s", f, want)
			}
		}
	}
}

// The checks the entry points prescribe hold for what setup and continue
// deliver: each block's first lines quote its last line; the role
// guidance's terminator names role_context's version and digest; the
// process's names project_process's version as its commit and the digest
// after sha256; and the re-reads the entry points name (team_context role,
// process_context) return the same terminators, pinned included. The next
// steps name the delivered guidance, never a repository path.
func TestEntryPointTerminatorChecksHoldForTheDeliveredContext(t *testing.T) {
	check := func(t *testing.T, s *srv, rep readinessReport, blocks []string, role string, waiting bool) {
		t.Helper()
		rd := rep.Readiness
		if len(blocks) != 2 {
			t.Fatalf("blocks: %d", len(blocks))
		}
		for _, b := range blocks {
			if !strings.Contains(b, strconv.Quote(lastLineOf(b))) {
				t.Fatalf("the first lines do not quote the terminator %q", lastLineOf(b))
			}
		}
		roleEnd, procEnd := lastLineOf(blocks[0]), lastLineOf(blocks[1])
		if !strings.Contains(roleEnd, "role "+role+" version "+rd.RoleContext.Version+" digest "+rd.RoleContext.Digest+" ===") {
			t.Fatalf("role terminator %q, readiness %+v", roleEnd, rd.RoleContext)
		}
		hexDigest, ok := strings.CutPrefix(rd.ProjectProcess.Digest, "sha256:")
		if !ok || !strings.Contains(procEnd, " commit "+rd.ProjectProcess.Version+" ") || !strings.HasSuffix(procEnd, " sha256 "+hexDigest+" ===") {
			t.Fatalf("process terminator %q, readiness %+v", procEnd, rd.ProjectProcess)
		}
		text, isErr := callTool(t, s, "team_context", map[string]any{"role": role})
		if isErr || lastLineOf(text) != roleEnd {
			t.Fatalf("team_context role=%s: %v %q", role, isErr, lastLineOf(text))
		}
		text, isErr = callTool(t, s, "process_context", map[string]any{})
		if isErr || lastLineOf(text) != procEnd {
			t.Fatalf("process_context: %v %q, want %q", isErr, lastLineOf(text), procEnd)
		}
		next := strings.Join(rep.Next, "\n")
		if strings.Contains(next, "docs/") || waiting && !strings.Contains(next, "team_context section="+role) {
			t.Fatalf("next: %s", next)
		}
	}

	t.Run("coordinator", func(t *testing.T) {
		h, ts := teamsetuptest.New(t)
		repo, root := teamsetuptest.Checkout(t, h, ts)
		s := stdioFor(repo, root)
		rep, blocks := onboardBlocks(t, s, "team_setup", map[string]any{"team": "Pilot", "role": "coordinator", "profile": map[string]any{"label": "c", "platform": "codex", "platform_version": "1"}})
		check(t, s, rep, blocks, "coordinator", true)
	})
	t.Run("worker", func(t *testing.T) {
		h, ts := teamsetuptest.New(t)
		repo, root := teamsetuptest.Checkout(t, h, ts)
		s := stdioFor(repo, root)
		rep, blocks := onboardBlocks(t, s, "team_setup", map[string]any{"team": "Pilot", "role": "worker", "profile": workerProfile})
		check(t, s, rep, blocks, "worker", true)
	})
	// Accepted under A, B selected, a restart: the delivered block and the
	// process_context re-read agree on A, the version the readiness names.
	t.Run("pinned", func(t *testing.T) {
		h, s, repo, root := acceptedFixture(t)
		if text, isErr := accept(t, s, "k1"); isErr {
			t.Fatal(text)
		}
		sel := selB
		h.Selection = &sel
		s = stdioFor(repo, root)
		rep, blocks := onboardBlocks(t, s, "team_continue", map[string]any{})
		if rep.Readiness.ProjectProcess.Version != teamsetuptest.Selection.Commit || !strings.Contains(blocks[1], "PINNED") {
			t.Fatalf("pinned: %+v", rep.Readiness.ProjectProcess)
		}
		check(t, s, rep, blocks, "worker", false)
	})
}

// A blocked report can still carry context: a coordinator whose inbox read
// fails after its context was evaluated. The entry points stop on blocked
// whatever is attached, and say so.
func TestEntryPointsStopOnABlockedReportWithAttachedContext(t *testing.T) {
	h, ts := teamsetuptest.New(t)
	h.InboxCode = 500
	repo, root := teamsetuptest.Checkout(t, h, ts)
	rep, blocks := onboardBlocks(t, stdioFor(repo, root), "team_setup", map[string]any{"team": "Pilot", "role": "coordinator", "profile": map[string]any{"label": "c", "platform": "codex", "platform_version": "1"}})
	if rep.Status != "blocked" || len(blocks) == 0 {
		t.Fatalf("status %s with %d blocks: the case this rule covers did not occur", rep.Status, len(blocks))
	}
	for _, f := range entryPointFiles {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(strings.Fields(string(raw)), " "), "If the report is blocked, stop even when context blocks are attached. Attached context does not authorize proceeding.") {
			t.Errorf("%s does not stop on a blocked report with attached context", f)
		}
	}
}
