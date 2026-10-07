// Idle-timeout auto-suspend for mf-pi actors.
//
// Each mf-pi user is one Actor in the mfpi atespace. The admin server sees
// every user's proxied request because nginx runs an auth_request
// (/_mfpi_auth) for each /<username>/ path before forwarding it to the actor;
// a successful auth is therefore a faithful "last input" signal. The
// background reconciler suspends a RUNNING actor whose tier is in the
// idle-suspend set (small and mid by default) once it has seen no input for
// idleTimeout, releasing its worker/Pod resources while keeping the snapshot
// and user data. The actor is lazily resumed on the next user access by the
// router (Substrate built-in), and that access re-arms the idle clock.
//
// Large-tier actors are intentionally excluded: they are meant to stay warm /
// resident, so they are never auto-suspended for idleness.
//
// The last-input state is deliberately in-memory only. It is transient runtime
// state derived from live traffic; not persisting it means a freshly restarted
// admin never auto-suspends actors whose activity history it does not yet know,
// which is the conservative choice (no premature suspend just because the
// process restarted).
package main

import (
	"strings"
	"sync"
	"time"
)

// idleTiersDefault are the resource tiers whose actors are auto-suspended
// after idleTimeout without any input. Overridable via IDLE_TIERS (e.g.
// "large") and intended to stay {small, mid}: the small and mid tiers host the
// cheap, bursty multiplexed actors, so releasing a worker they sit idle on is
// exactly where the resource win is.
var idleTiersDefault = []string{"small", "mid"}

// idleTracker records the last time each user's actor was touched by real
// input (an authorized proxied request). Safe for concurrent use; handlers
// touch and the reconciler reads.
type idleTracker struct {
	mu   sync.Mutex
	last map[string]time.Time // username -> last input time
}

func newIdleTracker() *idleTracker {
	return &idleTracker{last: map[string]time.Time{}}
}

// touch records an input for the user at the given time.
func (t *idleTracker) touch(name string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.last[name] = at
}

// lastInput returns the user's last recorded input time and whether one exists.
func (t *idleTracker) lastInput(name string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.last[name]
	return at, ok
}

func (t *idleTracker) delete(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.last, name)
}

// parseIdleTiers parses a comma-separated tier list (e.g. "small,mid") into a
// set. Unknown entries are dropped. An empty input yields an empty set, which
// disables idle auto-suspend entirely.
func parseIdleTiers(csv string) map[string]bool {
	out := make(map[string]bool)
	for _, raw := range strings.Split(csv, ",") {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		out[t] = true
	}
	return out
}
