package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// grownHandoff is a handoff that kept every completed item: a current
// state, a long delivery history, and a closing "Pick up here" section.
func grownHandoff(eol string, items int) string {
	var b strings.Builder
	w := func(s string) { b.WriteString(s + eol) }
	w("# Session State")
	w("")
	w("Updated: 2026-10-13 06:00Z | Driving session: the implementer")
	w("")
	w("## Objective")
	w("")
	w("Ship the handoff compact command.")
	w("")
	for s := 0; s < items/20; s++ {
		w(fmt.Sprintf("## Verified delivery, batch %d", s))
		w("")
		for i := 0; i < 20; i++ {
			w(fmt.Sprintf("- item %03d: PR #%d merged as abc%04d, CI green, task 01a1%04d DONE.", s*20+i, 100+s*20+i, s*20+i, s*20+i))
		}
		w("")
	}
	w("## Pick up here")
	w("Open the PR after #215 merges.")
	return b.String()
}

func compactCheckout(t *testing.T, body string) (dir, handoff, archive string) {
	t.Helper()
	dir = t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	handoff = filepath.Join(dir, "docs", "SESSION-STATE.md")
	archive = filepath.Join(dir, "docs", "SESSION-STATE-ARCHIVE.md")
	if err := os.WriteFile(handoff, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, handoff, archive
}

func runCompact(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	out, errOut, err := runAimemEnv(t, dir, t.TempDir(), "", env, append([]string{"handoff", "compact"}, args...)...)
	if err != nil {
		t.Fatalf("handoff compact: %v %s %s", err, out, errOut)
	}
	return out
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// nonBlank lists the non-blank lines of s.
func nonBlank(s string) []string {
	var ls []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			ls = append(ls, l)
		}
	}
	return ls
}

// A grown handoff keeps its head and its "Pick up here" section within the
// limits, and every other line moves to the archive under a dated heading:
// no line is lost or duplicated. Session start then injects it whole.
func TestHandoffCompactMovesTheHistoryOfAGrownHandoff(t *testing.T) {
	for _, eol := range []string{"\n", "\r\n"} {
		body := grownHandoff(eol, 200)
		dir, handoff, archive := compactCheckout(t, body)
		out := runCompact(t, dir, nil)

		kept, arch := readFile(t, handoff), readFile(t, archive)
		if n := countLines(kept); n > handoffLineLimit || len(kept) > handoffInjectLimit {
			t.Fatalf("eol %q: the kept state is %d lines, %d bytes", eol, n, len(kept))
		}
		if !strings.HasPrefix(kept, "# Session State"+eol) ||
			!strings.HasSuffix(kept, "## Pick up here"+eol+"Open the PR after #215 merges."+eol+eol+handoffMarker+eol) {
			t.Fatalf("eol %q: the head or the pick-up section was not kept:\n%s", eol, kept)
		}
		if eol == "\n" && strings.Contains(kept+arch, "\r") {
			t.Fatalf("an LF handoff gained CR")
		}
		if eol == "\r\n" && strings.Count(kept+arch, "\n") != strings.Count(kept+arch, "\r\n") {
			t.Fatalf("a CRLF handoff gained bare LF")
		}
		if !strings.HasPrefix(arch, "# Session state archive") || strings.Count(arch, "## Archived ") != 1 ||
			!strings.Contains(arch, "from docs/SESSION-STATE.md"+eol) {
			t.Fatalf("eol %q: archive header or dated heading missing:\n%.400s", eol, arch)
		}
		// The cut falls at a section boundary: each delivery batch kept is
		// kept whole.
		for s := 0; s < 10; s++ {
			if !strings.Contains(kept, fmt.Sprintf("batch %d"+eol, s)) {
				continue
			}
			for i := s * 20; i < s*20+20; i++ {
				if !strings.Contains(kept, fmt.Sprintf("- item %03d:", i)) {
					t.Fatalf("eol %q: batch %d is kept without item %d:\n%s", eol, s, i, kept)
				}
			}
		}
		// Every original line is in exactly one of the two files.
		seen := map[string]int{}
		for _, l := range append(nonBlank(kept), nonBlank(arch)...) {
			seen[l]++
		}
		for _, l := range nonBlank(body) {
			if seen[l] != 1 {
				t.Fatalf("eol %q: line %q appears %d times after compact", eol, l, seen[l])
			}
		}
		for _, want := range []string{"moving lines ", "moved headings: ## Verified delivery", "SESSION-STATE-ARCHIVE.md", "kept "} {
			if !strings.Contains(out, want) {
				t.Errorf("eol %q: output lacks %q:\n%s", eol, want, out)
			}
		}
		if ctx := sessionStartContext(t, dir, nil); strings.Contains(ctx, "session-start limit") || !strings.Contains(ctx, "Open the PR after #215") {
			t.Errorf("eol %q: session start does not inject the compacted handoff whole", eol)
		}
	}
}

// A second run, and a run on a handoff that is already compact, change no
// file and say so.
func TestHandoffCompactIsANoOpOnACompactHandoff(t *testing.T) {
	dir, handoff, archive := compactCheckout(t, grownHandoff("\n", 200))
	runCompact(t, dir, nil)
	kept, arch := readFile(t, handoff), readFile(t, archive)
	if out := runCompact(t, dir, nil); !strings.Contains(out, "already compact") {
		t.Fatalf("second run: %s", out)
	}
	if readFile(t, handoff) != kept || readFile(t, archive) != arch {
		t.Fatal("a second run changed a file")
	}

	small := "# Session State\n\n## Objective\n\nOne thing.\n\n## Pick up here\nDo it.\n"
	dir, handoff, archive = compactCheckout(t, small)
	if out := runCompact(t, dir, nil); !strings.Contains(out, "already compact") {
		t.Fatalf("small handoff: %s", out)
	}
	if readFile(t, handoff) != small {
		t.Fatal("a compact handoff was rewritten")
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("a compact handoff created an archive: %v", err)
	}
}

// With the marker, everything below it moves and everything above it
// stays, however long; history added below later goes in a second block.
func TestHandoffCompactMovesWhatIsBelowTheMarker(t *testing.T) {
	above := "# Session State\n\n## Objective\n\nThe current state.\n\n## Pick up here\nNext step.\n\n"
	dir, handoff, archive := compactCheckout(t, above+handoffMarker+"\n\n## Done\n\n- first history item\n")
	runCompact(t, dir, nil)
	if got := readFile(t, handoff); got != above+handoffMarker+"\n" {
		t.Fatalf("kept:\n%q", got)
	}
	if err := os.WriteFile(handoff, []byte(above+handoffMarker+"\n- second history item\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCompact(t, dir, nil)
	arch := readFile(t, archive)
	if strings.Count(arch, "## Archived ") != 2 || strings.Index(arch, "first history item") > strings.Index(arch, "second history item") {
		t.Fatalf("archive:\n%s", arch)
	}
	if readFile(t, handoff) != above+handoffMarker+"\n" {
		t.Fatal("the current state above the marker changed")
	}
}

// A run that wrote the archive but stopped before rewriting the handoff
// does not archive the same block twice when it is run again.
func TestHandoffCompactRerunAfterAnInterruptedRunDoesNotDuplicate(t *testing.T) {
	body := grownHandoff("\n", 200)
	dir, handoff, archive := compactCheckout(t, body)
	runCompact(t, dir, nil)
	kept, arch := readFile(t, handoff), readFile(t, archive)
	if err := os.WriteFile(handoff, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := runCompact(t, dir, nil); !strings.Contains(out, "not appended again") {
		t.Fatalf("rerun: %s", out)
	}
	if readFile(t, archive) != arch || readFile(t, handoff) != kept {
		t.Fatal("the rerun duplicated the block or kept a different state")
	}
}

// --dry-run prints the plan and writes nothing.
func TestHandoffCompactDryRunWritesNothing(t *testing.T) {
	body := grownHandoff("\n", 200)
	dir, handoff, archive := compactCheckout(t, body)
	if out := runCompact(t, dir, nil, "--dry-run"); !strings.Contains(out, "dry run") {
		t.Fatalf("dry run: %s", out)
	}
	if readFile(t, handoff) != body {
		t.Fatal("dry run rewrote the handoff")
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatal("dry run wrote the archive")
	}
}

// --summarize asks the curation backend, with the history redacted, and
// puts its bounded summary before the "Pick up here" section; without a
// backend it archives only and says why.
func TestHandoffCompactSummarize(t *testing.T) {
	secret := "gh" + "p_" + strings.Repeat("a1", 18)
	body := strings.Replace(grownHandoff("\n", 200), "- item 150:", "- item 150: token "+secret+";", 1)
	var prompt string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		prompt = string(b)
		var lines []string
		for i := 0; i < 30; i++ {
			lines = append(lines, fmt.Sprintf("- Done: summary line %d", i))
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, "```\n"+strings.Join(lines, "\n")+"\n```")
	}))
	defer ts.Close()
	env := []string{"AIMEM_CURATE_BACKEND=openai", "AIMEM_CURATE_MODEL=fake", "AIMEM_OPENAI_BASE_URL=" + ts.URL, "AIMEM_OPENAI_API_KEY=k"}

	dir, handoff, archive := compactCheckout(t, body)
	out := runCompact(t, dir, env, "--summarize")
	kept := readFile(t, handoff)
	if !strings.Contains(prompt, "item 199") || strings.Contains(prompt, secret) {
		t.Fatalf("the backend did not get the redacted history (has item 199: %v, has secret: %v)",
			strings.Contains(prompt, "item 199"), strings.Contains(prompt, secret))
	}
	i, j := strings.Index(kept, "## Summary of archived history"), strings.Index(kept, "## Pick up here")
	if i < 0 || j < i || !strings.Contains(kept, "- Done: summary line 0") || strings.Contains(kept, "summary line 12") ||
		strings.Contains(kept, "```") || !strings.Contains(out, "summary: added 12 lines") {
		t.Fatalf("summary block:\n%s\n%s", kept, out)
	}
	if n := countLines(kept); n > handoffLineLimit || len(kept) > handoffInjectLimit {
		t.Fatalf("with the summary the kept state is %d lines, %d bytes", n, len(kept))
	}
	if !strings.Contains(readFile(t, archive), "item 199") {
		t.Fatal("history not archived")
	}

	// A dry run never calls the backend.
	prompt = ""
	dir, _, _ = compactCheckout(t, body)
	if out := runCompact(t, dir, env, "--summarize", "--dry-run"); prompt != "" || !strings.Contains(out, "not requested in a dry run") {
		t.Fatalf("a dry run called the backend: %s", out)
	}

	// A handoff compact by the plain limits stays untouched with
	// --summarize too, and the backend is not asked.
	prompt = ""
	small := strings.Repeat("- a current item\n", handoffLineLimit-10)
	dir, handoff, _ = compactCheckout(t, small)
	if out := runCompact(t, dir, env, "--summarize"); prompt != "" || !strings.Contains(out, "already compact") || readFile(t, handoff) != small {
		t.Fatalf("--summarize cut a compact handoff: %s", out)
	}

	// Also with no backend at all.
	if out := runCompact(t, dir, []string{"AIMEM_CURATE_BACKEND=openai", "AIMEM_CURATE_MODEL="}, "--summarize"); !strings.Contains(out, "already compact") || readFile(t, handoff) != small {
		t.Fatalf("--summarize without a backend cut a compact handoff: %s", out)
	}

	// Above a marker nothing is cut, so the summary gets only the room
	// left: 40 lines above it leave 6 bullet lines within 50 lines.
	// The state ends with a blank line, which counts as a line.
	above := "## Pick up here\n" + strings.Repeat("- a current item\n", 38) + "\n"
	dir, handoff, _ = compactCheckout(t, above+handoffMarker+"\n- history\n")
	if out := runCompact(t, dir, env, "--summarize"); !strings.Contains(out, "summary: added 6 lines") {
		t.Fatalf("marker with summary: %s", out)
	}
	if n := countLines(readFile(t, handoff)); n > handoffLineLimit {
		t.Fatalf("the summary took the marker-mode state to %d lines", n)
	}
	// With no room left the backend is not asked and nothing is added.
	prompt = ""
	full := ""
	for len(full)+10 <= handoffInjectLimit-len(handoffMarker)-20 {
		full += "- item 01\n"
	}
	dir, handoff, _ = compactCheckout(t, full+handoffMarker+"\n- history\n")
	if out := runCompact(t, dir, env, "--summarize", "--lines", "2000"); prompt != "" || !strings.Contains(out, "no room left") || readFile(t, handoff) != full+handoffMarker+"\n" {
		t.Fatalf("no room: %s", out)
	}

	dir, handoff, archive = compactCheckout(t, body)
	out = runCompact(t, dir, []string{"AIMEM_CURATE_BACKEND=openai", "AIMEM_CURATE_MODEL="}, "--summarize")
	if !strings.Contains(out, "summary: not added (no curation backend") || strings.Contains(readFile(t, handoff), "## Summary") ||
		!strings.Contains(readFile(t, archive), "item 199") {
		t.Fatalf("without a backend: %s", out)
	}
}

// The session-start note over the limit names the command.
func TestSessionStartNoteNamesHandoffCompact(t *testing.T) {
	if note := handoffHead("docs/SESSION-STATE.md", strings.Repeat("x\n", handoffInjectLimit)); !strings.Contains(note, "`aimem handoff compact`") {
		t.Fatalf("note: %q", note[len(note)-300:])
	}
}

// A current state above the marker that is itself over the limits is kept,
// and the run says so.
func TestHandoffCompactWarnsWhenTheStateAboveTheMarkerIsTooLong(t *testing.T) {
	above := strings.Repeat("- a current item\n", handoffLineLimit+5)
	dir, handoff, _ := compactCheckout(t, above+handoffMarker+"\n- history\n")
	if out := runCompact(t, dir, nil); !strings.Contains(out, "warning: the current state above") {
		t.Fatalf("no warning: %s", out)
	}
	if readFile(t, handoff) != above+handoffMarker+"\n" {
		t.Fatal("the current state above the marker was cut")
	}
}

// Where every line is a boundary, the cut lands at the line limit itself:
// the kept state fills the limit and never exceeds it.
func TestHandoffCompactFillsTheLineLimit(t *testing.T) {
	var b strings.Builder
	for i := range 200 {
		fmt.Fprintf(&b, "## step %03d\n", i)
	}
	dir, handoff, _ := compactCheckout(t, b.String())
	runCompact(t, dir, nil)
	if n := countLines(readFile(t, handoff)); n > handoffLineLimit || n < handoffLineLimit-2 {
		t.Fatalf("kept %d lines; want close to %d", n, handoffLineLimit)
	}
}

// A grown handoff keeps its closing section under any of the closing
// names, in any case and as a heading or a lead-in; without one, or under a
// heading that only starts with a name, the last section is archived.
func TestHandoffCompactKeepsTheClosingSectionByName(t *testing.T) {
	var closings []string
	for _, name := range handoffClosingNames {
		closings = append(closings, "## "+name, "### "+strings.ToLower(name), "## "+strings.ToUpper(name)+":", "**"+name+":**")
	}
	closings = append(closings, "## Exact next action (2026-10-13)", "Exact next action: run the tests", "## Next — Monday", "## Next steps - after the merge")
	for _, closing := range closings {
		body := strings.Replace(grownHandoff("\n", 200), "## Pick up here", closing, 1)
		dir, handoff, archive := compactCheckout(t, body)
		runCompact(t, dir, nil)
		kept, arch := readFile(t, handoff), readFile(t, archive)
		if !strings.HasSuffix(kept, closing+"\nOpen the PR after #215 merges.\n\n"+handoffMarker+"\n") ||
			strings.Contains(arch, closing) || strings.Contains(arch, "Open the PR") {
			t.Errorf("closing %q was not kept:\n%s", closing, kept)
		}
		if n := countLines(kept); n > handoffLineLimit || len(kept) > handoffInjectLimit {
			t.Errorf("closing %q: the kept state is %d lines, %d bytes", closing, n, len(kept))
		}
	}
	for _, other := range []string{"## Notes", "## Next week", "## Pick up the pieces", "- Next: a list item"} {
		body := strings.Replace(grownHandoff("\n", 200), "## Pick up here", other, 1)
		dir, handoff, archive := compactCheckout(t, body)
		runCompact(t, dir, nil)
		kept, arch := readFile(t, handoff), readFile(t, archive)
		if strings.Contains(kept, other) || strings.Contains(kept, "Open the PR") ||
			!strings.HasSuffix(strings.TrimRight(arch, "\n"), other+"\nOpen the PR after #215 merges.") {
			t.Errorf("%q is not a closing section, yet it was kept:\n%s", other, kept)
		}
		if n := countLines(kept); n > handoffLineLimit || len(kept) > handoffInjectLimit || !strings.HasPrefix(kept, "# Session State\n") {
			t.Errorf("%q: the kept state is %d lines, %d bytes", other, n, len(kept))
		}
	}
}

// A closing section whose body has a line that is itself a closing name
// ("Next: ...") is kept from its heading, not from that line; so is a
// "Pick up here" heading with any continuation, as before.
func TestHandoffCompactKeepsTheClosingSectionFromItsFirstLine(t *testing.T) {
	for _, closing := range []string{"## Pick up here\nNext: open the PR.", "## Exact next action\n**Next action:** open the PR.\nThen: wait.", "## Pick up here today\nOpen the PR."} {
		body := strings.Replace(grownHandoff("\n", 200), "## Pick up here\nOpen the PR after #215 merges.", closing, 1)
		dir, handoff, archive := compactCheckout(t, body)
		runCompact(t, dir, nil)
		kept, arch := readFile(t, handoff), readFile(t, archive)
		first := strings.SplitN(closing, "\n", 2)[0]
		if !strings.HasSuffix(kept, closing+"\n\n"+handoffMarker+"\n") || strings.Contains(arch, first) {
			t.Errorf("closing %q was not kept whole:\n%s", closing, kept)
		}
	}
	current := "# Session State\n\n## Pick up here\nNext: open the PR.\n\n"
	if got := insertBeforePickUp(current, "## Summary\n\n- Done: x\n\n", -1); !strings.Contains(got, "- Done: x\n\n## Pick up here\nNext:") {
		t.Errorf("summary not before the closing heading:\n%s", got)
	}
}

// A closing section at the full 10 lines, with a "Next:" line inside it,
// gets the summary before its heading, through planning and insertion both
// with a cut and above a marker.
func TestHandoffSummaryBeforeAFullClosingSection(t *testing.T) {
	closing := "## Next steps\n- one\n- two\nNext: three\n- four\n- five\n- six\n- seven\n- eight\n- nine\n"
	block := "## Summary\n\n- Done: x\n\n"
	body := strings.Replace(grownHandoff("\n", 200), "## Pick up here\nOpen the PR after #215 merges.\n", closing, 1)
	plan := planCompact(body, handoffLineLimit, handoffInjectLimit, handoffSummaryMaxLines+3, handoffSummaryMaxBytes+128)
	if plan.moved == "" || !strings.HasSuffix(plan.current, closing+"\n") {
		t.Fatalf("the 10-line closing section was not kept:\n%s", plan.current)
	}
	if got := insertBeforePickUp(plan.current, block, plan.closing); !strings.HasSuffix(got, block+closing+"\n") {
		t.Errorf("after a cut, the summary is not before the closing heading:\n%s", got)
	}
	// A head that ends with a "Next:" lead-in within the last lines: the
	// summary still goes where the plan put the closing section.
	var hb strings.Builder
	hb.WriteString("# Session State\n\n")
	for i := 0; i < 60; i++ {
		lead := "- h"
		if i < 20 {
			lead = "Next: h"
		}
		fmt.Fprintf(&hb, "## H %d\n%s %d\n\n", i, lead, i)
	}
	hb.WriteString("## Pick up here\nGo.\n")
	short := planCompact(hb.String(), handoffLineLimit, handoffInjectLimit, handoffSummaryMaxLines+3, handoffSummaryMaxBytes+128)
	if !strings.Contains(short.current, "Next: h") || !strings.HasSuffix(short.current, "## Pick up here\nGo.\n\n") {
		t.Fatalf("the head should end with a Next: section and keep the closing one:\n%s", short.current)
	}
	if got := insertBeforePickUp(short.current, block, short.closing); !strings.HasSuffix(got, block+"## Pick up here\nGo.\n\n") {
		t.Errorf("the summary is not before the planned closing section:\n%s", got)
	}

	above := "# Session State\n\n" + closing + "\n"
	marked := planCompact(above+handoffMarker+"\n- old history\n", handoffLineLimit, handoffInjectLimit, 0, 0)
	if got := insertBeforePickUp(marked.current, block, marked.closing); got != "# Session State\n\n"+block+closing+"\n" {
		t.Errorf("above a marker, the summary is not before the closing heading:\n%s", got)
	}
}

// The summary goes before the closing section whatever its name.
func TestHandoffSummaryGoesBeforeEveryClosingName(t *testing.T) {
	for _, name := range handoffClosingNames {
		current := "# Session State\n\n## Objective\n\nOne thing.\n\n## " + name + "\nDo it.\n\n"
		got := insertBeforePickUp(current, "## Summary\n\n- Done: x\n\n", -1)
		if !strings.Contains(got, "- Done: x\n\n## "+name+"\nDo it.") {
			t.Errorf("%q: summary not before the closing section:\n%s", name, got)
		}
	}
}

// The command's usage names every closing name.
func TestHandoffUsageNamesTheClosingNames(t *testing.T) {
	_, errOut, err := runAimemEnv(t, t.TempDir(), t.TempDir(), "", nil, "handoff")
	if err == nil {
		t.Fatal("aimem handoff without compact succeeded")
	}
	for _, name := range handoffClosingNames {
		if !strings.Contains(errOut, name) {
			t.Errorf("usage lacks %q:\n%s", name, errOut)
		}
	}
}
