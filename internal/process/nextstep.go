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
// it uses none, with the values explained. A process in use is offered by
// the tag it was selected by, with the commit the other projects follow,
// or by that commit when it was selected by commit alone. It names
// commands only; the admin chooses and runs one, which records the pin
// like any other selection.
func NextStep(project string, inUse []InUse) string {
	var b strings.Builder
	if len(inUse) == 0 {
		b.WriteString("next step: select the project's process on this hub host:\n")
		fmt.Fprintf(&b, "  aimem process select <repo> <tag> [<manifest>] -p %s\n", project)
		b.WriteString("    <repo>      the process repository's git URL, e.g. https://github.com/example/process.git\n")
		b.WriteString("    <tag>       a release tag of that repository, e.g. v1.4.0; a full 40-hex commit also works\n")
		fmt.Fprintf(&b, "    <manifest>  optional: the manifest's path in that repository, by default %s\n", DefaultManifest)
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
		if u.Ref != "" {
			fmt.Fprintf(&b, "    aimem process select %s %s %s -p %s  # at %s\n", shellArg(u.Repo), shellArg(u.Ref), shellArg(u.Manifest), shellArg(project), u.Commit)
		} else {
			fmt.Fprintf(&b, "    aimem process select %s %s %s -p %s\n", shellArg(u.Repo), shellArg(u.Commit), shellArg(u.Manifest), shellArg(project))
		}
	}
	fmt.Fprintf(&b, "  or another process: aimem process select <repo> <tag> [<manifest>] -p %s\n", shellArg(project))
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
