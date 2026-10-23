package process

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// With no process in use on the hub, the next step explains the values
// with an example each; with some, it offers each one as a complete
// command, in the order given, and at most inUseShown of them: by its
// exact commit, with the tag it was selected by as --ref.
func TestNextStep(t *testing.T) {
	got := NextStep("new", nil)
	for _, want := range []string{
		"aimem process select <repo> <tag> [<manifest>] -p new",
		"<repo>      the process repository's git URL, e.g. https://",
		"<tag>       a release tag of that repository, e.g. v1.4.0; a full 40-hex commit also works",
		"<manifest>  optional: the manifest's path in that repository, by default processes/aimem/manifest.json",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("no process in use: lacks %q:\n%s", want, got)
		}
	}

	if strings.Contains(got, ReferenceRepo) {
		t.Fatalf("a dev build names a reference tag it does not have:\n%s", got)
	}

	c1, c2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	got = NextStep("new", []InUse{
		{Repo: "https://example.com/p.git", Ref: "v1.4.0", Commit: c1, Manifest: "m.json", Projects: []string{"alpha", "beta"}},
		{Repo: "git@example.com:o/q.git", Commit: c2, Manifest: "x/m.json", Projects: []string{"gamma"}},
	})
	first := "  as alpha, beta:\n    aimem process select https://example.com/p.git " + c1 + " m.json --ref v1.4.0 -p new\n"
	second := "  as gamma:\n    aimem process select git@example.com:o/q.git " + c2 + " x/m.json -p new\n"
	if !strings.Contains(got, first) || !strings.Contains(got, second) || strings.Index(got, first) > strings.Index(got, second) {
		t.Fatalf("processes in use:\n%s", got)
	}
	if strings.Contains(got, "<repo>      ") {
		t.Fatalf("the example explanation is printed although processes are in use:\n%s", got)
	}

	var many []InUse
	for i := range inUseShown + 2 {
		many = append(many, InUse{Repo: "https://example.com/p.git", Commit: fmt.Sprintf("%040d", i), Manifest: "m.json", Projects: []string{fmt.Sprint("p", i)}})
	}
	got = NextStep("new", many)
	if strings.Count(got, "    aimem process select ") != inUseShown || !strings.Contains(got, "(2 more not shown)") {
		t.Fatalf("more than %d offered:\n%s", inUseShown, got)
	}
}

// A release build names the reference process at its own tag, above the
// generic form; a dev build or a non-release version names none.
func TestNextStepReferenceProcess(t *testing.T) {
	defer func(r string) { Release = r }(Release)
	Release = "v0.10.14"
	got := NextStep("new", nil)
	ref := "    aimem process select https://github.com/BlackVS/aimem.git v0.10.14 -p new\n"
	generic := "  aimem process select <repo> <tag> [<manifest>] -p new\n"
	if !strings.Contains(got, ref) || !strings.Contains(got, generic) || strings.Index(got, ref) > strings.Index(got, generic) {
		t.Fatalf("release build:\n%s", got)
	}
	for _, r := range []string{"dev", "", "v0.10.14-3-gabc1234", "0.10.14"} {
		Release = r
		if got := NextStep("new", nil); strings.Contains(got, ReferenceRepo) {
			t.Fatalf("version %q names a reference tag:\n%s", r, got)
		}
	}
}

// An offered command runs as printed: a selection may hold characters a
// shell interprets (the manifest path accepts ' " $ ; & |), and a POSIX
// shell must read every argument back unchanged.
func TestNextStepQuotesForTheShell(t *testing.T) {
	sel := Ref{Repo: "https://example.com/team$1;x.git", Ref: "main", Commit: strings.Repeat("1", 40), Manifest: `process/team's"$HOME;&|~x.json`}
	if err := sel.Validate(); err != nil {
		t.Fatalf("the test selection must be one the hub accepts: %v", err)
	}
	got := NextStep("beta", []InUse{{Repo: sel.Repo, Ref: sel.Ref, Commit: sel.Commit, Manifest: sel.Manifest, Projects: []string{"alpha"}}})
	if !strings.Contains(got, `'process/team'\''s"$HOME;&|~x.json'`) || !strings.Contains(got, `'https://example.com/team$1;x.git'`) {
		t.Fatalf("not quoted:\n%s", got)
	}
	var line string
	for _, l := range strings.Split(got, "\n") {
		if strings.HasPrefix(l, "    aimem process select ") {
			line = strings.TrimPrefix(l, "    aimem process select ")
		}
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on this machine to read the command back")
	}
	out, err := exec.Command(sh, "-c", `printf '%s\n' `+line).Output()
	if err != nil {
		t.Fatalf("sh could not read the command: %v", err)
	}
	want := strings.Join([]string{sel.Repo, sel.Commit, sel.Manifest, "--ref", "main", "-p", "beta"}, "\n") + "\n"
	if string(out) != want {
		t.Fatalf("sh read back\n%q\nwant\n%q", out, want)
	}
}
