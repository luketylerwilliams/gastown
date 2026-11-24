package daemon

import (
	"io"
	"log"
	"os"
	"testing"
	"time"
)

// testLogger returns a logger that discards output, for helpers that log on
// every path.
func testLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestDeaconHeartbeatCountsAsPostStart(t *testing.T) {
	started := time.Now()

	tests := []struct {
		name string
		hb   time.Time
		want bool
	}{
		{"after start", started.Add(time.Minute), true},
		{"exactly at start", started, true},
		// The hq-o31s3 race: c1565 was written 2s BEFORE the "Deacon started"
		// log line and was counted pre-restart, arming the 4-tick kill rule.
		{"race: 2s before start", started.Add(-2 * time.Second), true},
		{"within window: 15s before start", started.Add(-15 * time.Second), true},
		{"boundary: exactly window before start", started.Add(-deaconStartupHBWindow), true},
		{"genuinely pre-restart leftover", started.Add(-deaconStartupHBWindow - time.Second), false},
		{"old heartbeat from prior instance", started.Add(-time.Hour), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deaconHeartbeatCountsAsPostStart(tt.hb, started); got != tt.want {
				t.Errorf("deaconHeartbeatCountsAsPostStart(%v, start) = %v, want %v",
					tt.hb.Sub(started).Round(time.Second), got, tt.want)
			}
		})
	}
}

func TestProcessDescendantsSelf(t *testing.T) {
	// /proc-based; skip where unavailable (non-Linux CI).
	if _, err := os.ReadFile("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	got := processDescendants(os.Getpid(), 4)
	found := false
	for _, pid := range got {
		if pid == os.Getpid() {
			found = true
		}
		if pid <= 0 {
			t.Errorf("processDescendants returned non-positive pid %d", pid)
		}
	}
	if !found {
		t.Errorf("processDescendants(self) did not include self: %v", got)
	}
	// A leaf-ish depth limit must never error or loop forever; reaching here
	// with >=1 result is the contract.
	if len(got) < 1 {
		t.Fatalf("expected at least the root pid")
	}
}

func TestProcStartTimeSelfSane(t *testing.T) {
	if _, err := os.ReadFile("/proc/self/stat"); err != nil {
		t.Skip("no /proc")
	}
	start, err := procStartTime(os.Getpid())
	if err != nil {
		t.Fatalf("procStartTime(self): %v", err)
	}
	// The test process started seconds ago — well after boot, not in the future.
	if start.After(time.Now()) {
		t.Errorf("procStartTime(self) = %v, in the future", start)
	}
	if time.Since(start) > 12*time.Hour {
		t.Errorf("procStartTime(self) = %v, implausibly old (>12h) for a just-spawned test process", start)
	}
}

func TestBootTimeSane(t *testing.T) {
	if _, err := os.ReadFile("/proc/stat"); err != nil {
		t.Skip("no /proc")
	}
	bt, err := bootTime()
	if err != nil {
		t.Fatalf("bootTime(): %v", err)
	}
	if bt.After(time.Now()) {
		t.Errorf("bootTime() = %v, in the future", bt)
	}
	// Host uptime is tracked operationally; any boot older than a year here
	// means btime parsing is wrong, not that the machine is ancient.
	if time.Since(bt) > 365*24*time.Hour {
		t.Errorf("bootTime() = %v, implausibly old", bt)
	}
}

func TestDeaconSubtreeHasRecentActivityNoTmux(t *testing.T) {
	d := &Daemon{logger: testLogger()}
	// tmux nil must fail closed (false), never panic, never grant immunity.
	if d.deaconSubtreeHasRecentActivity("hq-deacon") {
		t.Error("nil tmux must report no activity (fail closed)")
	}
}

func TestProcStartTimeBadPid(t *testing.T) {
	if _, err := os.ReadFile("/proc/1/stat"); err != nil {
		t.Skip("no /proc")
	}
	if _, err := procStartTime(999999999); err == nil {
		t.Error("expected error for nonexistent pid")
	}
}
