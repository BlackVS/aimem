package process

import (
	"fmt"
	"strings"
)

// NotSelected is the not-ready reason of a project whose tasks are on with
// no process selected: no member is ready for work, workers are announced
// unavailable and the coordinator issues no offers.
const NotSelected = "tasks on, process not selected"

// InUse is a process selection this hub already holds, with the projects
// that follow it.
type InUse struct {
	Repo     string   `json:"repo"`
	Ref      string   `json:"ref,omitempty"`
	Commit   string   `json:"commit"`
	Manifest string   `json:"manifest"`
	Projects []string `json:"projects"`
}

// inUseShown bounds how many selections NextStep offers.
const inUseShown = 5

// NextStep is what an admin is told about a project with tasks on and no
// process selected: the selection command for it, complete with the
// values of each process the hub already uses (most used first), or, when
// it uses none, with the three values explained. It names commands only;
// the admin chooses and runs one, which records the pin like any other
// selection.
func NextStep(project string, inUse []InUse) string {
	var b strings.Builder
	if len(inUse) == 0 {
		b.WriteString("next step: select the project's process on this hub host:\n")
		fmt.Fprintf(&b, "  aimem process select <repo> <commit> <manifest> [--ref <branch>] -p %s\n", project)
		b.WriteString("    <repo>      the process repository's git URL, e.g. https://github.com/example/process.git\n")
		b.WriteString("    <commit>    the full 40-hex commit to follow, e.g. 0123456789abcdef0123456789abcdef01234567\n")
		b.WriteString("    <manifest>  the manifest's path in that repository, e.g. process/manifest.json\n")
		b.WriteString("  (docs/QUICKSTART-NEW-PROJECT.md, step 2)\n")
		return b.String()
	}
	b.WriteString("next step: select the project's process on this hub host; processes this hub already uses:\n")
	for i, u := range inUse {
		if i == inUseShown {
			fmt.Fprintf(&b, "  (%d more not shown)\n", len(inUse)-i)
			break
		}
		fmt.Fprintf(&b, "  as %s:\n", strings.Join(u.Projects, ", "))
		fmt.Fprintf(&b, "    aimem process select %s %s %s", shellArg(u.Repo), shellArg(u.Commit), shellArg(u.Manifest))
		if u.Ref != "" {
			fmt.Fprintf(&b, " --ref %s", shellArg(u.Ref))
		}
		fmt.Fprintf(&b, " -p %s\n", shellArg(project))
	}
	fmt.Fprintf(&b, "  or another process: aimem process select <repo> <commit> <manifest> [--ref <branch>] -p %s\n", shellArg(project))
	return b.String()
}

// shellArg renders one argument of an offered command so a POSIX shell
// reads it back unchanged. A selection may hold characters the shell
// interprets (a manifest path may contain ' " $ ; &): anything outside a
// plain set is single-quoted, and a quote inside it closes the quoting,
// adds an escaped quote and reopens it. A ~ is never plain, since it
// expands at the start of a word.
func shellArg(s string) string {
	plain := s != ""
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("@%+=:,./_-", c)) {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
