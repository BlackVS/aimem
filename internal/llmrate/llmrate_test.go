package llmrate

import (
	"strings"
	"testing"
	"time"
)

func TestBlockedClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{200, `{"data":[]}`, false},
		{429, `{}`, true},
		{503, `whatever`, true},
		{200, `<!DOCTYPE html><html>blocked</html>`, true},
		{400, `{"error":{"message":"bad request"}}`, false},
	}
	for _, c := range cases {
		if got := Blocked(c.status, c.body); got != c.want {
			t.Errorf("Blocked(%d, %.20q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
	if !BlockedMessage("upstream said: <html><title>Attention Required! | Cloudflare</title>...") {
		t.Error("HTML smuggled in an error message must classify as a block")
	}
	if BlockedMessage("connection refused") {
		t.Error("ordinary errors are not blocks")
	}
}

func TestPenaltyAdaptsAndPersists(t *testing.T) {
	dir := t.TempDir()
	SetStateDir(dir)
	t.Cleanup(func() { SetStateDir("") })

	mu.Lock()
	penalty, blocks, lastHit = 0, 0, ""
	mu.Unlock()

	Penalize("test block")
	Penalize("test block")
	mu.Lock()
	p, b := penalty, blocks
	mu.Unlock()
	if p != 10*time.Second || b != 2 {
		t.Fatalf("after two blocks: penalty=%v blocks=%d; want 10s/2", p, b)
	}

	// A fresh "process" (state reset + reload) inherits the penalty.
	mu.Lock()
	penalty, blocks = 0, 0
	mu.Unlock()
	SetStateDir(dir)
	st := Status()
	if st["penalty_s"].(float64) != 10 || st["blocks"].(int) != 2 {
		t.Fatalf("persisted state not reloaded: %+v", st)
	}

	// Successes decay it back to zero.
	for range 8 {
		Recover()
	}
	if s := Status(); s["penalty_s"].(float64) != 0 {
		t.Fatalf("penalty should decay to 0, got %+v", s)
	}
}

func TestClip(t *testing.T) {
	if got := Clip(strings.Repeat("x", 500), 100); len(got) > 120 || !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("clip: %q", got)
	}
}
