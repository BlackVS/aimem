package process

import (
	"fmt"
	"strings"
	"testing"
)

// With no process in use on the hub, the next step explains the three
// values with an example each; with some, it offers each one as a
// complete command, in the order given, and at most inUseShown of them.
func TestNextStep(t *testing.T) {
	got := NextStep("new", nil)
	for _, want := range []string{
		"aimem process select <repo> <commit> <manifest> [--ref <branch>] -p new",
		"<repo>      the process repository's git URL, e.g. https://",
		"<commit>    the full 40-hex commit to follow, e.g. 0123456789abcdef0123456789abcdef01234567",
		"<manifest>  the manifest's path in that repository, e.g. process/manifest.json",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("no process in use: lacks %q:\n%s", want, got)
		}
	}

	c1, c2 := strings.Repeat("1", 40), strings.Repeat("2", 40)
	got = NextStep("new", []InUse{
		{Repo: "https://example.com/p.git", Ref: "main", Commit: c1, Manifest: "m.json", Projects: []string{"alpha", "beta"}},
		{Repo: "git@example.com:o/q.git", Commit: c2, Manifest: "x/m.json", Projects: []string{"gamma"}},
	})
	first := "  as alpha, beta:\n    aimem process select https://example.com/p.git " + c1 + " m.json --ref main -p new\n"
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
