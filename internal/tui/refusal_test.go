package tui

import (
	"encoding/json"
	"strings"
	"testing"
)

// The hub panel shows each refusal of the provider account under the pace
// line, with its kind of call, as health reports them, and shows nothing
// when there is none.
func TestHubPanelShowsTheLastRefusal(t *testing.T) {
	var pace map[string]any
	if err := json.Unmarshal([]byte(`{"interval_s":2,"penalty_s":0,"spacing_s":2,"blocks":0,"last_refusal":[
		{"kind":"completions","at":"2026-11-18T10:00:00Z","message":"provider refused the account (HTTP 429, insufficient_quota): no credits"},
		{"kind":"embeddings","at":"2026-11-18T10:00:00Z","message":"provider refused the account (HTTP 401, key refused)"}]}`), &pace); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	model{}.renderHubOne(&b, hubLine{Name: "hub", OK: true, Pace: pace})
	for _, want := range []string{
		"llm refused (completions): provider refused the account (HTTP 429, insufficient_quota): no credits",
		"llm refused (embeddings): provider refused the account (HTTP 401, key refused)",
	} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("missing %q:\n%s", want, b.String())
		}
	}
	if strings.Count(b.String(), "the provider account needs the administrator") != 2 {
		t.Fatalf("not one line per refusal:\n%s", b.String())
	}

	b.Reset()
	model{}.renderHubOne(&b, hubLine{Name: "hub", OK: true, Pace: map[string]any{"interval_s": 2.0, "spacing_s": 2.0}})
	if strings.Contains(b.String(), "llm refused") {
		t.Fatalf("a refusal shown without one:\n%s", b.String())
	}
}
