package enrollment

import (
	"sync"
	"time"
)

// Redemption bounds (enrollment.v1, Bounds): the subcode's 256 bits make
// guessing infeasible, so these limits bound load, not guessing.
const (
	RedemptionsPerAddress = 10
	RedemptionsPerBundle  = 20
	RateWindow            = time.Minute
)

// Limiter counts events per key in a sliding window. It is in memory: a hub
// restart forgets it, which costs at most one more window of requests.
type Limiter struct {
	limit  int
	window time.Duration
	mu     sync.Mutex
	seen   map[string][]time.Time
	// swept is when keys with nothing left in the window were last
	// forgotten; the sweep runs at most once per window, so its cost is
	// amortized over every event of that window.
	swept time.Time
}

// NewLimiter allows limit events per key in each window.
func NewLimiter(limit int, window time.Duration) *Limiter {
	return &Limiter{limit: limit, window: window, seen: map[string][]time.Time{}}
}

// Allow records an event for key at now and reports whether it is within the
// limit. A refused event is not recorded, so a caller that waits recovers.
func (l *Limiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-l.window)
	kept := l.seen[key][:0]
	for _, t := range l.seen[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.limit {
		l.seen[key] = kept
		return false
	}
	l.seen[key] = append(kept, now)
	// Forget keys with nothing left in the window, at most once per window,
	// so the map stays bounded by the keys active in the last two windows
	// and each event costs amortized constant work.
	if now.Sub(l.swept) >= l.window {
		for k, ts := range l.seen {
			if len(ts) == 0 || !ts[len(ts)-1].After(cut) {
				delete(l.seen, k)
			}
		}
		l.swept = now
	}
	return true
}
