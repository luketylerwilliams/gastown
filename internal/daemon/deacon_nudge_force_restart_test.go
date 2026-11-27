package daemon

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/deacon"
)

// writeFakeTmuxSimulatesPane creates a fake tmux binary that simulates a
// deacon pane. When frozen, every capture-pane returns identical static
// content: send-keys "succeeds" at the TTY level but the agent never
// processes the input, so NudgeSession verification fails with "Enter not
// processed after 3 retries" — the hq-c4b3w Class-B stall signature (hung
// model stream, zero CPU, static pane). When live, capture-pane content
// changes on every call, so Enter is "processed" and the nudge succeeds.
//
// Session existence is STATEFUL via the TMUX_STATE_FILE env var (the test
// sets it and pre-creates the file): has-session reports it, kill-session
// removes it, new-session recreates it. Statefulness is load-bearing — after
// restartStuckDeacon kills the session, Manager.Start must observe
// has-session=false and take the clean-respawn path (exactly one
// new-session). A stateless "always exists" fake makes Manager.Start return
// ErrAlreadyRunning with no respawn.
func writeFakeTmuxSimulatesPane(t *testing.T, dir string, frozen bool) {
	t.Helper()
	frozenFlag := "0"
	if frozen {
		frozenFlag = "1"
	}
	script := `#!/usr/bin/env bash
set -euo pipefail

cmd=""
skip_next=0
for arg in "$@"; do
  if [[ "$skip_next" -eq 1 ]]; then
    skip_next=0
    continue
  fi
  # -u is a standalone flag; -L takes a value (the socket name). The Tmux
  # wrapper builds "tmux -u [-L <socket>] <cmd> ..." (tmux.go commandContext),
  # so both prefix shapes must be skipped before the real command name.
  if [[ "$arg" == "-u" ]]; then
    continue
  fi
  if [[ "$arg" == "-L" ]]; then
    skip_next=1
    continue
  fi
  cmd="$arg"
  break
done

if [[ -n "${TMUX_LOG:-}" ]]; then
  printf "%s\n" "$cmd" >> "$TMUX_LOG"
fi

case "$cmd" in
  has-session)
    # Stateful: see TMUX_STATE_FILE (test pre-creates it; kill-session
    # removes, new-session recreates).
    if [[ -f "${TMUX_STATE_FILE:-/dev/null}" ]]; then
      exit 0
    fi
    exit 1
    ;;
  kill-session)
    rm -f "${TMUX_STATE_FILE:-/dev/null}"
    exit 0
    ;;
  new-session)
    : > "${TMUX_STATE_FILE:-/dev/null}"
    exit 0
    ;;
  capture-pane)
    if [[ "` + frozenFlag + `" == "1" ]]; then
      # Frozen pane: identical content on every capture (static, hung).
      printf 'FROZEN PANE LINE 1\nFROZEN PANE LINE 2\n$ \n'
    else
      # Live pane: content changes on every capture (progress). COUNTER_FILE
      # is set by the test (t.Setenv).
      n=$(cat "$COUNTER_FILE" 2>/dev/null || echo 0)
      n=$((n+1))
      echo "$n" > "$COUNTER_FILE"
      printf 'LIVE PANE TICK %s\n$ \n' "$n"
    fi
    exit 0
    ;;
  display-message)
    # pane_pid=0 (no process — activity check reads "no activity"),
    # pane_in_mode=0, session_attached=0 — harmless for all queries.
    echo "0"
    exit 0
    ;;
  list-panes)
    printf '%%99 opencode 12345 0\n'
    exit 0
    ;;
  show-environment)
    # No GT_PANE_ID / GT_AGENT declared.
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
}

// TestCheckDeaconHeartbeat_NudgeFailForceRestart verifies the hq-c4b3w
// behavior: when consecutive NudgeSession calls fail (frozen pane — plain
// Enter cannot unfreeze it), the daemon counts the failures and
// force-restarts the session (kill + respawn) once
// ConsecutiveNudgeFailuresV() is reached, instead of nudging a dead pane
// until the very-stale kill threshold. Pre-fix, the 2026-11-27 stall
// episodes self-recovered only after 7-16m of blind monitoring.
func TestCheckDeaconHeartbeat_NudgeFailForceRestart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}

	tests := []struct {
		name              string
		operationalConfig string
		frozenCalls       int
		wantRestart       bool
		desc              string
	}{
		{
			name:        "below limit: 2 consecutive nudge failures — no restart yet",
			frozenCalls: 2,
			wantRestart: false,
			desc:        "Default limit is 3; below it the daemon must log the budget and keep nudging",
		},
		{
			name:        "at limit: 3 consecutive nudge failures — force-restart",
			frozenCalls: 3,
			wantRestart: true,
			desc:        "3 consecutive failed nudges must trigger kill + respawn",
		},
		{
			name:              "configured limit 2: 2 consecutive failures — force-restart",
			operationalConfig: `{"operational":{"deacon":{"consecutive_nudge_failures":2}}}`,
			frozenCalls:       2,
			wantRestart:       true,
			desc:              "consecutive_nudge_failures config must drive the limit (2 → restart on 2nd failure)",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			townRoot := t.TempDir()
			fakeBinDir := t.TempDir()
			tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
			if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
				t.Fatalf("create tmux log: %v", err)
			}

			writeFakeTmuxSimulatesPane(t, fakeBinDir, true)
			t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TMUX_LOG", tmuxLog)
			// Stateful fake session: the deacon exists (and is frozen) at
			// test start; kill-session/new-session in the fake track it.
			stateFile := filepath.Join(fakeBinDir, "deacon-session-state")
			t.Setenv("TMUX_STATE_FILE", stateFile)
			if err := os.WriteFile(stateFile, []byte("1"), 0o644); err != nil {
				t.Fatalf("create session state: %v", err)
			}
			// 10m: inside the nudge tier (stale 5m < 10m < very-stale 30m).
			writeDeaconHeartbeat(t, townRoot, 10*time.Minute)

			if tc.operationalConfig != "" {
				if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
					t.Fatalf("mkdir settings: %v", err)
				}
				if err := os.WriteFile(filepath.Join(townRoot, "settings", "config.json"),
					[]byte(tc.operationalConfig), 0o644); err != nil {
					t.Fatalf("write config.json: %v", err)
				}
			}

			// Active work required: the idle guard suppresses nudges when no
			// beads are in flight.
			d := newTestDaemonWithStores(t, townRoot, map[string]beadsdk.Storage{
				"hq": &searchStorage{results: map[string][]*beadsdk.Issue{
					"in_progress": {{ID: "gt-stuck"}},
				}},
			})

			var logBuf strings.Builder
			d.logger = log.New(&logBuf, "", 0)

			// The respawn path builds a deacon.Manager inside
			// ensureDeaconRunning. Inject a manager with the poller lifecycle
			// stubbed: nudge.StartPoller re-execs the running binary, which
			// under `go test` is the test binary itself. The tmux wrapper
			// stays real so the PATH fake still observes the respawn
			// (new-session).
			d.deaconManagerFactory = func(townRoot string) *deacon.Manager {
				return deacon.NewManagerWithDeps(townRoot, nil, nil, nil)
			}

			for i := 0; i < tc.frozenCalls; i++ {
				d.checkDeaconHeartbeat()
			}

			logOutput := logBuf.String()
			data, err := os.ReadFile(tmuxLog)
			if err != nil {
				t.Fatalf("read tmux log: %v", err)
			}
			// Assert on the respawn (new-session): the stateful fake removes
			// the session on kill-session, so Manager.Start observes a dead
			// session and takes the clean-respawn path (production parity —
			// restartStuckDeacon kills before ensureDeaconRunning respawns).
			// The meaningful invariant is exactly one respawn for a
			// force-restart, zero otherwise.
			respawns := 0
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				if strings.HasPrefix(line, "new-session") {
					respawns++
				}
			}

			if tc.wantRestart {
				if respawns != 1 {
					t.Errorf("%s\nnew-session count = %d, want 1\nlog:\n%s", tc.desc, respawns, logOutput)
				}
				if !strings.Contains(logOutput, "killing stuck Deacon session") {
					t.Errorf("%s\nmissing kill log\nlog:\n%s", tc.desc, logOutput)
				}
				if !strings.Contains(logOutput, "force-restarting session") {
					t.Errorf("%s\nmissing force-restart log\nlog:\n%s", tc.desc, logOutput)
				}
			} else {
				if respawns != 0 {
					t.Errorf("%s\nnew-session count = %d, want 0 (below failure limit)\nlog:\n%s", tc.desc, respawns, logOutput)
				}
			}
		})
	}
}

// TestCheckDeaconHeartbeat_NudgeSuccessResetsBudget verifies that a
// successful nudge (live pane) resets the failure counter: failures must not
// accumulate across a period of health, or a briefly-frozen-then-recovered
// session would be force-restarted on stale pre-recovery history.
// Sequence (default limit 3): fail → succeed (reset) → fail → fail. The 4th
// call must log budget 2/3; without the reset it would log 3/3 and restart.
func TestCheckDeaconHeartbeat_NudgeSuccessResetsBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}

	townRoot := t.TempDir()
	fakeBinDir := t.TempDir()
	tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
	if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
		t.Fatalf("create tmux log: %v", err)
	}

	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_LOG", tmuxLog)
	t.Setenv("COUNTER_FILE", filepath.Join(t.TempDir(), "pane-counter"))
	// Stateful fake session: the deacon exists for the whole test (the nudge
	// tier never kills — the test asserts no kill-session happens).
	stateFile := filepath.Join(fakeBinDir, "deacon-session-state")
	t.Setenv("TMUX_STATE_FILE", stateFile)
	if err := os.WriteFile(stateFile, []byte("1"), 0o644); err != nil {
		t.Fatalf("create session state: %v", err)
	}
	writeDeaconHeartbeat(t, townRoot, 10*time.Minute)

	d := newTestDaemonWithStores(t, townRoot, map[string]beadsdk.Storage{
		"hq": &searchStorage{results: map[string][]*beadsdk.Issue{
			"in_progress": {{ID: "gt-stuck"}},
		}},
	})

	var logBuf strings.Builder
	d.logger = log.New(&logBuf, "", 0)

	// Defensive: if a regression force-restarts the session, the injected
	// manager avoids nudge.StartPoller re-execing the test binary (whose
	// TestMain would spin up a Dolt container).
	d.deaconManagerFactory = func(townRoot string) *deacon.Manager {
		return deacon.NewManagerWithDeps(townRoot, nil, nil, nil)
	}

	// 1. Frozen pane — nudge fails (budget 1/3).
	writeFakeTmuxSimulatesPane(t, fakeBinDir, true)
	d.checkDeaconHeartbeat()
	// 2. Live pane — nudge succeeds (budget resets to 0).
	writeFakeTmuxSimulatesPane(t, fakeBinDir, false)
	d.checkDeaconHeartbeat()
	// 3. Frozen again — nudge fails (budget 1/3, NOT 2/3).
	writeFakeTmuxSimulatesPane(t, fakeBinDir, true)
	d.checkDeaconHeartbeat()
	// 4. Frozen again — nudge fails (budget 2/3 — still below limit).
	d.checkDeaconHeartbeat()

	logOutput := logBuf.String()
	if d.deaconNudgeFailures != 2 {
		t.Errorf("nudge failure budget = %d, want 2 (success must reset the counter)\nlog:\n%s",
			d.deaconNudgeFailures, logOutput)
	}
	if !strings.Contains(logOutput, "consecutive failures: 2/3") {
		t.Errorf("expected budget 2/3 after success reset; got:\n%s", logOutput)
	}
	data, err := os.ReadFile(tmuxLog)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "kill-session") {
			t.Fatalf("unexpected kill-session: budget must not reach the limit without 3 consecutive failures\nlog:\n%s", logOutput)
		}
	}
}
