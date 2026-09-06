package llmrate

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMain pins a tiny base interval BEFORE conf()'s sync.Once latches,
// so the Wait tests measure their own gaps, not the 2s default.
func TestMain(m *testing.M) {
	os.Setenv("AIMEM_LLM_INTERVAL", "0.05")
	os.Exit(m.Run())
}

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

// resetPacing puts the pacer into a known state for a Wait test.
func resetPacing(t *testing.T, pen time.Duration) {
	t.Helper()
	mu.Lock()
	last = time.Time{}
	penalty = pen
	penGen = 0
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		last, penalty, penGen = time.Time{}, 0, 0
		mu.Unlock()
	})
}

// TestWaitDoesNotHoldMutexWhileSleeping: the bug the reservation model
// fixes — a sleeping waiter must not block other llmrate users
// (Status, Penalize, another Wait's reservation).
func TestWaitDoesNotHoldMutexWhileSleeping(t *testing.T) {
	resetPacing(t, 400*time.Millisecond)
	go Wait() // first call fires immediately, reserving t0
	go Wait() // second reserves t0+gap and sleeps ~400ms
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	Status() // locks mu internally
	if e := time.Since(start); e > 150*time.Millisecond {
		t.Fatalf("Status blocked %s behind a sleeping Wait", e)
	}
	time.Sleep(500 * time.Millisecond) // let the waiters drain before cleanup
}

// TestWaitSpacingConcurrent: N concurrent callers must return spaced
// by at least the gap, in some order — the pacing contract.
func TestWaitSpacingConcurrent(t *testing.T) {
	const gap = 120 * time.Millisecond
	resetPacing(t, gap) // interval defaults may add more; only a lower bound is asserted
	done := make(chan time.Time, 3)
	for range 3 {
		go func() { Wait(); done <- time.Now() }()
	}
	var times []time.Time
	for range 3 {
		times = append(times, <-done)
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	for i := 1; i < len(times); i++ {
		if d := times[i].Sub(times[i-1]); d < gap-30*time.Millisecond {
			t.Fatalf("returns %d and %d only %s apart (gap %s)", i-1, i, d, gap)
		}
	}
}

// TestPenalizeWidensQueuedWaiters: a Penalize while waiters sleep must
// widen THEIR remaining spacing, not just future reservations —
// otherwise an in-flight burst keeps hammering a blocked upstream at
// the old cadence (max-review finding on PR #17).
func TestPenalizeWidensQueuedWaiters(t *testing.T) {
	resetPacing(t, 100*time.Millisecond)
	start := time.Now()
	done := make(chan time.Duration, 2)
	for range 2 {
		go func() { Wait(); done <- time.Since(start) }()
	}
	time.Sleep(30 * time.Millisecond) // let them reserve their slots
	Penalize("test block")            // penalty jumps to the 5s floor
	var returns []time.Duration
	for range 2 {
		returns = append(returns, <-done)
	}
	slices.Sort(returns)
	// The first caller fired before the block. The second must have
	// been re-queued to the widened gap (>=5s floor) instead of keeping
	// its original ~100ms slot.
	if returns[1] < 1*time.Second {
		t.Fatalf("queued waiter returned at %s — old-cadence slot survived a Penalize", returns[1])
	}
}
