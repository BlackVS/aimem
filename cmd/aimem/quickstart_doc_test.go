package main

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// commandUsages are the commands with a usage text of their own; every
// other command is described in mainUsage.
var commandUsages = map[string]string{
	"access":  accessUsage,
	"docs":    docsUsage,
	"hub":     hubUsage,
	"process": processUsage,
	"project": projectUsage,
	"skills":  skillsUsage,
	"token":   tokenUsage,
}

// docAimemCommands returns every aimem command a guide shows: each line of
// a fenced code block that starts with "aimem ", without its trailing
// comment, and each inline code span that does.
func docAimemCommands(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	inBlock := false
	span := regexp.MustCompile("`(aimem [^`]+)`")
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inBlock = !inBlock
			continue
		}
		if inBlock {
			if strings.HasPrefix(trimmed, "aimem ") {
				if i := strings.Index(trimmed, " #"); i >= 0 {
					trimmed = strings.TrimSpace(trimmed[:i])
				}
				out = append(out, trimmed)
			}
			continue
		}
		for _, m := range span.FindAllStringSubmatch(line, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// usageForm is one form of a command in a usage text. head is its first
// line without a description that starts right after the command word;
// full adds the indented continuation lines.
type usageForm struct{ head, full string }

var proseWord = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// synopsisHead drops the description from a usage line whose command takes
// no arguments, such as "health   service health": a plain word after a
// gap of two or more spaces, followed by anything but an argument
// placeholder, starts prose, not a subcommand ("tasks   on|off [-p]",
// "teams   setup TEAM" keep theirs).
func synopsisHead(s string) string {
	cmd, rest, _ := strings.Cut(s, " ")
	if !strings.HasPrefix(rest, " ") {
		return s
	}
	words := strings.Fields(rest)
	if len(words) >= 2 && proseWord.MatchString(words[0]) {
		next := words[1]
		if !strings.ContainsAny(next[:1], "<[(-") && strings.ToUpper(next[:1]) != next[:1] {
			return cmd
		}
	}
	return s
}

// usageForms returns the forms of a usage text that start with cmd, with
// the leading "usage:" and "aimem " removed.
func usageForms(text, cmd string) []usageForm {
	var forms []usageForm
	cur, curIndent := -1, 0
	for _, line := range strings.Split(text, "\n") {
		indent := len(line) - len(strings.TrimLeft(line, " "))
		s := strings.TrimSpace(line)
		s = strings.TrimSpace(strings.TrimPrefix(s, "usage:"))
		s = strings.TrimPrefix(s, "aimem ")
		first, _, _ := strings.Cut(s, " ")
		if s != "" && hasAlternative(first, cmd) {
			forms = append(forms, usageForm{head: synopsisHead(s), full: s})
			cur, curIndent = len(forms)-1, indent
			continue
		}
		// A continuation is indented deeper than its form's row; a row at
		// the same depth is the next command, whose flags are not this
		// form's.
		if cur >= 0 && s != "" && indent > curIndent {
			forms[cur].full += " " + s
			continue
		}
		cur = -1
	}
	return forms
}

var literalWord = regexp.MustCompile(`^[a-z][a-z0-9-]*(\|[a-z][a-z0-9-]*)*$`)

// literal reports whether a usage word names fixed subcommands ("add",
// "on|off", "<list|sync>"), not a placeholder ("<name>", "NAME", "[dir]").
func literal(word string) (string, bool) {
	if strings.HasPrefix(word, "<") && strings.HasSuffix(word, ">") {
		inner := word[1 : len(word)-1]
		return inner, strings.Contains(inner, "|") && literalWord.MatchString(inner)
	}
	return word, literalWord.MatchString(word)
}

func hasAlternative(alts, word string) bool {
	return slices.Contains(strings.Split(alts, "|"), word)
}

// checkAgainstUsage checks one documented command against the CLI's usage
// text: a form for the command exists, every fixed subcommand word of that
// form is the one the command uses, and every flag the command passes is
// in that form.
func checkAgainstUsage(command string) error {
	words := strings.Fields(command)
	if len(words) < 2 || words[0] != "aimem" {
		return fmt.Errorf("not an aimem command")
	}
	text, ok := commandUsages[words[1]]
	if !ok {
		text = mainUsage
	}
	forms := usageForms(text, words[1])
	if len(forms) == 0 {
		return fmt.Errorf("no usage form for %q", words[1])
	}
	var matched []string
	for _, form := range forms {
		fw := strings.Fields(form.head)
		fits := true
		for i := 1; i < len(fw) && fits; i++ {
			alts, lit := literal(fw[i])
			if !lit {
				break
			}
			fits = i+1 < len(words) && hasAlternative(alts, words[i+1])
		}
		if fits {
			matched = append(matched, form.full)
		}
	}
	if len(matched) == 0 {
		return fmt.Errorf("no usage form of %q takes %q", words[1], strings.Join(words[2:], " "))
	}
	for _, w := range words[2:] {
		if !strings.HasPrefix(w, "-") || w == "-" {
			continue
		}
		flagName, _, _ := strings.Cut(w, "=")
		re := regexp.MustCompile(`(^|[\s\[(|])` + regexp.QuoteMeta(flagName) + `($|[\s\]|)=])`)
		found := false
		for _, form := range matched {
			found = found || re.MatchString(form)
		}
		if !found {
			return fmt.Errorf("flag %s is not in the usage of %q", flagName, words[1])
		}
	}
	return nil
}

// Every aimem command the new-project quickstart shows is one the CLI
// documents: the command, its subcommands and every flag it passes are in
// the usage text, so a renamed flag or subcommand fails here instead of
// on the operator's machine.
func TestQuickstartNewProjectCommandsMatchUsage(t *testing.T) {
	cmds := docAimemCommands(t, "../../docs/QUICKSTART-NEW-PROJECT.md")
	if len(cmds) < 20 {
		t.Fatalf("found %d aimem commands in the guide; the extraction is broken", len(cmds))
	}
	for _, c := range cmds {
		if err := checkAgainstUsage(c); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
	// The steps the guide exists to give, each present at least once.
	for _, want := range []string{
		"aimem token add ", "aimem project onboard ", "aimem access user-add ",
		"aimem access token-issue-user ", "aimem hub add team ", "aimem hub task-token team ",
		"aimem health", "aimem session-start", "aimem docs list",
		"aimem process default set ", "aimem process show", "aimem process show --template task",
		"aimem skills import --from-archive ", "aimem project repo set ", "aimem skills install",
	} {
		found := false
		for _, c := range cmds {
			found = found || strings.HasPrefix(c, want)
		}
		if !found {
			t.Errorf("the guide shows no %q", want)
		}
	}
}

// Step 2 imports the skills before onboard selects the process, binds the
// repository, and quotes the warning the CLI prints when the hub lacks a
// required skill.
func TestQuickstartNewProjectImportsSkillsBeforeTheProcess(t *testing.T) {
	raw, err := os.ReadFile("../../docs/QUICKSTART-NEW-PROJECT.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	step2, _, _ := strings.Cut(text[strings.Index(text, "## 2. "):], "## 3. ")
	imp := strings.Index(step2, "aimem skills import --from-archive aiskills-")
	sel := strings.Index(step2, "aimem project onboard ")
	if imp < 0 || sel < 0 || imp > sel {
		t.Errorf("step 2 does not import the skills release before onboard selects the process (import at %d, onboard at %d)", imp, sel)
	}
	if !strings.Contains(step2, "aimem project repo set --project my-project ") {
		t.Error("step 2 does not bind the project's repository")
	}
	if strings.Count(text, "`"+hubLacksSkills+"`") < 2 {
		t.Errorf("the guide does not quote the warning %q in step 2 and step 3.2", hubLacksSkills)
	}
}

// The checker itself refuses what it exists to catch.
func TestCheckAgainstUsageRefusesDrift(t *testing.T) {
	for _, c := range []string{
		"aimem access grant add --project p --user-name u",
		"aimem hub add team https://h --token-file f",
		"aimem tasks on -p p",
		"aimem docs list",
		"aimem project-id .",
		"aimem hub credential team",
		"aimem health",
		"aimem session-start",
		"aimem process select r 0123 m.json --ref main -p p",
		"aimem remember -p p --kind decision text",
		// --expires and --output are on the form's continuation row.
		"aimem access token-issue --user-name u --label l --project p --expires x --output f",
	} {
		if err := checkAgainstUsage(c); err != nil {
			t.Errorf("%s: %v", c, err)
		}
	}
	for _, c := range []string{
		"aimem access grant add --project p --user u", // a flag the form lacks
		"aimem access grant-add --project p",          // a subcommand that does not exist
		"aimem access grant put --project p",          // a wrong fixed word
		"aimem hub add team https://h --tokenfile f",  // a misspelt flag
		"aimem docs lst",                               // a misspelt subcommand
		"aimem token add n --label x",                  // a flag of another command
		"aimem tasks enable -p p",                      // tasks takes on|off
		"aimem frobnicate",                             // no such command
		"aimem health --json",                          // health takes no flag
		"aimem health --project p",                     // a flag of the next row (project repo)
		"aimem tasks on --sim 0.9",                     // a flag of a later row (dedup)
		"aimem process show --ref main",                // a flag of the select form
		"aimem hub task-token team --token-file f --x", // an unknown flag at the end
	} {
		if err := checkAgainstUsage(c); err == nil {
			t.Errorf("%s: accepted", c)
		}
	}
}
