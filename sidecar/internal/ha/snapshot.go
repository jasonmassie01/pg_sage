package ha

import "time"

// One monitor per database (fleet-04): the database's HA loop checks it
// on a fixed cadence, the executor checks it just before each cycle, and
// every other consumer reads its snapshot instead of probing.

// DefaultCheckInterval is the HA loop's cadence when none is set
// (ha.check_interval_seconds).
const DefaultCheckInterval = time.Minute

// staleCadences is how many cadences a snapshot stays valid; an older
// one means the loop stopped checking, and its role reads as unknown.
const staleCadences = 3

// Status is a monitor's last check, as Snapshot reports it.
type Status struct {
	Role       Role
	SafeMode   bool
	LastChange time.Time // a role or node change (zero: none seen)
	CheckedAt  time.Time // the last check (zero: none yet)
}

// WithCadence sets the cadence the monitor is checked at; zero or
// negative is DefaultCheckInterval.
func (m *Monitor) WithCadence(d time.Duration) *Monitor {
	m.mu.Lock()
	m.cadence = d
	m.mu.Unlock()
	return m
}

// Cadence is the cadence the monitor is checked at.
func (m *Monitor) Cadence() time.Duration {
	if m == nil {
		return DefaultCheckInterval
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cadenceLocked()
}

func (m *Monitor) cadenceLocked() time.Duration {
	if m.cadence <= 0 {
		return DefaultCheckInterval
	}
	return m.cadence
}

// Snapshot reports the last check without probing. Before the first check,
// and once the last one is more than three cadences old, the role is
// unknown, which fails closed. A nil monitor is unknown.
func (m *Monitor) Snapshot() Status {
	if m == nil {
		return Status{Role: RoleUnknown}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := Status{Role: m.role, SafeMode: m.safeMode, LastChange: m.lastChange,
		CheckedAt: m.checkedAt}
	if m.checkedAt.IsZero() || m.now().Sub(m.checkedAt) > staleCadences*m.cadenceLocked() {
		s.Role = RoleUnknown
	}
	return s
}

// Writable reports whether autonomous mutations may run at now: a
// confirmed primary, outside safe mode, at least cooldown after the last
// role or node change (a change dated after now is not over).
func (s Status) Writable(now time.Time, cooldown time.Duration) bool {
	if s.Role != RolePrimary || s.SafeMode {
		return false
	}
	return s.LastChange.IsZero() || now.Sub(s.LastChange) >= cooldown
}

// DefaultFailoverCooldown is how long after a role or node change the
// node stays not writable when none is set (earned autonomy's default,
// sre.autonomy.failover_cooldown_minutes).
const DefaultFailoverCooldown = 30 * time.Minute

// WithFailoverCooldown sets how long after a role or node change every
// consumer of the monitor holds autonomous mutations; zero or negative is
// DefaultFailoverCooldown.
func (m *Monitor) WithFailoverCooldown(d time.Duration) *Monitor {
	m.mu.Lock()
	m.cooldown = d
	m.mu.Unlock()
	return m
}

// FailoverCooldown is the monitor's failover cooldown.
func (m *Monitor) FailoverCooldown() time.Duration {
	if m == nil {
		return DefaultFailoverCooldown
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cooldown <= 0 {
		return DefaultFailoverCooldown
	}
	return m.cooldown
}
