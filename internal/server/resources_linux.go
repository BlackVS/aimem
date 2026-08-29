//go:build linux

package server

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// cpuPrev holds the previous /proc/stat sample so cpu_used_pct reflects
// real CPU time consumed since the last health poll — the same measure
// top reports, unlike the load average (which also counts I/O waiters
// and momentary run-queue blips).
var cpuPrev struct {
	mu          sync.Mutex
	idle, total uint64
}

func readCPUSample() (idle, total uint64, ok bool) {
	raw, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	f := strings.Fields(line)
	if len(f) < 5 || f[0] != "cpu" {
		return 0, 0, false
	}
	for i, v := range f[1:] {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total += n
		if i == 3 || i == 4 { // idle + iowait
			idle += n
		}
	}
	return idle, total, true
}

func cpuUsedPct() (int, bool) {
	idle, total, ok := readCPUSample()
	if !ok {
		return 0, false
	}
	cpuPrev.mu.Lock()
	prevIdle, prevTotal := cpuPrev.idle, cpuPrev.total
	cpuPrev.idle, cpuPrev.total = idle, total
	cpuPrev.mu.Unlock()
	if prevTotal == 0 || total == prevTotal {
		// First poll since process start (or a same-tick repeat): there is
		// no interval to measure, which used to surface as a missing
		// metric ("?%" in the GUI) until the next poll. Take a short
		// second sample instead so every caller gets a real figure.
		time.Sleep(200 * time.Millisecond)
		idle2, total2, ok := readCPUSample()
		if !ok || total2 == total {
			return 0, false
		}
		cpuPrev.mu.Lock()
		cpuPrev.idle, cpuPrev.total = idle2, total2
		cpuPrev.mu.Unlock()
		return int(100 - 100*(idle2-idle)/(total2-total)), true
	}
	return int(100 - 100*(idle-prevIdle)/(total-prevTotal)), true
}

// resources reports process and system memory plus state-root disk usage —
// the numbers an operator needs to see an OOM or full disk coming.
func resources(stateRoot string) map[string]any {
	out := map[string]any{}
	if kb := procStatusKB("/proc/self/status", "VmRSS:"); kb > 0 {
		out["proc_rss_mb"] = kb / 1024
	}
	total := meminfoKB("MemTotal:")
	avail := meminfoKB("MemAvailable:")
	if total > 0 {
		out["mem_total_mb"] = total / 1024
		out["mem_available_mb"] = avail / 1024
		out["mem_used_pct"] = int(100 - 100*avail/total)
	}
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		f := strings.Fields(string(raw))
		if len(f) >= 3 {
			out["load_1m"], out["load_5m"], out["load_15m"] = f[0], f[1], f[2]
		}
	}
	out["cpus"] = runtime.NumCPU()
	if pct, ok := cpuUsedPct(); ok {
		out["cpu_used_pct"] = pct
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs(stateRoot, &fs); err == nil && fs.Blocks > 0 {
		totalB := fs.Blocks * uint64(fs.Bsize)
		freeB := fs.Bavail * uint64(fs.Bsize)
		out["disk_total_mb"] = totalB / (1 << 20)
		out["disk_free_mb"] = freeB / (1 << 20)
		out["disk_used_pct"] = int(100 - 100*freeB/totalB)
	}
	return out
}

func procStatusKB(path, key string) int64 {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, key) {
			f := strings.Fields(line)
			if len(f) >= 2 {
				n, _ := strconv.ParseInt(f[1], 10, 64)
				return n
			}
		}
	}
	return 0
}

func meminfoKB(key string) int64 { return procStatusKB("/proc/meminfo", key) }
