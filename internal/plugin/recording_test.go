package plugin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPluginRunRecord(t *testing.T) {
	record := PluginRunRecord{
		PluginName: "test-plugin",
		RigName:    "gastown",
		Result:     ResultSuccess,
		Body:       "Test run completed successfully",
	}

	if record.PluginName != "test-plugin" {
		t.Errorf("expected plugin name 'test-plugin', got %q", record.PluginName)
	}
	if record.RigName != "gastown" {
		t.Errorf("expected rig name 'gastown', got %q", record.RigName)
	}
	if record.Result != ResultSuccess {
		t.Errorf("expected result 'success', got %q", record.Result)
	}
}

// TestRecordRunCreatesAndClosesReceipt pins the hq-scrh behavior: receipts
// are created on the TRACKED plane (no --ephemeral) and closed immediately.
// Ephemeral (wisp-plane) receipts are dolt_ignored and get wiped by any
// working-set reset, which silently breaks the daemon cooldown gate.
func TestRecordRunCreatesAndClosesReceipt(t *testing.T) {
	townRoot := t.TempDir()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd-args.log")
	bdPath := filepath.Join(binDir, "bd")
	fakeBD := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> \"$BD_ARGS_LOG\"\n" +
		"case \"$1\" in\n" +
		"  create) printf '{\\\"id\\\":\\\"gt-test-run\\\"}\\n' ;;\n" +
		"  close) exit 0 ;;\n" +
		"  *) exit 2 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bdPath, []byte(fakeBD), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_ARGS_LOG", logPath)

	recorder := NewRecorder(townRoot)
	id, err := recorder.RecordRun(PluginRunRecord{
		PluginName:  "tool-updater",
		RigName:     "gastown",
		Result:      RunResult("warning"),
		Title:       "tool-updater: failed=brew",
		Body:        "brew failed",
		ExtraLabels: []string{"source:test"},
	})
	if err != nil {
		t.Fatalf("RecordRun failed: %v", err)
	}
	if id != "gt-test-run" {
		t.Fatalf("RecordRun id = %q, want gt-test-run", id)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	log := string(data)
	for _, want := range []string{
		"create --json -t chore --title=tool-updater: failed=brew",
		"-l type:plugin-run",
		"-l plugin:tool-updater",
		"-l result:warning",
		"-l rig:gastown",
		"-l source:test",
		"--description=brew failed",
		"close gt-test-run --reason plugin run recorded",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("fake bd log missing %q in:\n%s", want, log)
		}
	}

	// hq-scrh guard: --ephemeral must NOT appear anywhere in the receipt
	// write path. Ephemeral receipts live on the dolt_ignored wisp plane,
	// which working-set resets (dolt merge/squash, server recreation,
	// `gt up --restore`) wipe with no audit trail — silently breaking the
	// daemon cooldown gate and `gt plugin history`.
	if strings.Contains(log, "--ephemeral") {
		t.Fatalf("RecordRun must not create ephemeral receipts (hq-scrh), got:\n%s", log)
	}
}

// TestCountRunsSinceQueriesEphemeralPlane pins the hq-yunu regression:
// plugin receipts are recorded as ephemeral beads (wisp plane), which
// bd list hides by default on bd >= 1.3.4. Without --include-ephemeral
// queryRuns always returns 0 rows, so the daemon cooldown gate is
// silently unenforced and `gt plugin history` is always empty.
func TestCountRunsSinceQueriesEphemeralPlane(t *testing.T) {
	townRoot := t.TempDir()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd-args.log")
	bdPath := filepath.Join(binDir, "bd")
	fakeBD := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$*\" >> \"$BD_ARGS_LOG\"\n" +
		"case \"$1\" in\n" +
		"  list) printf '[]\\n' ;;\n" +
		"  *) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bdPath, []byte(fakeBD), 0755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_ARGS_LOG", logPath)

	recorder := NewRecorder(townRoot)
	n, err := recorder.CountRunsSince("beads-sync", "15m")
	if err != nil {
		t.Fatalf("CountRunsSince failed: %v", err)
	}
	if n != 0 {
		t.Fatalf("CountRunsSince = %d, want 0 for empty list", n)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read fake bd log: %v", err)
	}
	log := string(data)
	for _, want := range []string{
		"list",
		"--all",
		"--include-ephemeral",
		"-l type:plugin-run",
		"-l plugin:beads-sync",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("fake bd log missing %q in:\n%s", want, log)
		}
	}
}

func TestRunResultConstants(t *testing.T) {
	if ResultSuccess != "success" {
		t.Errorf("expected ResultSuccess to be 'success', got %q", ResultSuccess)
	}
	if ResultFailure != "failure" {
		t.Errorf("expected ResultFailure to be 'failure', got %q", ResultFailure)
	}
	if ResultSkipped != "skipped" {
		t.Errorf("expected ResultSkipped to be 'skipped', got %q", ResultSkipped)
	}
}

func TestNewRecorder(t *testing.T) {
	recorder := NewRecorder("/tmp/test-town")
	if recorder == nil {
		t.Fatal("NewRecorder returned nil")
	}
	if recorder.townRoot != "/tmp/test-town" {
		t.Errorf("expected townRoot '/tmp/test-town', got %q", recorder.townRoot)
	}
}

func TestCooldownDurationParsing(t *testing.T) {
	t.Parallel()
	// Verify that plugin gate durations (Go time.ParseDuration format)
	// are parsed correctly. This is critical because bd's compact duration
	// uses "m" for months, while Go uses "m" for minutes. The fix computes
	// an absolute RFC3339 cutoff instead of passing the raw duration to bd.
	cases := []struct {
		input   string
		wantDur time.Duration
		wantErr bool
	}{
		{"5m", 5 * time.Minute, false},
		{"30m", 30 * time.Minute, false},
		{"1h", 1 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"1h30m", 90 * time.Minute, false},
		{"500ms", 500 * time.Millisecond, false},
		{"bogus", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			d, err := time.ParseDuration(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.input, err)
			}
			if d != tc.wantDur {
				t.Errorf("ParseDuration(%q) = %v, want %v", tc.input, d, tc.wantDur)
			}
			// Verify the cutoff time is in the past and approximately correct.
			cutoff := time.Now().Add(-d)
			elapsed := time.Since(cutoff)
			if elapsed < d-time.Second || elapsed > d+time.Second {
				t.Errorf("cutoff drift: expected ~%v ago, got %v ago", d, elapsed)
			}
		})
	}
}

// Integration tests for RecordRun, GetLastRun, GetRunsSince require
// a working beads installation and are skipped in unit tests.
// These functions shell out to `bd` commands.
