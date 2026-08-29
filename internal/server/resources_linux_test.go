//go:build linux

package server

import "testing"

// The very first poll used to return ok=false (no prior sample), which
// the GUI rendered as "?%". It must now self-sample and report a value.
func TestCPUUsedPctFirstCall(t *testing.T) {
	cpuPrev.mu.Lock()
	cpuPrev.idle, cpuPrev.total = 0, 0
	cpuPrev.mu.Unlock()
	pct, ok := cpuUsedPct()
	if !ok || pct < 0 || pct > 100 {
		t.Fatalf("first call: pct=%d ok=%v", pct, ok)
	}
	// Second call measures against the stored sample — still a value.
	if pct, ok = cpuUsedPct(); !ok || pct < 0 || pct > 100 {
		t.Fatalf("second call: pct=%d ok=%v", pct, ok)
	}
}
