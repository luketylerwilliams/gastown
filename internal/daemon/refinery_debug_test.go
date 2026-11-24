package daemon

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/steveyegge/gastown/internal/refinery"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestDebugRefinerySetup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip()
	}
	const rigName = "robloxantcolony"
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "rigs.json"), []byte(fmt.Sprintf(`{"rigs":{%q:{}}}`, rigName)), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeBinDir := t.TempDir()
	writeFakeTmuxRefinery(t, fakeBinDir)
	tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
	sessionsFile := filepath.Join(t.TempDir(), "sessions.txt")
	deadFile := filepath.Join(t.TempDir(), "dead.txt")
	deadStatus := filepath.Join(t.TempDir(), "dead-status.txt")
	healthyFile := filepath.Join(t.TempDir(), "healthy.txt")
	roeFile := filepath.Join(t.TempDir(), "roe.txt")
	hooksFile := filepath.Join(t.TempDir(), "hooks.txt")
	for _, p := range []string{tmuxLog, sessionsFile, deadFile, deadStatus, healthyFile, roeFile, hooksFile} {
		os.WriteFile(p, []byte{}, 0o644)
	}
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
		logger: log.New(io.Discard, "", 0),
		tmux:   tmux.NewTmux(),
	}
	d.refinerySafetyStopCheck = func(a, b string) (*refinery.SafetyStop, error) { return nil, nil }

	name := session.RefinerySessionName(session.PrefixFor(rigName))
	t.Logf("rigs: %v", d.getKnownRigs())
	t.Logf("name: %q", name)
	os.WriteFile(sessionsFile, []byte(name+"\n"), 0o644)
	os.WriteFile(healthyFile, []byte(name+"\n"), 0o644)
	ok, err := d.tmux.HasSession(name)
	t.Logf("HasSession: %v err=%v", ok, err)
	st := d.tmux.CheckSessionHealth(name, 0)
	t.Logf("CheckSessionHealth: %v", st)
	t.Logf("IsAgentAlive: %v", d.tmux.IsAgentAlive(name))
	gp, gerr := d.tmux.GetEnvironment(name, "GT_PROCESS_NAMES")
	t.Logf("GetEnvironment GT_PROCESS_NAMES: %q err=%v", gp, gerr)
	pc, perr := d.tmux.GetPaneCommand(name)
	t.Logf("GetPaneCommand: %q err=%v", pc, perr)
	t.Logf("SetRemainOnExit err=%v", d.tmux.SetRemainOnExit(name, true))
	roe, roeErr := d.tmux.RemainOnExitEnabled(name)
	t.Logf("RemainOnExitEnabled: %v err=%v", roe, roeErr)
	hook, hookErr := d.tmux.PaneDeadHookConfigured(name)
	t.Logf("PaneDeadHookConfigured: %v err=%v", hook, hookErr)
	d.sweepRefineryLiveness()
	data, _ := os.ReadFile(tmuxLog)
	t.Logf("tmux log:\n%s", data)
}
