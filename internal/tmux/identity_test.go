// ABOUTME: Unit tests for identity-verified liveness (hq-p4po).
package tmux

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"testing"
	"time"
)

func TestParseEnvironIdentity(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		wantRole string
		wantAct  string
		wantKnow bool
	}{
		{
			name:     "full identity",
			raw:      "PATH=/usr/bin\x00GT_ROLE=mayor\x00BD_ACTOR=mayor\x00HOME=/root",
			wantRole: "mayor",
			wantAct:  "mayor",
			wantKnow: true,
		},
		{
			name:     "role only",
			raw:      "GT_ROLE=robloxantcolony/refinery\x00OTHER=1",
			wantRole: "robloxantcolony/refinery",
			wantAct:  "",
			wantKnow: true,
		},
		{
			name:     "actor only",
			raw:      "BD_ACTOR=deacon/",
			wantRole: "",
			wantAct:  "deacon/",
			wantKnow: true,
		},
		{
			name:     "no markers",
			raw:      "PATH=/usr/bin\x00HOME=/root",
			wantKnow: false,
		},
		{
			name:     "empty blob",
			raw:      "",
			wantKnow: false,
		},
		{
			name:     "no trailing NUL and unprefixed entry",
			raw:      "FOO=bar\x00GT_ROLE=mayor",
			wantRole: "mayor",
			wantKnow: true,
		},
		{
			name:     "value with equals sign",
			raw:      "BD_ACTOR=a=b\x00GT_ROLE=r",
			wantRole: "r",
			wantAct:  "a=b",
			wantKnow: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseEnvironIdentity([]byte(tt.raw))
			if got.role != tt.wantRole {
				t.Errorf("role = %q, want %q", got.role, tt.wantRole)
			}
			if got.actor != tt.wantAct {
				t.Errorf("actor = %q, want %q", got.actor, tt.wantAct)
			}
			if got.known != tt.wantKnow {
				t.Errorf("known = %v, want %v", got.known, tt.wantKnow)
			}
		})
	}
}

func TestIdentityMatches(t *testing.T) {
	roleOnly := func(r string) processIdentity { return processIdentity{role: r, known: true} }
	full := func(r, a string) processIdentity { return processIdentity{role: r, actor: a, known: true} }

	tests := []struct {
		name     string
		expected sessionIdentity
		actual   processIdentity
		want     bool
	}{
		{
			name:     "same role and actor",
			expected: sessionIdentity{role: "mayor", actor: "mayor", known: true},
			actual:   full("mayor", "mayor"),
			want:     true,
		},
		{
			name:     "foreign role (pane mixup)",
			expected: sessionIdentity{role: "mayor", actor: "mayor", known: true},
			actual:   full("robloxantcolony/refinery", "robloxantcolony/refinery"),
			want:     false,
		},
		{
			name:     "same role different actor",
			expected: sessionIdentity{role: "refinery", actor: "darkstate/refinery", known: true},
			actual:   full("refinery", "robloxantcolony/refinery"),
			want:     false,
		},
		{
			name:     "actual without actor imposes no actor constraint",
			expected: sessionIdentity{role: "mayor", actor: "mayor", known: true},
			actual:   roleOnly("mayor"),
			want:     true,
		},
		{
			name:     "expected without actor imposes no actor constraint",
			expected: sessionIdentity{role: "mayor", known: true},
			actual:   full("mayor", "mayor"),
			want:     true,
		},
		{
			name:     "expected without role imposes no role constraint",
			expected: sessionIdentity{actor: "mayor", known: true},
			actual:   full("anything", "mayor"),
			want:     true,
		},
		{
			name:     "foreign role wins over matching actor",
			expected: sessionIdentity{role: "mayor", actor: "x", known: true},
			actual:   full("other", "x"),
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := identityMatches(tt.expected, tt.actual); got != tt.want {
				t.Errorf("identityMatches(%+v, %+v) = %v, want %v", tt.expected, tt.actual, got, tt.want)
			}
		})
	}
}

func TestSessionIdentityFromEnv(t *testing.T) {
	tests := []struct {
		name     string
		pairs    map[string]string
		wantRole string
		wantAct  string
		wantKnow bool
	}{
		{name: "none", pairs: map[string]string{}},
		{
			name:     "both",
			pairs:    map[string]string{identityRoleEnv: "mayor", identityActorEnv: "mayor"},
			wantRole: "mayor",
			wantAct:  "mayor",
			wantKnow: true,
		},
		{
			name:     "role only",
			pairs:    map[string]string{identityRoleEnv: "mayor"},
			wantRole: "mayor",
			wantKnow: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sessionIdentityFromEnv(tt.pairs)
			if got.role != tt.wantRole || got.actor != tt.wantAct || got.known != tt.wantKnow {
				t.Errorf("got %+v, want role=%q actor=%q known=%v", got, tt.wantRole, tt.wantAct, tt.wantKnow)
			}
		})
	}
}

func TestReadProcessIdentityChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only")
	}
	cmd := exec.Command("sleep", "10")
	// Set the identity directly on the child's environment: the kernel
	// applies it at execve, so /proc/<pid>/environ is correct from the
	// start (no exec-wrapper race).
	cmd.Env = append(os.Environ(), "GT_ROLE=mayor", "BD_ACTOR=mayor")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting child: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	// Poll: the kernel applies the child's environment at execve, which
	// happens a moment after Start returns (fork first carries the
	// parent's environ).
	var id processIdentity
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		id, err = readProcessIdentity(strconv.Itoa(cmd.Process.Pid))
		if err != nil {
			t.Fatalf("readProcessIdentity: %v", err)
		}
		if id.known {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child never showed identity in /proc environ: %+v", id)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if id.role != "mayor" || id.actor != "mayor" {
		t.Errorf("got %+v, want role=mayor actor=mayor", id)
	}
}

func TestReadProcessIdentityGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux-only")
	}
	// A nonexistent PID must not produce an error — the caller falls back
	// to the legacy name-based result instead of acting on a read failure.
	id, err := readProcessIdentity("99999999")
	if err != nil {
		t.Fatalf("expected no error for nonexistent pid, got %v", err)
	}
	if id.known {
		t.Errorf("nonexistent pid should report known=false")
	}
}
