//go:build scenario

package scenario

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const cardDir = "../../docs/scenarios"

// cardName is a card's file name; README.md and other files are not cards.
var cardName = regexp.MustCompile(`^\d{2}-[a-z0-9-]+\.md$`)

// TestScenarioCards runs every card in docs/scenarios on its own hub and
// workstation.
func TestScenarioCards(t *testing.T) {
	entries, err := os.ReadDir(cardDir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !cardName.MatchString(e.Name()) {
			continue
		}
		n++
		t.Run(strings.TrimSuffix(e.Name(), ".md"), func(t *testing.T) {
			w := newWorld(t)
			if err := w.run(filepath.Join(cardDir, e.Name())); err != nil {
				t.Fatal(err)
			}
		})
	}
	if n == 0 {
		t.Fatalf("no cards in %s", cardDir)
	}
}

// TestScenarioCardDrift runs a copy of the smoke card with one expected
// line changed and requires the run to fail, naming the copy, the step and
// the line: a card whose expected output no longer matches the program
// fails the suite.
func TestScenarioCardDrift(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(cardDir, "00-fixture.md"))
	if err != nil {
		t.Fatal(err)
	}
	const want = "not ready for work: tasks on, process not selected"
	lines := strings.Split(string(b), "\n")
	at := -1
	for i, l := range lines {
		if strings.TrimRight(l, "\r") == want {
			at = i
		}
	}
	if at < 0 {
		t.Fatalf("the smoke card no longer holds %q; pick another line for the drift test", want)
	}
	lines[at] = "not ready for work: tasks off"
	copied := filepath.Join(t.TempDir(), "00-fixture.md")
	if err := os.WriteFile(copied, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
	w := newWorld(t)
	err = w.run(copied)
	prefix := fmt.Sprintf("%s:%d: step 3 (Tasks are switched on for the project): expected line %q not in output:", copied, at+1, "not ready for work: tasks off")
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatalf("the drifted card: got %v\nwant an error starting %q", err, prefix)
	}
}

// TestParseCardRefuses covers what a card may not hold: each case is a
// step a person could run in a shell but the runner could not, or a card
// whose shape the convention does not allow.
func TestParseCardRefuses(t *testing.T) {
	step := func(heading, cmd, text string) string {
		return "# Card\n\n" + heading + "\n\nWhy.\n\n```sh\n" + cmd + "\n```\n\n```text\n" + text + "\n```\n"
	}
	vars := map[string]string{"project": "p"}
	for _, c := range []struct{ name, card, want string }{
		{"pipe", step("### 1. A (hub)", "aimem health | jq .status", "ok"), "card.md:8: step 1: \"|\": \"|\" is shell syntax"},
		{"redirection", step("### 1. A (hub)", "aimem health >out.json", "ok"), "is shell syntax"},
		{"variable", step("### 1. A (hub)", "aimem tasks on -p $PROJECT", "ok"), "is shell syntax"},
		{"quoting", step("### 1. A (hub)", `aimem tasks on -p "p"`, "ok"), "is shell syntax"},
		{"tilde", step("### 1. A (hub)", "aimem hub add team https://h --token-file ~/t", "ok"), `"~/t": "~" is shell syntax`},
		{"comment", step("### 1. A (hub)", "aimem health # the hub", "ok"), `"#": "#" is shell syntax`},
		{"not aimem", step("### 1. A (hub)", "curl -k https://x", "ok"), "must be an aimem command"},
		{"two commands", step("### 1. A (hub)", "aimem health\naimem projects", "ok"), "more than one command"},
		{"unknown placeholder", step("### 1. A (hub)", "aimem tasks on -p <projekt>", "ok"), "unknown placeholder <projekt>"},
		{"unknown placeholder in text", step("### 1. A (hub)", "aimem health", "<projekt>"), "card.md:12: unknown placeholder <projekt>"},
		{"no side", step("### 1. A", "aimem health", "ok"), "a step heading is"},
		{"no expected line", step("### 1. A (hub)", "aimem health", ""), "needs a reason line, an sh block and a text block"},
		{"numbering", step("### 2. A (hub)", "aimem health", "ok"), "step 2 follows step 0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "card.md")
			if err := os.WriteFile(p, []byte(c.card), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := ParseCard(p, vars)
			if err == nil || !strings.Contains(filepath.ToSlash(err.Error()), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}

	// A continued command is one command, and placeholders are substituted
	// in it and in the expected lines.
	p := filepath.Join(t.TempDir(), "card.md")
	os.WriteFile(p, []byte(step("### 1. A (workstation)", "aimem tasks on \\\n  -p <project>", "project \"<project>\"")), 0o600)
	c, err := ParseCard(p, vars)
	if err != nil {
		t.Fatal(err)
	}
	st := c.Steps[0]
	if strings.Join(st.Args, " ") != "aimem tasks on -p p" || st.Side != "workstation" || st.Expect[0].Text != `project "p"` || st.Line != 8 {
		t.Fatalf("parsed: %+v", st)
	}
}
