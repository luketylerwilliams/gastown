// ABOUTME: Identity-verified liveness for GT sessions (hq-p4po).
// ABOUTME: Name-based pane matching can be spoofed by a foreign agent process
// running in the target pane (the "pane mixup" class); these helpers verify
// that a name-matched process carries the same GT_ROLE/BD_ACTOR identity the
// session declares, so a wrong agent in the pane cannot report as alive.
package tmux

import (
	"os"
	"runtime"
	"strings"
)

// Identity marker environment variables. They are set on the tmux session
// environment at startup (internal/session) and inherited by every process
// the session spawns.
const (
	identityRoleEnv  = "GT_ROLE"
	identityActorEnv = "BD_ACTOR"
)

// processIdentity is the identity carried by a single process environment.
type processIdentity struct {
	role  string
	actor string
	// known reports whether at least one marker was present in the
	// environment. A process without any marker (bare runtime, legacy
	// session) imposes no identity constraint.
	known bool
}

// sessionIdentity is the identity a session declares via its tmux environment.
type sessionIdentity struct {
	role  string
	actor string
	known bool
}

// parseEnvironIdentity extracts identity markers from a raw NUL-separated
// environment blob (the on-disk format of /proc/<pid>/environ).
func parseEnvironIdentity(b []byte) processIdentity {
	id := processIdentity{}
	for _, kv := range strings.Split(string(b), "\x00") {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch key {
		case identityRoleEnv:
			if id.role == "" {
				id.role = value
				id.known = true
			}
		case identityActorEnv:
			if id.actor == "" {
				id.actor = value
				id.known = true
			}
		}
	}
	return id
}

// readProcessIdentity reads GT_ROLE/BD_ACTOR from /proc/<pid>/environ.
// It returns an identity with known=false (no error) on non-Linux platforms
// or when the process no longer exists, so callers fall back to the legacy
// name-based behavior instead of restarting a healthy session.
func readProcessIdentity(pid string) (processIdentity, error) {
	if runtime.GOOS == "windows" {
		return processIdentity{}, nil
	}
	b, err := os.ReadFile("/proc/" + pid + "/environ")
	if err != nil {
		if os.IsNotExist(err) {
			return processIdentity{}, nil
		}
		return processIdentity{}, err
	}
	return parseEnvironIdentity(b), nil
}

// sessionIdentityFromEnv builds a declared identity from raw environment
// key/value pairs (already trimmed of blank values).
func sessionIdentityFromEnv(pairs map[string]string) sessionIdentity {
	id := sessionIdentity{}
	if r, ok := pairs[identityRoleEnv]; ok {
		id.role = r
		id.known = true
	}
	if a, ok := pairs[identityActorEnv]; ok {
		id.actor = a
		id.known = true
	}
	return id
}

// SessionIdentity reports the identity the session declares via its tmux
// environment.
func (t *Tmux) SessionIdentity(session string) (sessionIdentity, error) {
	pairs := map[string]string{}
	for _, key := range []string{identityRoleEnv, identityActorEnv} {
		v, ok, err := t.getEnvironmentOptional(session, key)
		if err != nil {
			return sessionIdentity{}, err
		}
		if ok && v != "" {
			pairs[key] = v
		}
	}
	return sessionIdentityFromEnv(pairs), nil
}

// identityMatches reports whether the matched process carries the identity
// the session declares. The role is compared when both sides declare one,
// and the actor is compared when both sides declare one. A side that
// declares nothing imposes no constraint (legacy sessions keep working).
func identityMatches(expected sessionIdentity, actual processIdentity) bool {
	if expected.role != "" && actual.role != "" && expected.role != actual.role {
		return false
	}
	if expected.actor != "" && actual.actor != "" && expected.actor != actual.actor {
		return false
	}
	return true
}
