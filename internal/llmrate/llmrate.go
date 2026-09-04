// Package llmrate paces and retries outbound LLM calls, process-wide.
//
// Motivating incident (2026-09-04): a hub's curation sweep fired dozens
// of requests back-to-back through a provider chain whose far end sits
// behind Cloudflare; the burst tripped its bot/rate rules and every
// batch died for the hour — while interactive agents on the same chain,
// one polite request at a time, never saw an error. The cure is
// to make aimem's batch traffic look like everyone else's: space the
// calls (AIMEM_LLM_INTERVAL seconds apart, default 2), retry transient
// blocks with backoff instead of abandoning the batch
// (AIMEM_LLM_RETRIES, default 3), and — when a rate-block IS detected —
// adaptively widen the spacing (penalty doubles per block, capped,
// halves again on each clean success).
package llmrate

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	mu      sync.Mutex
	last    time.Time
	penalty time.Duration // adaptive add-on to the base interval
	blocks  int           // rate-blocks seen (persisted, for stats)
	lastHit string        // RFC3339 of the newest block

	stateDir string // "" = process-local only (tests)

	confOnce sync.Once
	interval time.Duration
	retries  int
)

// SetStateDir wires cross-process persistence: the adaptive penalty and
// block counters live in <dir>/llmrate.json, so a curate run inherits
// the spacing the previous run earned, and health/TUI can display it.
func SetStateDir(dir string) {
	mu.Lock()
	defer mu.Unlock()
	stateDir = dir
	loadLocked()
}

type state struct {
	PenaltyMS int64  `json:"penalty_ms"`
	Blocks    int    `json:"blocks"`
	LastBlock string `json:"last_block,omitempty"`
	Updated   string `json:"updated"`
}

func loadLocked() {
	if stateDir == "" {
		return
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, "llmrate.json"))
	if err != nil {
		return
	}
	var s state
	if json.Unmarshal(raw, &s) != nil {
		return
	}
	penalty, blocks, lastHit = time.Duration(s.PenaltyMS)*time.Millisecond, s.Blocks, s.LastBlock
}

func saveLocked() {
	if stateDir == "" {
		return
	}
	raw, _ := json.Marshal(state{PenaltyMS: penalty.Milliseconds(), Blocks: blocks,
		LastBlock: lastHit, Updated: time.Now().UTC().Format(time.RFC3339)})
	os.WriteFile(filepath.Join(stateDir, "llmrate.json"), raw, 0o600)
}

// Status reports the pacer for stats surfaces (health, TUI). It
// re-reads the persisted state so any process shows what the last
// LLM-calling process experienced.
func Status() map[string]any {
	iv, _ := conf()
	mu.Lock()
	defer mu.Unlock()
	loadLocked()
	out := map[string]any{
		"interval_s": iv.Seconds(),
		"penalty_s":  penalty.Seconds(),
		"spacing_s":  (iv + penalty).Seconds(),
		"blocks":     blocks,
	}
	if lastHit != "" {
		out["last_block"] = lastHit
	}
	return out
}

const (
	defaultInterval = 2 * time.Second
	maxPenalty      = 2 * time.Minute
	penaltyStep     = 5 * time.Second
)

func conf() (time.Duration, int) {
	confOnce.Do(func() {
		interval, retries = defaultInterval, 3
		if v := os.Getenv("AIMEM_LLM_INTERVAL"); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
				interval = time.Duration(f * float64(time.Second))
			}
		}
		if v := os.Getenv("AIMEM_LLM_RETRIES"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				retries = n
			}
		}
	})
	return interval, retries
}

// Retries reports how many retry attempts a blocked call gets.
func Retries() int { _, r := conf(); return r }

// Wait blocks until this process's next call slot: calls are spaced by
// the base interval plus the current adaptive penalty. Serialized under
// the mutex, so concurrent callers queue rather than stampede.
func Wait() {
	iv, _ := conf()
	mu.Lock()
	defer mu.Unlock()
	gap := iv + penalty
	if gap <= 0 {
		last = time.Now()
		return
	}
	if s := time.Until(last.Add(gap)); s > 0 {
		time.Sleep(s)
	}
	last = time.Now()
}

// Penalize widens the spacing after a detected block (doubles, from a
// 5s floor, capped at 2 minutes), persists, and says so — these lines
// land in the hub journal / task output where outages get diagnosed.
func Penalize(reason string) {
	iv, _ := conf()
	mu.Lock()
	defer mu.Unlock()
	if penalty < penaltyStep {
		penalty = penaltyStep
	} else {
		penalty *= 2
	}
	if penalty > maxPenalty {
		penalty = maxPenalty
	}
	blocks++
	lastHit = time.Now().UTC().Format(time.RFC3339)
	saveLocked()
	fmt.Fprintf(os.Stderr, "aimem llmrate: upstream rate-block detected (%s) — call spacing widened to %s (block #%d)\n",
		Clip(strings.TrimSpace(reason), 160), iv+penalty, blocks)
}

// Recover narrows the spacing again after a clean success, announcing
// only the return to baseline (per-success lines would be noise).
func Recover() {
	iv, _ := conf()
	mu.Lock()
	defer mu.Unlock()
	if penalty == 0 {
		return
	}
	penalty /= 2
	if penalty < time.Second {
		penalty = 0
		fmt.Fprintf(os.Stderr, "aimem llmrate: upstream healthy again — call spacing back to base %s\n", iv)
	}
	saveLocked()
}

// RetryDelay is the backoff before retry attempt n (0-based):
// ~15s, 30s, 60s… plus jitter, capped at 2 minutes.
func RetryDelay(attempt int) time.Duration {
	d := 15 * time.Second << attempt
	if d > maxPenalty {
		d = maxPenalty
	}
	return d + time.Duration(rand.Int63n(int64(5*time.Second)))
}

// Blocked classifies a response as a transient upstream block worth
// retrying: rate-limit or server-side status, an HTML body where JSON
// belongs (Cloudflare block pages), or a proxy error message carrying
// one (LiteLLM wraps the upstream's HTML inside its JSON error).
func Blocked(status int, body string) bool {
	if status == 429 || status >= 500 {
		return true
	}
	return BlockedMessage(body)
}

// BlockedMessage detects a block page smuggled inside an error string.
func BlockedMessage(msg string) bool {
	t := strings.TrimSpace(msg)
	if strings.HasPrefix(t, "<") {
		return true
	}
	low := strings.ToLower(t)
	return strings.Contains(low, "<html") || strings.Contains(low, "cloudflare") ||
		strings.Contains(low, "attention required")
}

// Clip bounds provider error text for logs: a Cloudflare block page is
// ~8KB of HTML that once flooded the hub journal line by line.
func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "… (truncated)"
}
