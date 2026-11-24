package daemon

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// setupSpawnLivenessTest reuses the refinery fake-tmux harness but swaps in a
// capturing logger so tests can assert the EXACT post-spawn liveness log line
// (hq-89smx: "started successfully" must never be logged blindly).
func setupSpawnLivenessTest(t *testing.T) (*refineryTestEnv, *bytes.Buffer) {
	t.Helper()
	env := setupRefineryTest(t)
	buf := &bytes.Buffer{}
	env.daemon.logger = log.New(buf, "", 0)
	return env, buf
}

// TestRefinerySpawnOutcome_Healthy_LogsVerifiedSuccess asserts a session that
// survives the post-spawn checks gets a success line explicitly marked as
// liveness-verified.
func TestRefinerySpawnOutcome_Healthy_LogsVerifiedSuccess(t *testing.T) {
	env, buf := setupSpawnLivenessTest(t)
	writeLines(t, env.sessionsFile, []string{env.name})
	writeLines(t, env.healthyFile, []string{env.name})

	env.daemon.logRefinerySpawnOutcome(env.name, "robloxantcolony")

	out := buf.String()
	if !strings.Contains(out, "started successfully (liveness verified") {
		t.Errorf("expected verified-success line, got: %q", out)
	}
	if strings.Contains(out, "spawned but exited") {
		t.Errorf("healthy session must not log an exit line, got: %q", out)
	}
}

// TestRefinerySpawnOutcome_SessionGone_LogsExitedNotSuccess covers the silent-
// exit class observed in the hq-p0aam kill-test: the new session exits (clean,
// status 0) right after spawn. The old code still logged "started
// successfully"; the fix must log "spawned but exited" instead.
func TestRefinerySpawnOutcome_SessionGone_LogsExitedNotSuccess(t *testing.T) {
	env, buf := setupSpawnLivenessTest(t)
	// Session vanished entirely: sessions file stays empty.

	env.daemon.logRefinerySpawnOutcome(env.name, "robloxantcolony")

	out := buf.String()
	if !strings.Contains(out, "spawned but exited") {
		t.Errorf("expected spawned-but-exited line, got: %q", out)
	}
	if strings.Contains(out, "started successfully") {
		t.Errorf("must never claim success for a dead session, got: %q", out)
	}
}

// TestRefinerySpawnOutcome_DeadPane_LogsExitedWithStatus covers remain-on-exit
// deaths: the session survives but its agent pane is dead. The log must report
// death with the pane exit status and must not claim success.
func TestRefinerySpawnOutcome_DeadPane_LogsExitedWithStatus(t *testing.T) {
	env, buf := setupSpawnLivenessTest(t)
	writeLines(t, env.sessionsFile, []string{env.name})
	writeLines(t, env.deadFile, []string{env.name})
	writeLines(t, env.deadStatus, []string{env.name + "=0"})

	env.daemon.logRefinerySpawnOutcome(env.name, "robloxantcolony")

	out := buf.String()
	if !strings.Contains(out, "agent pane died") || !strings.Contains(out, "exit status 0") {
		t.Errorf("expected dead-pane line with exit status, got: %q", out)
	}
	if strings.Contains(out, "started successfully") {
		t.Errorf("must never claim success for a dead pane, got: %q", out)
	}
}
