package daemon

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// writeFakeTmuxRefinery creates a fake tmux for sweepRefineryLiveness tests.
//
//   - TMUX_SESSIONS_FILE: one session name per line (has-session).
//   - TMUX_DEAD_FILE:     sessions whose agent pane is dead (#{pane_dead}).
//   - TMUX_DEAD_STATUS:   "name=STATUS" lines for #{pane_dead_status}
//     (clean status-0 exit vs crash classification).
//   - TMUX_HEALTHY_FILE:  sessions whose agent process is alive (emulated:
//     pane_current_command + GT_PROCESS_NAMES resolve to PID 1's comm, a
//     direct command match in matchesPaneRuntimeChecked).
//   - TMUX_ROE_FILE:      sessions with remain-on-exit explicitly on.
//   - TMUX_HOOKS_FILE:    sessions with a session-scoped pane-died hook.
//
// State changes (set-option remain-on-exit, set-hook pane-died, kill-session)
// are logged to TMUX_LOG as
//
//	set-option remain-on-exit <name> <on|off>
//	set-hook pane-died <name>
//	kill-session <name>
//
// so tests can assert the self-heal wiring, and the fake's state files are
// updated so a re-run of the sweep sees the healed state (idempotency).
//
// Subcommand emulation: has-session, show-environment, display-message
// (pane_dead / pane_dead_status / pane_current_command / pane_pid /
// session_name), show-options -s (remain-on-exit), show-hooks -w, set-option,
// set-hook, kill-session. display-message for #{pane_pid} fails outside
// TMUX_HEALTHY_FILE so KillSessionWithProcesses takes the direct KillSession
// path (skipping process-kill grace sleeps).
func writeFakeTmuxRefinery(t *testing.T, dir string) {
	t.Helper()
	script := `#!/usr/bin/env bash
FAKE_COMM="$(ps -p 1 -o comm= 2>/dev/null | tr -d '[:space:]')"
[[ -z "$FAKE_COMM" ]] && FAKE_COMM="bash"
cmd=""; target=""; format=""; val1=""; val2=""; pos=0
args=("$@")
i=0
while [[ $i -lt ${#args[@]} ]]; do
  arg="${args[$i]}"
  case "$arg" in
    -u|-w|-g|-s|-b) ;;  # bare flags
    -L) i=$((i+1));;    # socket name (tmux.NewTmux() appends -L <socket> in daemon tests; fake ignores sockets)
    -t) i=$((i+1)); target="${args[$i]}" ;;
    -p|-F) i=$((i+1)); format="${args[$i]}" ;;
    *)
      case "$cmd" in
        "") cmd="$arg" ;;  # the command word itself is not a positional
        set-option|set-hook)
          case $pos in 0) val1="$arg" ;; 1) val2="$arg" ;; esac
          pos=$((pos+1))
          ;;
        show-environment)
          [[ $pos -eq 0 ]] && val1="$arg"
          pos=$((pos+1))
          ;;
      esac
      ;;
  esac
  i=$((i+1))
done
target="${target#=}"
base="${target%%:*}"

in_file() { [[ -n "${1:-}" ]] && [[ -n "${2:-}" ]] && grep -qxF "$base" "$2" 2>/dev/null; }

case "$cmd" in
  has-session)
    if in_file x "$TMUX_SESSIONS_FILE"; then exit 0; fi
    exit 1
    ;;
  show-environment)
    if [[ "$val1" == "GT_PROCESS_NAMES" ]] && in_file x "$TMUX_HEALTHY_FILE"; then
      echo "GT_PROCESS_NAMES=$FAKE_COMM"
      exit 0
    fi
    echo "unknown variable ${val1:-}" >&2
    exit 1
    ;;
  display-message)
    case "$format" in
      # pane_dead_status MUST be tested before pane_dead: the latter glob
      # also matches #{pane_dead_status} and would return the dead flag
      # (1/0) instead of the exit status (hq-p0aam classification input).
      *pane_dead_status*)
        status="0"
        if [[ -n "${TMUX_DEAD_STATUS_FILE:-}" ]]; then
          # Fixed-string prefix match: grep -x + glob would never match
          # "name=STATUS" lines (the * became a regex quantifier on =).
          line="$(grep -F "${base}=" "$TMUX_DEAD_STATUS_FILE" 2>/dev/null | head -1)"
          [[ -n "$line" ]] && status="${line#*=}"
        fi
        echo "$status"
        exit 0
        ;;
      *pane_dead*)
        if in_file x "$TMUX_DEAD_FILE"; then echo 1; else echo 0; fi
        exit 0
        ;;
      *pane_current_command*)
        if in_file x "$TMUX_HEALTHY_FILE"; then echo "$FAKE_COMM"; else echo "opencode"; fi
        exit 0
        ;;
      *pane_pid*)
        if in_file x "$TMUX_HEALTHY_FILE"; then echo 1; exit 0; fi
        exit 1
        ;;
      *session_name*)
        echo "$base"
        exit 0
        ;;
    esac
    exit 0
    ;;
  show-options)
    # Emulates "show-options -s -t <sess> remain-on-exit": session-scoped value.
    if in_file x "$TMUX_ROE_FILE"; then
      echo "remain-on-exit on"
    else
      echo "remain-on-exit off"
    fi
    exit 0
    ;;
  show-hooks)
    if in_file x "$TMUX_HOOKS_FILE"; then
      echo 'pane-died[0] run-shell "gt log crash"'
    fi
    exit 0
    ;;
  set-option)
    if [[ "$val1" == "remain-on-exit" && -n "${TMUX_LOG:-}" ]]; then
      echo "set-option remain-on-exit $base $val2" >> "$TMUX_LOG"
    fi
    if [[ -n "${TMUX_ROE_FILE:-}" ]]; then
      if [[ "$val2" == "on" ]]; then
        grep -qxF "$base" "$TMUX_ROE_FILE" 2>/dev/null || echo "$base" >> "$TMUX_ROE_FILE"
      else
        tmp="$(mktemp)"; grep -vxF "$base" "$TMUX_ROE_FILE" > "$tmp" 2>/dev/null || true
        mv -f "$tmp" "$TMUX_ROE_FILE" 2>/dev/null || true
      fi
    fi
    exit 0
    ;;
  set-hook)
    if [[ "$val1" == "pane-died" ]]; then
      if [[ -n "${TMUX_LOG:-}" ]]; then
        echo "set-hook pane-died $base" >> "$TMUX_LOG"
      fi
      if [[ -n "${TMUX_HOOKS_FILE:-}" ]]; then
        grep -qxF "$base" "$TMUX_HOOKS_FILE" 2>/dev/null || echo "$base" >> "$TMUX_HOOKS_FILE"
      fi
    fi
    exit 0
    ;;
  kill-session)
    if [[ -n "${TMUX_LOG:-}" ]]; then
      echo "kill-session $base" >> "$TMUX_LOG"
    fi
    if [[ -n "${TMUX_SESSIONS_FILE:-}" ]]; then
      tmp="$(mktemp)"; grep -vxF "$base" "$TMUX_SESSIONS_FILE" > "$tmp" 2>/dev/null || true
      mv -f "$tmp" "$TMUX_SESSIONS_FILE" 2>/dev/null || true
    fi
    exit 0
    ;;
esac
exit 0
`
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
}

// refineryTestEnv holds the test environment for sweepRefineryLiveness tests.
type refineryTestEnv struct {
	daemon       *Daemon
	name         string
	tmuxLog      string
	sessionsFile string
	deadFile     string
	deadStatus   string
	healthyFile  string
	roeFile      string
	hooksFile    string
}

// setupRefineryTest creates fake tmux + a minimal Daemon with one known rig
// and no active safety stop. CWD is moved to a temp dir so events.LogFeed
// cannot find a town root and stays a no-op.
func setupRefineryTest(t *testing.T) *refineryTestEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}

	const rigName = "robloxantcolony"
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("mkdir mayor: %v", err)
	}
	rigsJSON := fmt.Sprintf(`{"rigs":{%q:{}}}`, rigName)
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), []byte(rigsJSON), 0o644); err != nil {
		t.Fatalf("write rigs.json: %v", err)
	}

	fakeBinDir := t.TempDir()
	tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
	sessionsFile := filepath.Join(t.TempDir(), "sessions.txt")
	deadFile := filepath.Join(t.TempDir(), "dead.txt")
	deadStatus := filepath.Join(t.TempDir(), "dead-status.txt")
	healthyFile := filepath.Join(t.TempDir(), "healthy.txt")
	roeFile := filepath.Join(t.TempDir(), "roe.txt")
	hooksFile := filepath.Join(t.TempDir(), "hooks.txt")
	for _, p := range []string{tmuxLog, sessionsFile, deadFile, deadStatus, healthyFile, roeFile, hooksFile} {
		if err := os.WriteFile(p, []byte{}, 0o644); err != nil {
			t.Fatalf("create %s: %v", p, err)
		}
	}

	writeFakeTmuxRefinery(t, fakeBinDir)
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_LOG", tmuxLog)
	t.Setenv("TMUX_SESSIONS_FILE", sessionsFile)
	t.Setenv("TMUX_DEAD_FILE", deadFile)
	t.Setenv("TMUX_DEAD_STATUS_FILE", deadStatus)
	t.Setenv("TMUX_HEALTHY_FILE", healthyFile)
	t.Setenv("TMUX_ROE_FILE", roeFile)
	t.Setenv("TMUX_HOOKS_FILE", hooksFile)

	t.Chdir(t.TempDir())

	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(io.Discard, "", 0), // discard: not asserted
		tmux:   tmux.NewTmux(),
	}
	// No safety stop in the test town (no beads DB to query anyway).
	d.refinerySafetyStopCheck = func(townRoot, rigName string) (*refinery.SafetyStop, error) {
		return nil, nil
	}
	t.Cleanup(func() { session.SetDefaultRegistry(session.NewPrefixRegistry()) })

	return &refineryTestEnv{
		daemon:       d,
		name:         session.RefinerySessionName(session.PrefixFor(rigName)),
		tmuxLog:      tmuxLog,
		sessionsFile: sessionsFile,
		deadFile:     deadFile,
		deadStatus:   deadStatus,
		healthyFile:  healthyFile,
		roeFile:      roeFile,
		hooksFile:    hooksFile,
	}
}

func readTMUXLog(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func logContains(t *testing.T, lines []string, want string) bool {
	t.Helper()
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

// TestRefineryLiveness_Healthy_InstallsResilienceWiring verifies the steady-
// state self-heal (hq-p0aam scope a + crash trace): a healthy refinery with
// remain-on-exit off and no pane-died hook gets both fixed, and a re-sweep is
// idempotent (no repeat set-option/set-hook).
func TestRefineryLiveness_Healthy_InstallsResilienceWiring(t *testing.T) {
	env := setupRefineryTest(t)
	writeLines(t, env.sessionsFile, []string{env.name})
	writeLines(t, env.healthyFile, []string{env.name})

	env.daemon.sweepRefineryLiveness()

	lines := readTMUXLog(t, env.tmuxLog)
	if !logContains(t, lines, "set-option remain-on-exit "+env.name+" on") {
		t.Errorf("expected remain-on-exit set on, log: %v", lines)
	}
	if !logContains(t, lines, "set-hook pane-died "+env.name) {
		t.Errorf("expected pane-died hook installed, log: %v", lines)
	}
	if kills := readKilledZombieSessions(t, env.tmuxLog); len(kills) != 0 {
		t.Errorf("healthy session must not be killed, got: %v", kills)
	}

	// Second sweep: wiring is in place → no new set-option / set-hook.
	// Compare line counts: sweep 1 legitimately wrote the wiring lines above.
	before := len(readTMUXLog(t, env.tmuxLog))
	env.daemon.sweepRefineryLiveness()
	lines = readTMUXLog(t, env.tmuxLog)
	for _, line := range lines[before:] {
		if strings.Contains(line, "set-option") || strings.Contains(line, "set-hook") {
			t.Errorf("second sweep must be idempotent, new lines: %v", lines[before:])
		}
	}
}

// TestRefineryLiveness_Healthy_KeepsExistingHook verifies sessions that
// already carry a session-scoped pane-died hook (the refinery start path
// since 0511e79) are NOT re-wired: no duplicate hook, remain-on-exit is
// still fixed, and the daemon (not a tmux auto-respawn hook) stays the
// respawn authority.
func TestRefineryLiveness_Healthy_KeepsExistingHook(t *testing.T) {
	env := setupRefineryTest(t)
	writeLines(t, env.sessionsFile, []string{env.name})
	writeLines(t, env.healthyFile, []string{env.name})
	writeLines(t, env.hooksFile, []string{env.name})

	env.daemon.sweepRefineryLiveness()

	lines := readTMUXLog(t, env.tmuxLog)
	for _, line := range lines {
		if strings.Contains(line, "set-hook") {
			t.Errorf("existing hook must not be touched, got: %v", lines)
		}
	}
	if !logContains(t, lines, "set-option remain-on-exit "+env.name+" on") {
		t.Errorf("expected remain-on-exit set on, log: %v", lines)
	}
}

// TestRefineryLiveness_DeadPane_ClassifiesAndReaps is the core silent-exit
// path: a retained dead pane (incl. clean status-0 exit) is classified after
// the grace window, reaped with a visible session-death event, and the
// result suppresses ensureRefineryRunning's event gate (refineryLivenessHealed)
// so the successor is spawned even with an empty merge queue.
func TestRefineryLiveness_DeadPane_ClassifiesAndReaps(t *testing.T) {
	env := setupRefineryTest(t)
	origGrace := zombieGrace
	zombieGrace = 1 * time.Nanosecond
	t.Cleanup(func() { zombieGrace = origGrace })

	// Steady state: healthy session, wiring installed.
	writeLines(t, env.sessionsFile, []string{env.name})
	writeLines(t, env.healthyFile, []string{env.name})
	env.daemon.sweepRefineryLiveness()

	// The agent exits cleanly (status 0) — pane retained by remain-on-exit.
	writeLines(t, env.healthyFile, nil)
	writeLines(t, env.deadFile, []string{env.name})
	writeLines(t, env.deadStatus, []string{env.name + "=0"})

	env.daemon.sweepRefineryLiveness()

	lines := readTMUXLog(t, env.tmuxLog)
	if !logContains(t, lines, "kill-session "+env.name) {
		t.Fatalf("classified dead refinery must be reaped, log: %v", lines)
	}
	if !env.daemon.refineryLivenessHealed(env.name) {
		t.Fatalf("classified death must suppress the event gate (refineryLivenessHealed)")
	}
	if env.daemon.refineryLivenessHealed(env.name) {
		t.Fatalf("refineryLivenessHealed must be consumed (single respawn per classification)")
	}
}

// TestRefineryLiveness_DeadPane_GraceHoldsOff ensures a dead pane observed
// for less than the grace window is only classified (not reaped): a healthy
// handoff shows a dead pane sub-second, and the refinery pane may be mid-
// respawn.
func TestRefineryLiveness_DeadPane_GraceHoldsOff(t *testing.T) {
	env := setupRefineryTest(t)
	origGrace := zombieGrace
	zombieGrace = time.Hour
	t.Cleanup(func() { zombieGrace = origGrace })

	writeLines(t, env.sessionsFile, []string{env.name})
	writeLines(t, env.healthyFile, []string{env.name})
	env.daemon.sweepRefineryLiveness()

	// First observation of the dead pane: classify, hold off.
	writeLines(t, env.healthyFile, nil)
	writeLines(t, env.deadFile, []string{env.name})
	env.daemon.sweepRefineryLiveness()
	if kills := readKilledZombieSessions(t, env.tmuxLog); len(kills) != 0 {
		t.Fatalf("first dead-pane observation must not kill, got: %v", kills)
	}

	// Within the grace window: still no kill.
	env.daemon.sweepRefineryLiveness()
	if kills := readKilledZombieSessions(t, env.tmuxLog); len(kills) != 0 {
		t.Fatalf("dead pane inside grace window must not kill, got: %v", kills)
	}
}

// TestRefineryLiveness_VanishedSession_MarksForRespawn covers the original
// ro-66o manifestation: remain-on-exit off destroyed the pane on exit, so
// the session is simply GONE — no dead pane to classify. The sweep must mark
// the vanishing so ensureRefineryRunning respawns it despite an empty merge
// queue, and must not double-mark on subsequent sweeps.
func TestRefineryLiveness_VanishedSession_MarksForRespawn(t *testing.T) {
	env := setupRefineryTest(t)

	// Healthy on the first sweep.
	writeLines(t, env.sessionsFile, []string{env.name})
	writeLines(t, env.healthyFile, []string{env.name})
	env.daemon.sweepRefineryLiveness()

	// The session vanished (pane not retained).
	writeLines(t, env.sessionsFile, nil)
	writeLines(t, env.healthyFile, nil)
	env.daemon.sweepRefineryLiveness()

	if !env.daemon.refineryLivenessHealed(env.name) {
		t.Fatalf("vanished session must be marked for respawn (refineryLivenessHealed)")
	}
	if env.daemon.refineryLivenessHealed(env.name) {
		t.Fatalf("mark must be consumed (single respawn per vanishing)")
	}

	// Subsequent sweeps: no new marks, no kills (nothing to kill).
	before := readTMUXLog(t, env.tmuxLog)
	env.daemon.sweepRefineryLiveness()
	env.daemon.sweepRefineryLiveness()
	after := readTMUXLog(t, env.tmuxLog)
	if len(after) != len(before) {
		t.Fatalf("no action expected for an already-absent session, new log lines: %v", after[len(before):])
	}
}

// TestRefineryLiveness_SafetyStop_Skipped verifies safety-stopped rigs are
// untouched: no wiring, no classification, no respawn marks — even if the
// session vanishes.
func TestRefineryLiveness_SafetyStop_Skipped(t *testing.T) {
	env := setupRefineryTest(t)
	env.daemon.refinerySafetyStopCheck = func(townRoot, rigName string) (*refinery.SafetyStop, error) {
		return &refinery.SafetyStop{}, nil
	}

	// Healthy session on a safety-stopped rig: untouched.
	writeLines(t, env.sessionsFile, []string{env.name})
	writeLines(t, env.healthyFile, []string{env.name})
	env.daemon.sweepRefineryLiveness()
	if lines := readTMUXLog(t, env.tmuxLog); len(lines) != 0 {
		t.Fatalf("safety-stopped rig must be untouched, got: %v", lines)
	}

	// Vanished session on a safety-stopped rig: no respawn mark.
	writeLines(t, env.sessionsFile, nil)
	writeLines(t, env.healthyFile, nil)
	env.daemon.sweepRefineryLiveness()
	if env.daemon.refineryLivenessHealed(env.name) {
		t.Fatalf("safety-stopped rig must not be marked for respawn")
	}
}

// TestSweepZombieSessions_SkipsClaimedRefineries verifies the interaction
// with the generic hq-hpd1 sweep: a refinery session claimed by
// sweepRefineryLiveness (dead pane observed, grace not yet elapsed) must not
// be reaped by sweepZombieSessions in the same heartbeat.
func TestSweepZombieSessions_SkipsClaimedRefineries(t *testing.T) {
	env := setupRefineryTest(t)
	origGrace := zombieGrace
	zombieGrace = time.Hour // the refinery sweep will never reach classify
	t.Cleanup(func() { zombieGrace = origGrace })

	// Healthy first so the refinery sweep claims the name.
	writeLines(t, env.sessionsFile, []string{env.name})
	writeLines(t, env.healthyFile, []string{env.name})
	env.daemon.sweepRefineryLiveness()

	// Now the pane is dead; both sweeps run in heartbeat order.
	writeLines(t, env.healthyFile, nil)
	writeLines(t, env.deadFile, []string{env.name})
	env.daemon.sweepRefineryLiveness()
	env.daemon.sweepZombieSessions()

	if kills := readKilledZombieSessions(t, env.tmuxLog); len(kills) != 0 {
		t.Fatalf("claimed refinery must not be reaped by the generic zombie sweep, got: %v", kills)
	}
}
