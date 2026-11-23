package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// writeFakeTmuxZombie creates a fake tmux for sweepZombieSessions tests.
//
// Session existence: TMUX_SESSIONS_FILE (one session name per line).
// Dead panes:       TMUX_DEAD_FILE   (one session name per line).
// Kill commands are logged to TMUX_LOG as "kill-session <name>".
//
// Subcommands emulated: has-session, show-environment (always "unknown"),
// display-message (#{pane_dead} / #{pane_pid}), set-option, set-hook,
// kill-session. display-message for #{pane_pid} fails on purpose so
// KillSessionWithProcesses takes the direct KillSession path (skipping the
// 2s process-kill grace sleeps).
func writeFakeTmuxZombie(t *testing.T, dir string) {
	t.Helper()
	script := `#!/usr/bin/env bash
cmd=""; target=""; format=""; skip_next=0; skip_flag=""
for arg in "$@"; do
  if [[ "$skip_next" -eq 1 ]]; then
    if [[ "$skip_flag" == "-t" ]]; then target="$arg"; fi
    if [[ "$skip_flag" == "-p" ]]; then format="$arg"; fi
    skip_next=0
    continue
  fi
  case "$arg" in
    -L|-F) skip_flag="$arg"; skip_next=1 ;;
    -t)    skip_flag="-t"; skip_next=1 ;;
    -p)    skip_flag="-p"; skip_next=1 ;;
    -u)    ;;
    *)     if [[ -z "$cmd" ]]; then cmd="$arg"; fi ;;
  esac
done

# Strip exact-match prefix and window/pane specifiers ("sess:^" -> "sess").
target="${target#=}"
base="${target%%:*}"

case "$cmd" in
  has-session)
    if [[ -n "${TMUX_SESSIONS_FILE:-}" ]] && grep -qxF "$base" "$TMUX_SESSIONS_FILE" 2>/dev/null; then
      exit 0
    fi
    exit 1
    ;;
  show-environment)
    # No session env vars in the fake; liveness queries must fail closed.
    exit 1
    ;;
  display-message)
    if [[ "$format" == *pane_pid* ]]; then
      # Force KillSessionWithProcesses onto the direct KillSession path.
      exit 1
    fi
    if [[ -n "${TMUX_DEAD_FILE:-}" ]] && grep -qxF "$base" "$TMUX_DEAD_FILE" 2>/dev/null; then
      echo 1
    else
      echo 0
    fi
    exit 0
    ;;
  kill-session)
    if [[ -n "${TMUX_LOG:-}" ]]; then
      echo "kill-session $base" >> "$TMUX_LOG"
    fi
    exit 0
    ;;
  set-option|set-hook)
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

// zombieTestEnv holds the test environment for sweepZombieSessions tests.
type zombieTestEnv struct {
	daemon       *Daemon
	tmuxLog      string
	sessionsFile string
	deadFile     string
}

// setupZombieTest creates fake tmux + a minimal Daemon. CWD is moved to a
// temp dir so events.LogFeed cannot find a town root and stays a no-op.
func setupZombieTest(t *testing.T) *zombieTestEnv {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}

	townRoot := t.TempDir()
	fakeBinDir := t.TempDir()
	tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
	sessionsFile := filepath.Join(t.TempDir(), "sessions.txt")
	deadFile := filepath.Join(t.TempDir(), "dead.txt")
	for _, p := range []string{tmuxLog, sessionsFile, deadFile} {
		if err := os.WriteFile(p, []byte{}, 0o644); err != nil {
			t.Fatalf("create %s: %v", p, err)
		}
	}

	writeFakeTmuxZombie(t, fakeBinDir)
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_LOG", tmuxLog)
	t.Setenv("TMUX_SESSIONS_FILE", sessionsFile)
	t.Setenv("TMUX_DEAD_FILE", deadFile)

	t.Chdir(t.TempDir())

	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(io.Discard, "", 0), // discard: not asserted
		tmux:   tmux.NewTmux(),
	}
	t.Cleanup(func() { session.SetDefaultRegistry(session.NewPrefixRegistry()) })

	return &zombieTestEnv{daemon: d, tmuxLog: tmuxLog, sessionsFile: sessionsFile, deadFile: deadFile}
}

func writeLines(t *testing.T, path string, names []string) {
	t.Helper()
	content := strings.Join(names, "\n")
	if len(names) > 0 {
		content += "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readKilledZombieSessions(t *testing.T, tmuxLog string) []string {
	t.Helper()
	data, err := os.ReadFile(tmuxLog)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	var kills []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "kill-session ") {
			kills = append(kills, strings.TrimPrefix(line, "kill-session "))
		}
	}
	return kills
}

func TestSweepZombieSessions_AbsentSession_NoAction(t *testing.T) {
	env := setupZombieTest(t)
	// No sessions at all → nothing to observe, nothing to kill.
	env.daemon.sweepZombieSessions()

	if kills := readKilledZombieSessions(t, env.tmuxLog); len(kills) != 0 {
		t.Errorf("expected no kills, got: %v", kills)
	}
}

func TestSweepZombieSessions_GraceThenKill(t *testing.T) {
	env := setupZombieTest(t)
	origGrace := zombieGrace
	zombieGrace = 1 * time.Nanosecond
	t.Cleanup(func() { zombieGrace = origGrace })

	// hq-mayor exists and its agent pane is dead (handoff-pane-death zombie).
	writeLines(t, env.sessionsFile, []string{session.MayorSessionName()})
	writeLines(t, env.deadFile, []string{session.MayorSessionName()})

	// First sweep: observes, starts the grace window, does NOT kill yet.
	env.daemon.sweepZombieSessions()
	if kills := readKilledZombieSessions(t, env.tmuxLog); len(kills) != 0 {
		t.Fatalf("first sweep should only observe, but killed: %v", kills)
	}

	// Second sweep: grace elapsed, pane still dead → reaped.
	env.daemon.sweepZombieSessions()
	kills := readKilledZombieSessions(t, env.tmuxLog)
	if len(kills) != 1 || kills[0] != session.MayorSessionName() {
		t.Fatalf("second sweep should kill %s, got: %v", session.MayorSessionName(), kills)
	}

	// Third sweep: session is gone now → no further kills, observation dropped.
	writeLines(t, env.sessionsFile, nil)
	writeLines(t, env.deadFile, nil)
	env.daemon.sweepZombieSessions()
	if kills := readKilledZombieSessions(t, env.tmuxLog); len(kills) != 1 {
		t.Fatalf("third sweep should not add kills, got: %v", kills)
	}
}

func TestSweepZombieSessions_SessionRecreatedBeforeGraceDropsObservation(t *testing.T) {
	env := setupZombieTest(t)
	origGrace := zombieGrace
	zombieGrace = time.Minute // long enough that the test window never elapses
	t.Cleanup(func() { zombieGrace = origGrace })

	name := session.MayorSessionName()
	writeLines(t, env.sessionsFile, []string{name})
	writeLines(t, env.deadFile, []string{name})

	// Observe the zombie.
	env.daemon.sweepZombieSessions()

	// The session is gone (e.g. daemon ensure-step recreated and it is no
	// longer a dead pane) → observation must be dropped; no kill ever happens.
	writeLines(t, env.sessionsFile, nil)
	writeLines(t, env.deadFile, nil)
	env.daemon.sweepZombieSessions()
	env.daemon.sweepZombieSessions()

	if kills := readKilledZombieSessions(t, env.tmuxLog); len(kills) != 0 {
		t.Fatalf("expected no kills after recovery, got: %v", kills)
	}
}
