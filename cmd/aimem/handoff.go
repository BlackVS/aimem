package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"aimem/internal/redact"
)

const (
	// handoffMarker separates the current state (above) from history
	// (below) in docs/SESSION-STATE.md; compact moves what is below it.
	handoffMarker    = "<!-- aimem: history below -->"
	handoffLineLimit = 50
	// A trailing "Pick up here" section longer than this is not a pick-up
	// line but a section compact treats like any other.
	handoffTailMaxLines = 10
	// The summary block --summarize adds, at most.
	handoffSummaryMaxLines = 12
	handoffSummaryMaxBytes = 1024
)

func handoffCmd(args []string) error {
	if len(args) == 0 || args[0] != "compact" {
		return fmt.Errorf("usage: aimem handoff compact [--file docs/SESSION-STATE.md] [--lines N] [--summarize] [--dry-run]")
	}
	fs := flag.NewFlagSet("handoff compact", flag.ExitOnError)
	file := fs.String("file", filepath.Join("docs", "SESSION-STATE.md"), "the handoff to compact")
	lines := fs.Int("lines", handoffLineLimit, "line limit for the current state when the file has no marker")
	summarize := fs.Bool("summarize", false, "add a short summary of the moved part, written by the curation backend")
	backend := fs.String("backend", envOr("AIMEM_CURATE_BACKEND", "claude"), "summary backend for an unbound model: claude or openai")
	model := fs.String("model", "", "summary model (default: AIMEM_CURATE_MODEL, else haiku for the claude backend)")
	dry := fs.Bool("dry-run", false, "print what would move and write nothing")
	fs.Parse(args[1:])

	raw, err := os.ReadFile(*file)
	if err != nil {
		return fmt.Errorf("no handoff to compact: %w", err)
	}
	body := strings.TrimPrefix(string(raw), "\xef\xbb\xbf")
	var reserveLines, reserveBytes int
	if *summarize {
		// The summary block, with its heading and blank lines.
		reserveLines, reserveBytes = handoffSummaryMaxLines+3, handoffSummaryMaxBytes+128
	}
	plan := planCompact(body, *lines, handoffInjectLimit, reserveLines, reserveBytes)
	archive := filepath.Join(filepath.Dir(*file), "SESSION-STATE-ARCHIVE.md")
	if plan.marker && (countLines(plan.current) > *lines || len(plan.current) > handoffInjectLimit) {
		fmt.Printf("warning: the current state above %s is %d lines, %d bytes, over %d lines or %d bytes; "+
			"compact moves only what is below the line, so shorten the current state itself.\n",
			handoffMarker, countLines(plan.current), len(plan.current), *lines, handoffInjectLimit)
	}
	if plan.moved == "" {
		fmt.Printf("aimem handoff compact: %s is already compact (%d lines, %d bytes); nothing moved.\n",
			*file, countLines(body), len(body))
		return nil
	}

	now := time.Now().UTC()
	heading := fmt.Sprintf("## Archived %s from %s", now.Format("2006-01-02 15:04Z"), filepath.ToSlash(*file))
	fmt.Printf("aimem handoff compact: moving lines %d-%d of %s (%d lines, %d bytes) to %s under %q.\n",
		plan.first, plan.last, *file, countLines(plan.moved), len(plan.moved), archive, heading)
	if hs := markdownHeadings(plan.moved); len(hs) > 0 {
		fmt.Printf("moved headings: %s\n", strings.Join(hs, "; "))
	}

	summary := ""
	if *summarize && *dry {
		fmt.Println("summary: not requested in a dry run.")
	} else if *summarize {
		s, why := handoffSummary(plan.moved, *model, *backend)
		if why != "" {
			fmt.Printf("summary: not added (%s); the moved part is archived only.\n", why)
		} else {
			summary = "## Summary of archived history (" + now.Format("2006-01-02") + ")" + plan.eol + plan.eol +
				strings.ReplaceAll(s, "\n", plan.eol) + plan.eol + plan.eol
			fmt.Printf("summary: added %d lines to the current state.\n", countLines(s))
		}
	}
	current := insertBeforePickUp(plan.current, summary)
	out := current + handoffMarker + plan.eol
	if *dry {
		fmt.Printf("dry run: would keep %d lines (%d bytes); nothing written.\n", countLines(out), len(out))
		return nil
	}

	// The archive first: a run that stops between the two writes leaves
	// the history in both files, and the rerun finds it already archived.
	old, err := os.ReadFile(archive)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.HasSuffix(strings.TrimRight(string(old), "\r\n"), plan.moved) {
		fmt.Printf("%s already ends with this block; not appended again.\n", archive)
	} else {
		a := string(old)
		if a == "" {
			a = "# Session state archive" + plan.eol + plan.eol +
				"History moved out of " + filepath.ToSlash(*file) + " by `aimem handoff compact`. It is never injected into a session." + plan.eol
		}
		a = strings.TrimRight(a, "\r\n") + plan.eol + plan.eol + heading + plan.eol + plan.eol + plan.moved + plan.eol
		if err := writeFileAtomic(archive, []byte(a)); err != nil {
			return err
		}
	}
	if err := writeFileAtomic(*file, []byte(out)); err != nil {
		return err
	}
	fmt.Printf("kept %d lines (%d bytes) in %s; history goes below the line %s.\n", countLines(out), len(out), *file, handoffMarker)
	return nil
}

// compactPlan is how compact splits a handoff. current is the new current
// state without the marker line; moved is the history to archive, trimmed
// of blank lines at either end ("" when nothing moves); first and last are
// its 1-based line range in the file.
type compactPlan struct {
	current, moved string
	first, last    int
	eol            string
	marker         bool // the file has the marker line
}

// planCompact splits body. With the marker, everything below it moves.
// Without one, a file within lineLimit and byteLimit stays as it is;
// otherwise the current state is the head that fits the limits less the
// reserve (room for a summary block), ending
// before a heading or blank line where one falls in its second half, plus
// a short trailing "Pick up here" section, and the rest moves.
func planCompact(body string, lineLimit, byteLimit, reserveLines, reserveBytes int) compactPlan {
	p := compactPlan{eol: "\n"}
	if strings.Contains(body, "\r\n") {
		p.eol = "\r\n"
	}
	lines := strings.SplitAfter(body, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 && !strings.HasSuffix(lines[len(lines)-1], "\n") {
		lines[len(lines)-1] += p.eol
	}
	for i, l := range lines {
		if strings.TrimSpace(l) == handoffMarker {
			p.current, p.marker = strings.Join(lines[:i], ""), true
			p.setMoved(lines, i+1, len(lines))
			return p
		}
	}
	if len(lines) <= lineLimit && len(body) <= byteLimit {
		p.current = body
		return p
	}
	pre, tail := lines, []string(nil)
	for i := len(lines) - 1; i > 0 && len(lines)-i <= handoffTailMaxLines; i-- {
		if isPickUp(lines[i]) {
			pre, tail = lines[:i], lines[i:]
			break
		}
	}
	tailText := strings.Join(tail, "")
	// The marker line and up to two blank lines are added around the tail.
	budgetLines := lineLimit - reserveLines - len(tail) - 3
	budgetBytes := byteLimit - reserveBytes - len(tailText) - len(handoffMarker) - 3*len(p.eol)
	n, size := 0, 0
	for n < len(pre) && n < budgetLines && size+len(pre[n]) <= budgetBytes {
		size += len(pre[n])
		n++
	}
	if n < len(pre) {
		n = cutBoundary(pre, n)
	}
	head := strings.Join(pre[:n], "")
	if head != "" && !strings.HasSuffix(head, p.eol+p.eol) && tailText != "" {
		head += p.eol
	}
	p.current = head + tailText
	p.setMoved(pre, n, len(pre))
	if p.moved == "" {
		p.current = body
	}
	if p.current != "" && !strings.HasSuffix(p.current, p.eol+p.eol) {
		p.current += p.eol
	}
	return p
}

// cutBoundary moves a cut before line n back to the nearest heading in the
// second half of the head, so no section is split and no heading is left
// without its body, or else to the nearest blank line there.
func cutBoundary(lines []string, n int) int {
	for _, boundary := range []func(string) bool{
		func(l string) bool { return strings.HasPrefix(l, "#") },
		func(l string) bool { return l == "" },
	} {
		for j := n; j > n/2; j-- {
			if boundary(strings.TrimSpace(lines[j])) {
				return j
			}
		}
	}
	return n
}

// setMoved sets the moved text to lines[from:to] without the blank lines at
// either end, and its 1-based line range.
func (p *compactPlan) setMoved(lines []string, from, to int) {
	for from < to && strings.TrimSpace(lines[from]) == "" {
		from++
	}
	for to > from && strings.TrimSpace(lines[to-1]) == "" {
		to--
	}
	if from == to {
		return
	}
	p.moved = strings.TrimRight(strings.Join(lines[from:to], ""), "\r\n")
	p.first, p.last = from+1, to
}

// isPickUp reports whether a line opens the handoff's closing "Pick up
// here" section, as a heading or as a bold or plain lead-in.
func isPickUp(line string) bool {
	l := strings.ToLower(strings.TrimLeft(strings.TrimSpace(line), "#*_ "))
	return strings.HasPrefix(l, "pick up here")
}

// insertBeforePickUp puts block before the current state's "Pick up here"
// section, or at its end when it has none.
func insertBeforePickUp(current, block string) string {
	if block == "" {
		return current
	}
	lines := strings.SplitAfter(current, "\n")
	for i := len(lines) - 1; i > 0 && len(lines)-i <= handoffTailMaxLines+1; i-- {
		if isPickUp(lines[i]) {
			return strings.Join(lines[:i], "") + block + strings.Join(lines[i:], "")
		}
	}
	return current + block
}

// handoffSummary asks the curation backend for a short summary of the moved
// history. The text is redacted before it leaves the machine. It returns a
// reason instead when there is no backend or the call fails.
func handoffSummary(moved, model, backend string) (string, string) {
	syn, _, err := curateBackend(stateRoot(), model, backend)
	if err != nil {
		return "", "no curation backend: " + err.Error()
	}
	text, _ := redact.String(moved, 64<<10)
	prompt := "Below is history being moved out of a coding project's session handoff into an archive. " +
		"Summarize it in at most 8 short Markdown bullet lines, each starting with \"- Done:\", \"- Decision:\" or \"- Open:\". " +
		"Keep commit hashes, PR numbers, task IDs and file paths exact. Output only the bullet lines.\n\n<history>\n" +
		text + "\n</history>\n"
	out, _, err := syn.Complete(prompt)
	if err != nil {
		return "", "the curation backend failed: " + llmClip(err.Error())
	}
	s := clipSummary(out)
	if s == "" {
		return "", "the curation backend returned no summary"
	}
	return s, ""
}

// clipSummary keeps the non-empty lines of out, within the summary's line
// and byte bounds, as valid UTF-8.
func clipSummary(out string) string {
	var kept []string
	size := 0
	for _, l := range strings.Split(strings.ToValidUTF8(out, ""), "\n") {
		l = strings.TrimRight(l, "\r \t")
		if strings.TrimSpace(l) == "" || strings.HasPrefix(strings.TrimSpace(l), "```") {
			continue
		}
		if len(kept) == handoffSummaryMaxLines || size+len(l)+1 > handoffSummaryMaxBytes {
			break
		}
		kept = append(kept, l)
		size += len(l) + 1
	}
	return strings.Join(kept, "\n")
}

func llmClip(s string) string {
	if len(s) > 200 {
		return strings.ToValidUTF8(s[:200], "") + "..."
	}
	return s
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimRight(s, "\r\n"), "\n") + 1
}

// markdownHeadings lists the Markdown headings in s, outside code fences.
func markdownHeadings(s string) []string {
	var hs []string
	fence := false
	for _, l := range strings.Split(s, "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "```") {
			fence = !fence
			continue
		}
		if !fence && strings.HasPrefix(t, "#") {
			hs = append(hs, t)
		}
	}
	return hs
}
