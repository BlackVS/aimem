package tui

import (
	"encoding/json"
	"strings"
	"testing"
)

// The hub panel shows a refusal of the provider account under the pace
// line, as health reports it, and shows nothing when there is none.
func TestHubPanelShowsTheLastRefusal(t *testing.T) {
	for _, c := range []struct {
		pace string
		want bool
	}{
		{`{"interval_s":2,"penalty_s":0,"spacing_s":2,"blocks":0,"last_refusal":{"at":"2026-11-18T10:00:00Z","message":"provider refused the account (HTTP 429, insufficient_quota): no credits"}}`, true},
		{`{"interval_s":2,"penalty_s":0,"spacing_s":2,"blocks":0}`, false},
	} {
		var pace map[string]any
		if err := json.Unmarshal([]byte(c.pace), &pace); err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		model{}.renderHubOne(&b, hubLine{Name: "hub", OK: true, Pace: pace})
		got := strings.Contains(b.String(), "llm refused: provider refused the account (HTTP 429, insufficient_quota): no credits") &&
			strings.Contains(b.String(), "the provider account needs the administrator")
		if got != c.want {
			t.Fatalf("refusal shown %v, want %v:\n%s", got, c.want, b.String())
		}
	}
}
