package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// deaconProcessActivityGrace is how recently a descendant opencode process of
// the deacon's tmux pane must have been spawned — or how recently a live
// descendant must have accumulated CPU time — for the pane subtree to count
// as live activity (hq-is5lo, extended by hq-6nyy3). Tool spawns happen every
// few seconds during an active turn; a quiet subtree for this long with a
// stale heartbeat and a static pane is the genuine stuck signature.
const deaconProcessActivityGrace = 5 * time.Minute

// deaconCPUActivityDelta is the minimum utime+stime growth (in clock ticks) a
// live descendant must show between two daemon activity checks to count as
// work in flight (hq-6nyy3). 100 ticks = 1 CPU-second at the usual USER_HZ=100
// — far more than daemon polling overhead, far less than an active
// go test/git/npm compile or a busy node runtime produces.
const deaconCPUActivityDelta int64 = 100

// deaconStartupHBWindow bounds the daemon startup-heartbeat race fix: a
// heartbeat written within this window AFTER the daemon recorded the deacon
// start is treated as post-start even when its timestamp lands a second or two
// BEFORE d.deaconLastStarted (the "Deacon started successfully" log line is
// written after the spawn, so a fast first heartbeat can be counted as
// pre-restart — the hq-o31s3 startup-hb race that armed the 4-tick kill rule
// against an ACTIVE deacon at the 21:41:54Z reap).
const deaconStartupHBWindow = 30 * time.Second

// deaconHeartbeatCountsAsPostStart reports whether hb should be considered a
// heartbeat from the CURRENT deacon instance rather than a leftover from
// before d.deaconLastStarted. It implements the startup-race tolerance:
// timestamps within deaconStartupHBWindow before the recorded start count as
// post-start.
func deaconHeartbeatCountsAsPostStart(hbTime, started time.Time) bool {
	if !hbTime.Before(started) {
		return true
	}
	return started.Sub(hbTime) <= deaconStartupHBWindow
}

// deaconSubtreeHasRecentActivity reports whether the tmux pane's process
// subtree contains an opencode process spawned within deaconProcessActivityGrace.
// The daemon cannot see inside the opencode server (its run-id logs are not
// attributable per session), but the TUI hierarchy under the pane is direct
// evidence of liveness:
//
//	tmux shell -> opencode (TUI) -> opencode serve --stdio -> /bin/bash -c ...
//
// A freshly-spawned `opencode serve` child means the current instance is
// working; a fresh bash grandchild means a tool is executing right now. This
// is the activity-based busy signal for the kill gate (hq-is5lo FIX 2): while
// it holds, ticks must NOT count toward the kill no matter what the pane hash
// or heartbeat age say.
//
// Fails closed (returns false) on any error: an unreadable process table looks
// like the old behavior (pane-change alone defers), never like immunity.
func (d *Daemon) deaconSubtreeHasRecentActivity(sessionName string) bool {
	if d.tmux == nil {
		return false
	}
	panePIDStr, err := d.tmux.GetPanePID(sessionName)
	if err != nil {
		d.logger.Printf("Activity check: pane_pid lookup failed for %s: %v (treating as no activity)", sessionName, err)
		return false
	}
	panePID, err := strconv.Atoi(strings.TrimSpace(panePIDStr))
	if err != nil || panePID <= 0 {
		return false
	}

	cutoff := time.Now().Add(-deaconProcessActivityGrace)
	descendants := processDescendants(panePID, 4)
	// hq-6nyy3: a monotonic probe sequence id lets procCPUTimeSamples dedupe
	// per-pid samples when a pid legitimately appears multiple times in the
	// subtree walk (multiple task/*/children entries listing the same child).
	probeID := atomic.AddUint64(&d.deaconActivityProbe, 1)
	for _, pid := range descendants {
		cmd, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(cmd))
		if !isDeaconActivityComm(name) {
			continue
		}
		started, err := procStartTime(pid)
		if err != nil {
			continue
		}
		if started.After(cutoff) {
			return true
		}
		// hq-6nyy3 blind-spot fix (a): long-running tool children (bash -c,
		// go test, git, npm, python...) spawned BEFORE the grace window are
		// invisible to the fresh-spawn check above. A deacon mid a 10-minute
		// `go test ./...` has a stale opencode ancestor and excluded bash
		// grandchildren — the gate read false while the deacon was working.
		// Live CPU-time accumulation closes this: compare the descendant's
		// utime+stime against the sample from the previous check; growth of
		// at least deaconCPUActivityDelta ticks means it is burning CPU NOW.
		total, err := procCPUTime(pid)
		if err != nil {
			continue
		}
		if total >= deaconCPUActivityDelta {
			if d.deaconActivityCPU == nil {
				d.deaconActivityCPU = map[int]cpuSample{}
			}
			d.deaconActivityMu.Lock()
			prev, seen := d.deaconActivityCPU[pid]
			d.deaconActivityCPU[pid] = cpuSample{probe: probeID, total: total}
			d.deaconActivityMu.Unlock()
			if !seen {
				// First sample for this pid: if it already has significant CPU
				// (the child was burning before we started watching), treat it
				// as activity — hq-6nyy3: long-running tools that accumulated
				// CPU before the daemon's first probe must not be invisible.
				if total >= deaconCPUActivityDelta {
					d.logger.Printf("Activity check: pid %d (%s) baseline %d CPU ticks — work in flight",
						pid, name, total)
					return true
				}
			} else if prev.probe != probeID && total-prev.total >= deaconCPUActivityDelta {
				d.logger.Printf("Activity check: pid %d (%s) gained %d CPU ticks since last check — work in flight",
					pid, name, total-prev.total)
				return true
			}
		}
	}
	return false
}

// isDeaconActivityComm reports whether a process command name is part of the
// deacon's expected execution subtree (agent runtimes and the tool processes
// they spawn, hq-6nyy3). Matches /proc/<pid>/comm, which is truncated to 15
// characters, so prefix forms like "opencode serv" or "node:999999" variants
// are tolerated where safe.
func isDeaconActivityComm(name string) bool {
	switch name {
	case "opencode", "claude", "node", "bash", "sh", "git", "go", "npm", "npx",
		"python", "python3", "rg", "grep", "jq", "gt", "bd", "tmux", "sleep":
		return true
	}
	// Compiled Go test binaries are named <pkg>.test; toolchains spawn
	// compile/link children (compile, link, cgo).
	if strings.HasSuffix(name, ".test") || name == "compile" || name == "link" || name == "cgo" {
		return true
	}
	return false
}

// cpuSample records the cumulative CPU time of a pid as observed by one
// activity probe (a single deaconSubtreeHasRecentActivity call).
type cpuSample struct {
	probe uint64
	total int64
}

// processDescendants returns pids reachable from root within maxDepth levels
// using /proc/<pid>/task/*/children. Best-effort: unreadable entries are
// skipped, and cycles cannot occur in a process tree.
func processDescendants(root int, maxDepth int) []int {
	return processDescendantsFrom("/proc", root, maxDepth)
}

// processDescendantsFrom is the proc-root-parameterized core of
// processDescendants (hq-6nyy3): tests materialize a synthetic /proc layout in
// a temp dir to exercise the activity gate deterministically.
func processDescendantsFrom(procRoot string, root int, maxDepth int) []int {
	result := []int{root}
	frontier := []int{root}
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var next []int
		for _, pid := range frontier {
			children, err := os.ReadDir(filepath.Join(procRoot, strconv.Itoa(pid), "task"))
			if err != nil {
				continue
			}
			for _, tid := range children {
				data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "task", tid.Name(), "children"))
				if err != nil {
					continue
				}
				for _, field := range strings.Fields(string(data)) {
					child, err := strconv.Atoi(field)
					if err != nil || child <= 0 {
						continue
					}
					next = append(next, child)
					result = append(result, child)
				}
			}
		}
		frontier = next
	}
	return result
}

// procStartTime returns the process start time from /proc/<pid>/stat field 22
// (starttime, clock ticks since boot). Converted with boot time + USER_HZ.
func procStartTime(pid int) (time.Time, error) {
	return procStartTimeFrom("/proc", pid)
}

// procStartTimeFrom is the proc-root-parameterized core of procStartTime
// (hq-6nyy3 test seam). Boot time is only consulted for the real /proc; a
// synthetic tree uses relative tick arithmetic against an epoch-zero boot.
func procStartTimeFrom(procRoot string, pid int) (time.Time, error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return time.Time{}, err
	}
	// Field 2 (comm) may contain spaces/parens; skip past the closing paren.
	stat := string(data)
	idx := strings.LastIndex(stat, ")")
	if idx < 0 || idx+2 >= len(stat) {
		return time.Time{}, fmt.Errorf("malformed stat for pid %d", pid)
	}
	fields := strings.Fields(stat[idx+2:])
	// starttime is overall field 22; after fields 1-2 there are 20 remaining
	// fields (state at index 0 ... starttime at index 19), so a valid stat has
	// exactly len(fields) == 20.
	const numFieldsAfterComm = 20
	if len(fields) < numFieldsAfterComm {
		return time.Time{}, fmt.Errorf("short stat for pid %d", pid)
	}
	ticks, err := strconv.ParseUint(fields[numFieldsAfterComm-1], 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	boot, err := bootTime()
	if err != nil {
		return time.Time{}, err
	}
	hz := userHZ()
	age := time.Duration(ticks) * time.Second / time.Duration(hz)
	return boot.Add(age), nil
}

// procCPUTime returns a process's cumulative CPU time in clock ticks —
// utime+stime from /proc/<pid>/stat fields 14 and 15. Used by the hq-6nyy3
// CPU-delta activity signal: two samples of a live pid taken a daemon tick
// apart whose sum grew show work in flight even when nothing was spawned
// recently.
func procCPUTime(pid int) (total int64, err error) {
	return procCPUTimeFrom("/proc", pid)
}

// procCPUTimeFrom is the proc-root-parameterized core of procCPUTime
// (hq-6nyy3 test seam).
func procCPUTimeFrom(procRoot string, pid int) (total int64, err error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	stat := string(data)
	idx := strings.LastIndex(stat, ")")
	if idx < 0 || idx+2 >= len(stat) {
		return 0, fmt.Errorf("malformed stat for pid %d", pid)
	}
	fields := strings.Fields(stat[idx+2:])
	// Field 2 (comm) is consumed up to the closing paren, so overall field 14
	// (utime) is index 11 and field 15 (stime) is index 12 here.
	const utimeIdx, stimeIdx = 11, 12
	if len(fields) <= stimeIdx {
		return 0, fmt.Errorf("short stat for pid %d", pid)
	}
	ut, err := strconv.ParseInt(fields[utimeIdx], 10, 64)
	if err != nil {
		return 0, err
	}
	st, err := strconv.ParseInt(fields[stimeIdx], 10, 64)
	if err != nil {
		return 0, err
	}
	return ut + st, nil
}

// bootTime reads btime from /proc/stat.
func bootTime() (time.Time, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "btime ") {
			secs, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime")), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(secs, 0), nil
		}
	}
	return time.Time{}, fmt.Errorf("btime not found in /proc/stat")
}

var cachedUserHZ int64

// userHZ returns the clock ticks per second (sysconf(_SC_CLK_TCK)) via getconf,
// cached; falls back to the near-universal 100.
func userHZ() int64 {
	if cachedUserHZ > 0 {
		return cachedUserHZ
	}
	v := int64(0)
	if out, err := exec.Command("getconf", "CLK_TCK").Output(); err == nil {
		v, _ = strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	}
	if v <= 0 {
		v = 100
	}
	cachedUserHZ = v
	return v
}
