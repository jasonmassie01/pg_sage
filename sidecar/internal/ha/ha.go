package ha

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	flipThreshold  = 5 // consecutive flips before entering safe mode
	stableThreshold = 5 // consecutive stable checks before exiting safe mode
)

// Monitor tracks PostgreSQL primary/replica role and detects role flips.
type Monitor struct {
	pool        *pgxpool.Pool
	logFn       func(string, string, ...any)

	mu          sync.Mutex
	wasReplica  bool
	flipCount   int
	stableCount int
	safeMode    bool
	initialized bool
}

// New creates a new HA Monitor.
func New(pool *pgxpool.Pool, logFn func(string, string, ...any)) *Monitor {
	return &Monitor{
		pool:  pool,
		logFn: logFn,
	}
}

// Check queries pg_is_in_recovery() and detects role flips between calls.
// After 5 consecutive flips it enters safe mode (no autonomous actions).
// After 5 consecutive stable checks it exits safe mode.
func (m *Monitor) Check(ctx context.Context) bool {
	var inRecovery bool
	err := m.pool.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery)

	m.mu.Lock()
	defer m.mu.Unlock()

	if err != nil {
		// Read wasReplica under the lock — concurrent Check calls write
		// it (C3). On a transient error, hold the last known role.
		m.logFn("ha", "pg_is_in_recovery() failed: %v", err)
		return m.wasReplica
	}

	if !m.initialized {
		m.wasReplica = inRecovery
		m.initialized = true
		role := "primary"
		if inRecovery {
			role = "replica"
		}
		m.logFn("ha", "initial role detected: %s", role)
		return inRecovery
	}

	if inRecovery != m.wasReplica {
		m.flipCount++
		m.stableCount = 0

		oldRole := "primary"
		newRole := "replica"
		if m.wasReplica {
			oldRole = "replica"
			newRole = "primary"
		}
		m.logFn("ha",
			"role flip detected: %s -> %s (flip #%d)",
			oldRole, newRole, m.flipCount,
		)

		if m.flipCount >= flipThreshold && !m.safeMode {
			m.safeMode = true
			m.logFn("ha",
				"entering safe mode after %d consecutive flips",
				m.flipCount,
			)
		}

		m.wasReplica = inRecovery
	} else {
		m.stableCount++
		m.flipCount = 0

		if m.safeMode && m.stableCount >= stableThreshold {
			m.safeMode = false
			m.stableCount = 0
			m.logFn("ha",
				"exiting safe mode after %d consecutive stable checks",
				stableThreshold,
			)
		}
	}

	return inRecovery
}

// IsReplica returns the last known replica status.
func (m *Monitor) IsReplica() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.wasReplica
}

// InSafeMode returns true if excessive role flips have been detected.
func (m *Monitor) InSafeMode() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.safeMode
}
