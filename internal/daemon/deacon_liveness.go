package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// deaconProcessActivityGrace is how recently a descendant opencode process of
// the deacon's tmux pane must have been spawned for the pane subtree to count
// as live activity (hq-is5lo). Tool spawns happen every few seconds during an
// active turn; a quiet subtree for this long with a stale heartbeat and a
// static pane is the genuine stuck signature.
const deaconProcessActivityGrace = 5 * time.Minute

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
	for _, pid := range descendants {
		cmd, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(cmd))
		if name != "opencode" && name != "claude" && name != "node" {
			continue
		}
		started, err := procStartTime(pid)
		if err != nil {
			continue
		}
		if started.After(cutoff) {
			return true
		}
	}
	return false
}

// processDescendants returns pids reachable from root within maxDepth levels
// using /proc/<pid>/task/*/children. Best-effort: unreadable entries are
// skipped, and cycles cannot occur in a process tree.
func processDescendants(root int, maxDepth int) []int {
	result := []int{root}
	frontier := []int{root}
	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		var next []int
		for _, pid := range frontier {
			children, err := os.ReadDir(fmt.Sprintf("/proc/%d/task", pid))
			if err != nil {
				continue
			}
			for _, tid := range children {
				data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%s/children", pid, tid.Name()))
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
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
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
	// starttime is field 22 overall; after removing fields 1-2 it is index 19.
	const starttimeIdx = 19
	if len(fields) <= starttimeIdx {
		return time.Time{}, fmt.Errorf("short stat for pid %d", pid)
	}
	ticks, err := strconv.ParseUint(fields[starttimeIdx], 10, 64)
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
