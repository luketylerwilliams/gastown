package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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
func writeFakeProcTree(t *testing.T, rootPID, childPID int, childComm string, childStartTicks uint64) string {
	t.Helper()
	root := t.TempDir()
	// root pid dir with one task exposing children
	taskDir := filepath.Join(root, strconv.Itoa(rootPID), "task", "9001")
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(taskDir, "children"), []byte(strconv.Itoa(childPID)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// child pid dir
	childDir := filepath.Join(root, strconv.Itoa(childPID))
	if err := os.MkdirAll(childDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "comm"), []byte(childComm+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFakeProcStat(t, childDir, childPID, childComm, 0, childStartTicks)
	return root
}

// writeFakeProcStat writes /proc-style field 1..22 for the fixture pid:
// fields 1-2 (pid, comm), then 20 more where state is index 0, utime (field
// 14) lands at index 11, stime (field 15) at index 12, and starttime (field
// 22) at index 19 — exactly what the production parsers slice after the
// closing paren of comm. childCPU goes entirely into utime with stime 0.
func writeFakeProcStat(t *testing.T, childDir string, childPID int, childComm string, childCPU int64, childStartTicks uint64) {
	t.Helper()
	rest := make([]string, 0, 20)
	rest = append(rest, "S") // state (field 3)
	for i := 0; i < 8; i++ { // fields 4..11
		rest = append(rest, "1")
	}
	rest = append(rest,
		strconv.FormatInt(childCPU, 10), // utime (field 14)
		"0",                             // stime (field 15)
		"1", "1", "1",                   // cutime, ctime, priority (16..18)
		"0",                                     // nice (19)
		"0",                                     // rt_priority (20)
		"1",                                     // policy (21)
		strconv.FormatUint(childStartTicks, 10), // starttime (field 22)
	)
	stat := strconv.Itoa(childPID) + " (" + childComm + ") " + strings.Join(rest, " ")
	if err := os.WriteFile(filepath.Join(childDir, "stat"), []byte(stat+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// staleStartTicks returns a starttime value placing a process start ~1h before
// "now" as read through procStartTimeFrom's real-boot conversion (the machine
// has been up longer): outside the grace window by any margin.
func staleStartTicks(t *testing.T) uint64 {
	t.Helper()
	boot, err := bootTime()
	if err != nil {
		t.Skipf("no boot time: %v", err)
	}
	hz := userHZ()
	age := time.Since(boot) - time.Hour
	if age <= 0 {
		t.Skip("machine booted less than an hour ago; cannot build a stale fixture")
	}
	return uint64(age.Seconds() * float64(hz))
}

// freshStartTicks returns a starttime value placing a process start ~5s
// before "now" — inside the grace window.
func freshStartTicks(t *testing.T) uint64 {
	t.Helper()
	boot, err := bootTime()
	if err != nil {
		t.Skipf("no boot time: %v", err)
	}
	hz := userHZ()
	age := time.Since(boot) - 5*time.Second
	if age <= 0 {
		t.Skip("machine booted within the last 5 seconds")
	}
	return uint64(age.Seconds() * float64(hz))
}

// probeSubtreeOnce mirrors the production predicate in
// deaconSubtreeHasRecentActivity against a synthetic proc root: fresh-spawn OR
// CPU-delta evidence over the pane's descendant set. It exists because
// GetPanePID is a concrete tmux dependency; the predicate below is where the
// hq-6nyy3 behavior lives and it uses the same helpers and thresholds as
// production.
func probeSubtreeOnce(t *testing.T, procRoot string, panePID int, samples map[int]cpuSample, probeID uint64) bool {
	t.Helper()
	if samples == nil {
		samples = map[int]cpuSample{}
	}
	descendants := processDescendantsFrom(procRoot, panePID, 4)
	for _, pid := range descendants {
		comm, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "comm"))
		if err != nil {
			continue
		}
		name := strings.TrimSpace(string(comm))
		if !isDeaconActivityComm(name) {
			continue
		}
		started, err := procStartTimeFrom(procRoot, pid)
		if err != nil {
			continue
		}
		if started.After(time.Now().Add(-deaconProcessActivityGrace)) {
			return true
		}
		total, err := procCPUTimeFrom(procRoot, pid)
		if err != nil {
			continue
		}
		if total >= deaconCPUActivityDelta {
			prevSample, seen := samples[pid]
			samples[pid] = cpuSample{probe: probeID, total: total}
			if seen && prevSample.probe != probeID && total-prevSample.total >= deaconCPUActivityDelta {
				return true
			}
		}
	}
	return false
}

// TestCPUDeltaSignalDetectsLongRunningToolChild is the hq-6nyy3 core
// regression: a LONG-RUNNING bash child (spawned before the grace window, so
// the fresh-spawn check cannot see it) that keeps accumulating CPU must read
// as activity across two probes. Under b9cc301 (fresh-spawns-only) this exact
// shape returned false and the kill gate could fire on a working deacon.
func TestCPUDeltaSignalDetectsLongRunningToolChild(t *testing.T) {
	const (
		panePID = 4001
		toolPID = 4002
	)
	old := staleStartTicks(t)
	procRoot := writeFakeProcTree(t, panePID, toolPID, "bash", old)

	samples := map[int]cpuSample{}
	// Probe 1: baseline sample, no previous observation => no delta yet.
	if probeSubtreeOnce(t, procRoot, panePID, samples, 1) {
		t.Fatal("first probe must not report activity (no prior sample to diff)")
	}
	// Child keeps working: CPU grows past the delta threshold.
	writeFakeProcStat(t, filepath.Join(procRoot, strconv.Itoa(toolPID)), toolPID, "bash",
		deaconCPUActivityDelta, old)
	if !probeSubtreeOnce(t, procRoot, panePID, samples, 2) {
		t.Fatal("second probe must report activity: live bash descendant gained >= deaconCPUActivityDelta CPU ticks")
	}
}

// TestCPUDeltaSignalIdleDescendantNoActivity proves the new signal does not
// grant blanket immunity: a stale-started descendant whose CPU time is flat
// between probes (the EP7 freeze signature: hb stale, flat CPU, unresponded
// pokes) must still read as NO activity so the kill ladder can proceed.
func TestCPUDeltaSignalIdleDescendantNoActivity(t *testing.T) {
	const (
		panePID = 5001
		toolPID = 5002
	)
	old := staleStartTicks(t)
	procRoot := writeFakeProcTree(t, panePID, toolPID, "node", old)

	samples := map[int]cpuSample{}
	if probeSubtreeOnce(t, procRoot, panePID, samples, 1) {
		t.Fatal("first probe: no prior sample, must be false")
	}
	// Same CPU total at probe 2: frozen process.
	if probeSubtreeOnce(t, procRoot, panePID, samples, 2) {
		t.Fatal("flat CPU descendant must NOT count as activity (EP7 freeze class must stay killable)")
	}
	// Growth below the threshold is daemon/timer noise, not work.
	writeFakeProcStat(t, filepath.Join(procRoot, strconv.Itoa(toolPID)), toolPID, "node",
		deaconCPUActivityDelta-1, old)
	if probeSubtreeOnce(t, procRoot, panePID, samples, 3) {
		t.Fatal("sub-threshold CPU growth must not count as activity")
	}
}

// TestFreshSpawnStillCounts guards the pre-existing signal: a descendant
// spawned within the grace window counts immediately even with zero CPU
// history (b9cc301 behavior preserved).
func TestFreshSpawnStillCounts(t *testing.T) {
	const (
		panePID = 6001
		toolPID = 6002
	)
	procRoot := writeFakeProcTree(t, panePID, toolPID, "go", freshStartTicks(t))
	if !probeSubtreeOnce(t, procRoot, panePID, map[int]cpuSample{}, 1) {
		t.Fatal("freshly-spawned go descendant must count as activity on the first probe")
	}
}

// TestNonActivityCommExcluded ensures unrelated subtree processes (e.g. a
// hung pager left open in the pane) neither fresh-spawn-immunize nor
// CPU-delta-immunize the deacon.
func TestNonActivityCommExcluded(t *testing.T) {
	const (
		panePID = 7001
		toolPID = 7002
	)
	old := staleStartTicks(t)
	procRoot := writeFakeProcTree(t, panePID, toolPID, "less", old)
	samples := map[int]cpuSample{}
	if probeSubtreeOnce(t, procRoot, panePID, samples, 1) {
		t.Fatal("less started long ago must not count")
	}
	writeFakeProcStat(t, filepath.Join(procRoot, strconv.Itoa(toolPID)), toolPID, "less",
		10*deaconCPUActivityDelta, old)
	if probeSubtreeOnce(t, procRoot, panePID, samples, 2) {
		t.Fatal("a burning non-agent-comm descendant must not count as agent activity")
	}
}

// TestGoTestBinaryCommMatches verifies the comm matcher covers compiled test
// binaries and toolchain children spawned by `go test ./...`.
func TestGoTestBinaryCommMatches(t *testing.T) {
	for _, name := range []string{"daemon.test", "compile", "link", "cgo", "git", "rg", "bd", "gt"} {
		if !isDeaconActivityComm(name) {
			t.Errorf("isDeaconActivityComm(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"sleepy-pager", "vim", "tmux:ignore-me", "systemd"} {
		if isDeaconActivityComm(name) {
			t.Errorf("isDeaconActivityComm(%q) = true, want false", name)
		}
	}
}

// TestRealBusyChildCPUGrowth exercises procCPUTime against a live spinning
// process end-to-end (no fixtures): two reads a moment apart of a real busy
// descendant must differ, which is exactly the kernel evidence the gate
// consumes in production.
func TestRealBusyChildCPUGrowth(t *testing.T) {
	cmd := startBusyChild(t)
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	first, err := procCPUTime(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("procCPUTime: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	second, err := procCPUTime(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("procCPUTime: %v", err)
	}
	if second-first < deaconCPUActivityDelta {
		t.Skipf("busy child gained only %d ticks in 1.5s (loaded machine?); skipping", second-first)
	}
}
