package ha

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Role is the last confirmed PostgreSQL role of the monitored node.
type Role string

const (
	RoleUnknown Role = "unknown"
	RolePrimary Role = "primary"
	RoleReplica Role = "replica"
)

const (
	// flipThreshold role changes inside flipWindow mean the node is
	// flapping (failover/failback). The old rule required 5 CONSECUTIVE
	// flips, which is unreachable when checks run every ~10 minutes and
	// a real flap has stable samples between role changes (G1-B21).
	flipThreshold = 2
	flipWindow    = time.Hour
	// Safe mode ends only after stableThreshold consecutive confirmed
	// checks AND safeModeCooldown since the last flip.
	stableThreshold  = 3
	safeModeCooldown = 30 * time.Minute
)

// Monitor tracks the node's role and detects failover flapping.
// It fails closed: an unknown role never allows mutations.
type Monitor struct {
	probe func(context.Context) (bool, error)
	now   func() time.Time
	logFn func(string, string, ...any)
	// identity reads the node's timeline, system identifier and start
	// (nil: the role only); persist holds the identity history.
	identity func(context.Context) (Identity, error)
	persist  persistence

	mu          sync.Mutex
	role        Role // current role; unknown before/after a failed probe
	lastKnown   Role // last successfully observed role (flip detection)
	flips       []time.Time
	stableCount int
	safeMode    bool
	lastChange  time.Time // when a confirmed role (or the node) last changed
	current     Identity  // last observed identity
	identityErr bool      // an identity probe failure was logged
}

// New creates a new HA Monitor probing pg_is_in_recovery() on pool.
func New(pool *pgxpool.Pool, logFn func(string, string, ...any)) *Monitor {
	m := &Monitor{
		now:       time.Now,
		logFn:     logFn,
		role:      RoleUnknown,
		lastKnown: RoleUnknown,
	}
	m.probe = func(ctx context.Context) (bool, error) {
		var inRecovery bool
		err := pool.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery)
		return inRecovery, err
	}
	if pool != nil {
		m.identity = poolIdentity(pool)
	}
	return m
}

// Check probes the node's role and returns true when the node must NOT
// be treated as a writable primary: it is a replica, or its role could
// not be determined. Callers pass the result as "isReplica" to suppress
// mutations, so a failed probe fails closed.
func (m *Monitor) Check(ctx context.Context) bool {
	inRecovery, err := m.probe(ctx)
	if err != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.role = RoleUnknown
		m.stableCount = 0
		m.logFn("WARN", "ha: pg_is_in_recovery() failed; role unknown, "+
			"mutations blocked: %v", err)
		return true
	}
	observed := RolePrimary
	if inRecovery {
		observed = RoleReplica
	}
	id := m.readIdentity(ctx, observed)
	if m.persist.store == nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.observeLocked(observed, id)
		return observed != RolePrimary
	}
	m.checkPersisted(ctx, observed, id)
	return observed != RolePrimary
}

func (m *Monitor) observeLocked(observed Role, id Identity) {
	now := m.now()
	switch {
	case m.lastKnown == RoleUnknown:
		m.logFn("INFO", "ha: initial role detected: %s", observed)
	case observed != m.lastKnown:
		m.recordFlipLocked(now, observed)
	default:
		m.stableCount++
		m.nodeChangeLocked(now, id)
		m.maybeExitSafeModeLocked(now)
	}
	m.lastKnown = observed
	m.role = observed
	m.current = id
}

func (m *Monitor) recordFlipLocked(now time.Time, observed Role) {
	m.lastChange = now
	m.stableCount = 0
	kept := m.flips[:0]
	for _, at := range m.flips {
		if now.Sub(at) < flipWindow {
			kept = append(kept, at)
		}
	}
	m.flips = append(kept, now)
	m.logFn("WARN", "ha: role flip detected: %s -> %s (%d in last %s)",
		m.lastKnown, observed, len(m.flips), flipWindow)
	if len(m.flips) >= flipThreshold && !m.safeMode {
		m.safeMode = true
		m.logFn("WARN", "entering safe mode after %d role flips within %s",
			len(m.flips), flipWindow)
	}
}

func (m *Monitor) maybeExitSafeModeLocked(now time.Time) {
	if !m.safeMode || m.stableCount < stableThreshold {
		return
	}
	if len(m.flips) > 0 && now.Sub(m.flips[len(m.flips)-1]) < safeModeCooldown {
		return
	}
	m.safeMode = false
	m.stableCount = 0
	m.logFn("INFO", "ha: exiting safe mode after %d stable checks and %s cooldown",
		stableThreshold, safeModeCooldown)
}

// Role returns the current role; RoleUnknown before the first successful
// probe and after any failed probe.
func (m *Monitor) Role() Role {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.role
}

// MutationsAllowed reports whether autonomous mutations may run: only on
// a confirmed primary outside safe mode. Executors should gate on this
// rather than on !Check(ctx) alone.
func (m *Monitor) MutationsAllowed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.role == RolePrimary && !m.safeMode
}

// InSafeMode returns true while failover flapping is suspected.
func (m *Monitor) InSafeMode() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.safeMode
}

// LastRoleChange is when a confirmed role last changed (a failover or
// failback); zero before any change. The initial detection and failed
// probes are not changes.
func (m *Monitor) LastRoleChange() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastChange
}
