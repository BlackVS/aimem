package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"aimem/internal/teamsession"
)

// sessionStartContext runs session-start in dir and returns the injected
// additionalContext.
func sessionStartContext(t *testing.T, dir string, env []string) string {
	t.Helper()
	out, errOut, err := runAimemEnv(t, dir, t.TempDir(), "", env, "session-start")
	if err != nil {
		t.Fatalf("session-start: %v %s", err, errOut)
	}
	var hook struct {
		Specific struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &hook); err != nil {
		t.Fatalf("output is not the hook's JSON: %v %q", err, out)
	}
	return hook.Specific.Context
}

// writeHandoff writes the handoff, and an archive that must never be
// injected, into a fresh checkout.
func writeHandoff(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "SESSION-STATE.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "SESSION-STATE-ARCHIVE.md"), []byte("ARCHIVE-MARKER\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A handoff over the limit is injected as its head, cut at a line break,
// and a note naming the file; never the whole document, never the archive.
// The same holds in a team conversation.
func TestSessionStartInjectsOnlyTheHeadOfAnOversizedHandoff(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 4*handoffInjectLimit; i++ {
		fmt.Fprintf(&b, "line %05d: a completed item that should have gone to the archive — ü\n", i)
	}
	b.WriteString("TAIL-MARKER\n")
	body := b.String()
	dir := writeHandoff(t, body)
	path := filepath.Join("docs", "SESSION-STATE.md")

	for _, c := range []struct {
		name string
		env  []string
	}{{"personal", nil}, {"team", []string{teamsession.EnvVar + "=s-1"}}} {
		ctx := sessionStartContext(t, dir, c.env)
		start := strings.Index(ctx, "line 00000:")
		end := strings.Index(ctx, "\n\n[aimem: "+path+" is ")
		if start < 0 || end < 0 {
			t.Fatalf("%s: no handoff head or no note: %.300q", c.name, ctx)
		}
		head := ctx[start:end]
		if len(head) > handoffInjectLimit || !strings.HasPrefix(body, head+"\n") {
			t.Fatalf("%s: the head is %d bytes, not a whole-line prefix of the handoff within %d", c.name, len(head), handoffInjectLimit)
		}
		if strings.Contains(ctx, "TAIL-MARKER") || strings.Contains(ctx, "ARCHIVE-MARKER") || !utf8.ValidString(ctx) {
			t.Fatalf("%s: the injection carries the tail or the archive, or is not UTF-8", c.name)
		}
		note := ctx[end+2:]
		if i := strings.Index(note, "\n"); i >= 0 {
			note = note[:i]
		}
		for _, want := range []string{fmt.Sprintf("is %d bytes", len(body)), fmt.Sprintf("first %d bytes", len(head)+1), "docs/SESSION-STATE-ARCHIVE.md"} {
			if !strings.Contains(note, want) {
				t.Errorf("%s: the note lacks %q: %q", c.name, want, note)
			}
		}
		// The whole injection stays near the limit: the head, the header,
		// the note and the fixed notices of a checkout with no hub.
		if bound := handoffInjectLimit + 1024 + len(writingNotice) + len(environmentNotice); len(ctx) > bound {
			t.Errorf("%s: the injection is %d bytes, over %d", c.name, len(ctx), bound)
		}
	}
}

// A handoff with no line break within the limit is cut on a rune boundary
// (the JSON encoder would turn a split rune into U+FFFD).
func TestSessionStartCutsALongLineOnARuneBoundary(t *testing.T) {
	body := strings.Repeat("ü", handoffInjectLimit) // two bytes each
	ctx := sessionStartContext(t, writeHandoff(t, "x"+body), nil)
	if strings.ContainsRune(ctx, utf8.RuneError) || strings.Count(ctx, "ü") >= handoffInjectLimit/2 || !strings.Contains(ctx, "session-start limit") {
		t.Fatalf("a long line is not cut on a rune boundary with a note: %d bytes", len(ctx))
	}
}

// A handoff within the limit is injected unchanged, without a note.
func TestSessionStartInjectsAHandoffWithinTheLimitWhole(t *testing.T) {
	body := strings.Repeat("x", handoffInjectLimit-len("END\n")) + "END\n"
	ctx := sessionStartContext(t, writeHandoff(t, body), nil)
	if !strings.Contains(ctx, ":\n\n"+body) || strings.Contains(ctx, "session-start limit") || strings.Contains(ctx, "ARCHIVE-MARKER") {
		t.Fatalf("a handoff of exactly the limit was not injected whole: %d bytes", len(ctx))
	}
}

// The installed guidance sends environment blockers to aimem's recall
// before they are reported, as the session-start line does.
func TestInstalledGuidanceSendsEnvironmentBlockersToRecall(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "templates", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(b)), " ")
	for _, want := range []string{"## Environment blockers", "credentials, SSH, hardware keys, network, missing tools",
		"recall aimem with the error's text", "recall_memory", "`aimem recall -q", "apply what it finds", "aimem failure-recall"} {
		if !strings.Contains(text, want) {
			t.Errorf("templates/AGENTS.md lacks %q", want)
		}
	}
}

// The guidance installed into a project states the limit the hook applies
// and names the archive.
func TestInstalledGuidanceStatesTheHandoffLimit(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "templates", "AGENTS.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range append([]string{fmt.Sprintf("first %d KiB", handoffInjectLimit>>10), "docs/SESSION-STATE-ARCHIVE.md", "never injected", "`aimem handoff compact`", handoffMarker, "instead of cutting the file by hand"}, handoffClosingNames...) {
		if !strings.Contains(strings.Join(strings.Fields(string(b)), " "), want) {
			t.Errorf("templates/AGENTS.md lacks %q", want)
		}
	}
}
