package server

import (
	"strings"
	"testing"
)

// A list row's epic label is bounded: an epic title of any length is cut
// with an ellipsis (the full title on hover), so the meta, which never
// wraps, cannot squeeze the task title into a narrow column. The board
// card wraps its meta and keeps the plain label.
func TestTasksListRowBoundsTheEpicLabel(t *testing.T) {
	page := string(tasksHTML)
	const label = "esc(epicLabel(t.epic))"
	row := "` · <span class=\"epic\" title=\"${" + label + "}\">${" + label + "}</span>`"
	if strings.Count(page, row) != 1 {
		t.Fatalf("the list row does not wrap the epic label in a bounded span with its full title:\n%s", row)
	}
	css := ".trow .meta .epic{"
	i := strings.Index(page, css)
	if i < 0 {
		t.Fatal("no .trow .meta .epic rule")
	}
	rule := page[i : i+strings.Index(page[i:], "}")]
	for _, d := range []string{"display:inline-block", "max-width:", "overflow:hidden", "text-overflow:ellipsis"} {
		if !strings.Contains(rule, d) {
			t.Fatalf("the epic label rule lacks %q: %s", d, rule)
		}
	}
	// Every label goes through esc, and the board card keeps its own span.
	if n := strings.Count(page, "epicLabel(t.epic)"); n != strings.Count(page, label) {
		t.Fatalf("an epic label is rendered without esc: %d uses, %d escaped", n, strings.Count(page, label))
	}
	if !strings.Contains(page, "${t.epic?`<span>${"+label+"}</span>`:''}") {
		t.Fatal("the board card's epic label changed")
	}
}
