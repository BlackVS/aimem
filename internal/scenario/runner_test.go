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

// TestScenarioCardDrift runs a copy of a card with one expected line
// changed and requires the run to fail, naming the copy, the step and the
// line: a card whose expected output no longer matches the program fails
// the suite. The smoke card drifts in a command step, card 01 in an http
// step.
func TestScenarioCardDrift(t *testing.T) {
	for _, d := range []struct{ card, want, drifted, step string }{
		{"00-fixture.md", "not ready for work: tasks on, process not selected", "not ready for work: tasks off",
			"step 3 (Tasks are switched on for the project)"},
		{"01-new-project.md", `"title":"First task from the workstation"`, `"title":"Another task"`,
			"step 25 (The project's tasks list the new task)"},
	} {
		t.Run(strings.TrimSuffix(d.card, ".md"), func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(cardDir, d.card))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(b), "\n")
			at := -1
			for i, l := range lines {
				if strings.TrimRight(l, "\r") == d.want {
					at = i // the last one
				}
			}
			if at < 0 {
				t.Fatalf("%s no longer holds %q; pick another line for the drift test", d.card, d.want)
			}
			lines[at] = d.drifted
			copied := filepath.Join(t.TempDir(), d.card)
			if err := os.WriteFile(copied, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
				t.Fatal(err)
			}
			w := newWorld(t)
			err = w.run(copied)
			prefix := fmt.Sprintf("%s:%d: %s: expected line %q not in output:", copied, at+1, d.step, d.drifted)
			if err == nil || !strings.HasPrefix(err.Error(), prefix) {
				t.Fatalf("the drifted card: got %v\nwant an error starting %q", err, prefix)
			}
		})
	}
}

// TestParseCardRefuses covers what a card may not hold: each case is a
// step a person could run in a shell but the runner could not, or a card
// whose shape the convention does not allow.
func TestParseCardRefuses(t *testing.T) {
	step := func(heading, cmd, text string) string {
		return "# Card\n\n" + heading + "\n\nWhy.\n\n```sh\n" + cmd + "\n```\n\n```text\n" + text + "\n```\n"
	}
	httpStep := func(block, text string) string {
		return "# Card\n\n### 1. A (workstation)\n\nWhy.\n\n```http\n" + block + "\n```\n\n```text\n" + text + "\n```\n"
	}
	vars := map[string]string{"project": "p"}
	for _, c := range []struct{ name, card, want string }{
		{"sh and http", strings.Replace(step("### 1. A (hub)", "aimem health", "ok"), "```text", "```http\nGET /v1/health\n```\n\n```text", 1), "card.md:11: step 1: the reason line, then one sh or http block"},
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
		{"no expected line", step("### 1. A (hub)", "aimem health", ""), "needs a reason line, an sh or http block and a text block"},
		{"numbering", step("### 2. A (hub)", "aimem health", "ok"), "step 2 follows step 0"},
		// sh would read demo\project as demoproject; the error is matched
		// without the backslash, which ToSlash would turn into a slash.
		{"backslash", step("### 1. A (hub)", `aimem tasks on -p demo\project`, "ok"), "card.md:8: step 1: "},
		{"unclosed fence", strings.TrimSuffix(step("### 1. A (hub)", "aimem health", "ok"), "```\n"), "card.md:11: the fenced block opened here is not closed"},
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

	// An http step: the request line, the Token-File header naming a
	// placeholder, other headers and a JSON body, all substituted.
	httpVars := map[string]string{"project": "p", "task-token-file": `C:\fx\task.token`}
	os.WriteFile(p, []byte(httpStep("POST /v1/projects/<project>/tasks\nToken-File: <task-token-file>\nIdempotency-Key: k-<project>\n\n{\"title\": \"T <project>\"}", "201 Created")), 0o600)
	c, err = ParseCard(p, httpVars)
	if err != nil {
		t.Fatal(err)
	}
	r := c.Steps[0].HTTP
	if c.Steps[0].Args != nil || r == nil || r.Method != "POST" || r.Path != "/v1/projects/p/tasks" || r.TokenFile != httpVars["task-token-file"] ||
		len(r.Header) != 1 || r.Header[0] != [2]string{"Idempotency-Key", "k-p"} || r.Body != `{"title": "T p"}` || c.Steps[0].Line != 8 {
		t.Fatalf("parsed http step: %+v %+v", c.Steps[0], r)
	}
	for _, h := range []struct{ name, block, want string }{
		{"no token file", "GET /v1/projects/p/tasks", "needs a Token-File header"},
		{"literal token file", "GET /v1/projects/p/tasks\nToken-File: task.token", "Token-File names one placeholder"},
		{"authorization", "GET /v1/projects/p/tasks\nToken-File: <task-token-file>\nAuthorization: Bearer x", "not an Authorization line"},
		{"not v1", "GET /tasks\nToken-File: <task-token-file>", "starts with `METHOD /v1/...`"},
		{"method", "FETCH /v1/projects/p/tasks\nToken-File: <task-token-file>", "starts with `METHOD /v1/...`"},
		{"body not json", "POST /v1/projects/p/tasks\nToken-File: <task-token-file>\n\n{title: T}", "not JSON"},
		{"quote in body", "POST /v1/projects/p/tasks\nToken-File: <task-token-file>\n\n{\"title\": \"it's\"}", `"'" is shell syntax`},
		{"variable in header", "GET /v1/projects/p/tasks\nToken-File: <task-token-file>\nX-A: $HOME", `"$" is shell syntax`},
		{"unknown placeholder", "GET /v1/projects/<projekt>/tasks\nToken-File: <task-token-file>", "unknown placeholder <projekt>"},
		{"bad header", "GET /v1/projects/p/tasks\nToken-File: <task-token-file>\nnot a header", "is not a header line"},
	} {
		os.WriteFile(p, []byte(httpStep(h.block, "ok")), 0o600)
		if _, err := ParseCard(p, httpVars); err == nil || !strings.Contains(err.Error(), "card.md:8: step 1: ") || !strings.Contains(err.Error(), h.want) {
			t.Errorf("%s: got %v, want %q", h.name, err, h.want)
		}
	}

	// The backslash refusal reads the card's own words: a placeholder's
	// value, such as a Windows path, may hold backslashes.
	win := `C:\Users\runner\AppData\Local\Temp\aimem-sc1\fx\ca.pem`
	os.WriteFile(p, []byte(step("### 1. A (workstation)", "aimem hub add team https://h --ca-file <ca-file>", "ok")), 0o600)
	c, err = ParseCard(p, map[string]string{"ca-file": win})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Steps[0].Args; got[len(got)-1] != win {
		t.Fatalf("substituted path: %q", got)
	}
}
