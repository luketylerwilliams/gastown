package daemon

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// startBusyChild spawns a process under the test binary that burns CPU
// continuously, waits until it has demonstrably accumulated CPU, and returns
// it. The caller must Kill/Wait it.
func startBusyChild(t *testing.T) *exec.Cmd {
	t.Helper()
	// sh -c 'i=0; while :; do i=$((i+1)); done' spins in a tight loop and
	// accumulates utime immediately.
	cmd := exec.Command("sh", "-c", "i=0; while :; do i=$((i+1)); done")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn busy child: %v", err)
	}
	// Wait until the kernel has recorded >= 1 full tick of CPU so the very
	// first probe already sees total >= deaconCPUActivityDelta or the second
	// one does within a short window.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if total, err := procCPUTime(cmd.Process.Pid); err == nil && total >= 2 {
			return cmd
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	t.Skip("busy child never accumulated CPU (unexpected)")
	return nil
}

// writeFakeProcTree materializes a synthetic /proc layout: a root dir named
// like a pid containing task/<tid>/children files pointing at child pid dirs,
// each child dir containing comm and stat files. Returns the fake proc root.
// stat is built so fields 14/15 (utime/stime) equal the given totals.
func writeFakeProcTree(t *testing.T, rootPID, childPID int, childComm string, childCPU int64, childStartTicks uint64) string {
	t.Helper()
	root := t.TempDir()
	// root pid dir with one task exposing children
	taskDir := filepathJoin(t, root, itoa(rootPID), "task", "9001")
	mkdirAll(t, taskDir)
	writeFile(t, filepathJoin(t, taskDir, "children"), itoa(childPID)+"\n")
	// child pid dir
	childDir := filepathJoin(t, root, itoa(childPID))
	mkdirAll(t, childDir)
	writeFile(t, filepathJoin(t, childDir, "comm"), childComm+"\n")
	// stat: "pid (comm) S ppid pgrp session tty_nr tpgid flags" = fields 1..11,
	// then utime(14) stime(15). After ") " the fields start at overall field 3,
	// so utime is index 11 and stime index 12.
	stat := itoa(childPID) + " (" + childComm + ") S 1 1 1 0 0 0 0 0 0 0 0 " +
		itoa(childCPU) + " 0 " + itoa(childStartTicks) + " 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	writeFile(t, filepathJoin(t, childDir, "stat"), stat+"\n")
	return root
}

// runProbeWithFakeProc evaluates the pure predicate used by
// deaconSubtreeHasRecentActivity's CPU-delta branch against a synthetic proc
// tree, mirroring the production loop exactly (same helpers, same thresholds).
// It exists because GetPanePID is a concrete-method dependency on the tmux
// binary; the predicate is where the hq-6nyy3 behavior lives.
func runProbeWithFakeProc(t *testing.T, procRoot string, panePID, childPID int, prev map[int]cpuSample, probeID uint64) (bool, map[int]cpuSample) {
	t.Helper()
	samples := prev
	if samples == nil {
		samples = map[int]cpuSample{}
	}
	descendants := processDescendantsFrom(procRoot, panePID, 4)
	cutoff := time.Now().Add(-deaconProcessActivityGrace)
	for _, pid := range descendants {
		comm, err := os.ReadFile(filepathJoin(t, procRoot, itoa(pid), "comm"))
		if err != nil {
			continue
		}
		name := trimSpace(string(comm))
		if !isDeaconActivityComm(name) {
			continue
		}
		started, err := procStartTimeFrom(procRoot, pid)
		if err != nil {
			continue
		}
		if started.After(cutoff) {
			return true, samples
		}
		total, err := procCPUTimeFrom(procRoot, pid)
		if err != nil {
			continue
		}
		if total >= deaconCPUActivityDelta {
			prevSample, seen := samples[pid]
			samples[pid] = cpuSample{probe: probeID, total: total}
			if seen && prevSample.probe != probeID && total-prevSample.total >= deaconCPUActivityDelta {
				return true, samples
			}
		}
	}
	return false, samples
}
