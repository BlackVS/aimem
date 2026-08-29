// Package diff3 is a line-based three-way merge for shared documents
// (FEATURE-PROPOSALS #3): given the BASE both sides diverged from, it
// applies non-overlapping edits from each side automatically and marks
// overlapping ones as conflicts — turning DESIGN-shared-docs' "re-read,
// merge deliberately" from an instruction into a mechanism. Pure Go,
// no dependencies, sized for handoffs and runbooks (not for code
// trees: MaxLines guards the O(n·m) LCS).
package diff3

import (
	"fmt"
	"strings"
)

// MaxLines bounds each input; the LCS table is O(n·m) cells. 4000² is
// ~64MB of int32 worst-case — fine for a one-shot CLI merge, and far
// beyond any sane handoff or runbook.
const MaxLines = 4000

// hunk is one edit against the base: base[b1:b2) becomes repl.
// A pure insertion has b1 == b2.
type hunk struct {
	b1, b2 int
	repl   []string
}

// diff returns the edit script base -> other as ordered, disjoint
// hunks, derived from a longest-common-subsequence alignment.
func diff(base, other []string) []hunk {
	n, m := len(base), len(other)
	// lcs[i][j] = LCS length of base[i:], other[j:].
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if base[i] == other[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	var hunks []hunk
	i, j := 0, 0
	for i < n || j < m {
		if i < n && j < m && base[i] == other[j] {
			i++
			j++
			continue
		}
		// A mismatch region: extend it until the alignment matches again.
		b1, o1 := i, j
		for i < n || j < m {
			if i < n && j < m && base[i] == other[j] {
				break
			}
			if j < m && (i >= n || lcs[i][j+1] >= lcs[i+1][j]) {
				j++
			} else {
				i++
			}
		}
		hunks = append(hunks, hunk{b1: b1, b2: i, repl: append([]string(nil), other[o1:j]...)})
	}
	return hunks
}

// overlaps reports whether two hunks touch the same base region. Pure
// insertions at the same point overlap, and an insertion inside (or at
// the start of) a changed range overlaps it — deliberately
// conservative: when edit order is ambiguous, diff3 must conflict, not
// guess.
func overlaps(a, b hunk) bool {
	if a.b1 < b.b2 && b.b1 < a.b2 {
		return true
	}
	return a.b1 == b.b1 && (a.b1 == a.b2 || b.b1 == b.b2)
}

// applyWithin rebuilds one side's text for the base range [c1:c2) from
// its hunks inside that range.
func applyWithin(base []string, c1, c2 int, hs []hunk) []string {
	var out []string
	cur := c1
	for _, h := range hs {
		out = append(out, base[cur:h.b1]...)
		out = append(out, h.repl...)
		cur = h.b2
	}
	return append(out, base[cur:c2]...)
}

// Result is a completed merge.
type Result struct {
	Lines     []string
	Conflicts int
}

// Merge three-way-merges lines. localLabel and hubLabel name the sides
// inside conflict markers.
func Merge(base, local, hub []string, localLabel, hubLabel string) (Result, error) {
	if len(base) > MaxLines || len(local) > MaxLines || len(hub) > MaxLines {
		return Result{}, fmt.Errorf("inputs exceed %d lines; merge by hand", MaxLines)
	}
	hl := diff(base, local)
	hh := diff(base, hub)
	var out []string
	conflicts := 0
	pos := 0 // consumed base lines
	li, hi := 0, 0
	for li < len(hl) || hi < len(hh) {
		// Open a cluster with the earliest remaining hunk, then absorb
		// every hunk (from either side) that overlaps the growing range.
		var c1, c2 int
		switch {
		case hi >= len(hh) || (li < len(hl) && hl[li].b1 <= hh[hi].b1):
			c1, c2 = hl[li].b1, hl[li].b2
		default:
			c1, c2 = hh[hi].b1, hh[hi].b2
		}
		lStart, hStart := li, hi
		for {
			grew := false
			for li < len(hl) && overlaps(hl[li], hunk{b1: c1, b2: c2}) {
				if hl[li].b2 > c2 {
					c2 = hl[li].b2
				}
				li++
				grew = true
			}
			for hi < len(hh) && overlaps(hh[hi], hunk{b1: c1, b2: c2}) {
				if hh[hi].b2 > c2 {
					c2 = hh[hi].b2
				}
				hi++
				grew = true
			}
			if !grew {
				break
			}
		}
		out = append(out, base[pos:c1]...)
		lSide := applyWithin(base, c1, c2, hl[lStart:li])
		hSide := applyWithin(base, c1, c2, hh[hStart:hi])
		switch {
		case li == lStart: // only the hub changed this region
			out = append(out, hSide...)
		case hi == hStart: // only local changed it
			out = append(out, lSide...)
		case equal(lSide, hSide): // both made the identical change
			out = append(out, lSide...)
		default:
			conflicts++
			out = append(out, "<<<<<<< "+localLabel)
			out = append(out, lSide...)
			out = append(out, "=======")
			out = append(out, hSide...)
			out = append(out, ">>>>>>> "+hubLabel)
		}
		pos = c2
	}
	out = append(out, base[pos:]...)
	return Result{Lines: out, Conflicts: conflicts}, nil
}

// MergeText is the string-in, string-out convenience over Merge.
func MergeText(base, local, hub, localLabel, hubLabel string) (string, int, error) {
	res, err := Merge(splitLines(base), splitLines(local), splitLines(hub), localLabel, hubLabel)
	if err != nil {
		return "", 0, err
	}
	if len(res.Lines) == 0 {
		return "", res.Conflicts, nil
	}
	return strings.Join(res.Lines, "\n") + "\n", res.Conflicts, nil
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
