package ha

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Node identity (Sage SRE follow-ups B). A monitor only sees the role
// flips it observes, so a failover while pg_sage was not running would
// not open the earned-autonomy failover cooldown (M7, CHECK-40). With an
// IdentityStore the monitor persists the node's last observed identity
// and compares it at startup: a changed role, timeline or system
// identifier is a failover (or another cluster) and opens the cooldown;
// a plain restart is not. A change it saw before it stopped keeps its
// cooldown. While running, a timeline or system identifier change with
// the same role (a failover behind an address that follows the primary)
// also opens the cooldown, without counting toward flapping.

// Identity is what the monitor knows of the node. Zero fields are
// unknown.
type Identity struct {
	Role       Role
	TimelineID int64
	SystemID   string
	StartedAt  time.Time
}

// Persisted is a monitor's history: the last observed identity and when
// the node last changed.
type Persisted struct {
	Identity   Identity
	LastChange time.Time
	ObservedAt time.Time
}

// IdentityStore keeps each monitor's history under a stable key.
type IdentityStore interface {
	LoadIdentity(ctx context.Context, key string) (Persisted, bool, error)
	SaveIdentity(ctx context.Context, key string, p Persisted) error
}

// persistence is a monitor's history state; mu serializes the first
// load, the comparison and the saves.
type persistence struct {
	mu     sync.Mutex
	store  IdentityStore
	key    string
	loaded bool
	saved  Persisted
	synced bool // saved is what the store holds
}

// identitySQL reads the node's identity besides its role. On a primary
// the timeline is the WAL insert timeline (current right after a
// promotion); a standby reports its last checkpoint's.
const identitySQL = `SELECT
    (SELECT c.system_identifier::text FROM pg_catalog.pg_control_system() c),
    CASE WHEN pg_catalog.pg_is_in_recovery()
         THEN (SELECT c.timeline_id::int8 FROM pg_catalog.pg_control_checkpoint() c)
         ELSE ('x' || pg_catalog.substr(pg_catalog.pg_walfile_name(
                  pg_catalog.pg_current_wal_lsn()), 1, 8))::bit(32)::int8
    END,
    pg_catalog.pg_postmaster_start_time()`

func poolIdentity(pool *pgxpool.Pool) func(context.Context) (Identity, error) {
	return func(ctx context.Context) (Identity, error) {
		var id Identity
		err := pool.QueryRow(ctx, identitySQL).Scan(&id.SystemID, &id.TimelineID,
			&id.StartedAt)
		return id, err
	}
}

// WithIdentityStore persists the monitor's history in store under key (a
// stable name of the monitored database). An empty key or a nil store
// leaves persistence off.
func (m *Monitor) WithIdentityStore(store IdentityStore, key string) *Monitor {
	if store == nil || strings.TrimSpace(key) == "" {
		m.logFn("WARN", "ha: identity history not persisted: no store or an empty key; "+
			"a failover while pg_sage is down will not be seen")
		return m
	}
	m.persist.store, m.persist.key = store, key
	return m
}

// Identity is the node's last observed identity.
func (m *Monitor) Identity() Identity {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current
}

// readIdentity probes the identity; a failure leaves only the role known
// and is logged once.
func (m *Monitor) readIdentity(ctx context.Context, role Role) Identity {
	if m.identity == nil {
		return Identity{Role: role}
	}
	id, err := m.identity(ctx)
	if err != nil {
		m.mu.Lock()
		if !m.identityErr {
			m.identityErr = true
			m.logFn("WARN", "ha: node identity (timeline, system identifier) unavailable; "+
				"only the role is compared: %v", err)
		}
		m.mu.Unlock()
		return Identity{Role: role}
	}
	id.Role = role
	return id
}

// checkPersisted observes with the history: the first successful check
// loads and compares it, every check saves what changed.
func (m *Monitor) checkPersisted(ctx context.Context, observed Role, id Identity) {
	p := &m.persist
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.loaded {
		prev, found, err := p.store.LoadIdentity(ctx, p.key)
		p.loaded = true
		m.mu.Lock()
		m.reconcileLocked(id, prev, found, err)
		m.mu.Unlock()
		if err == nil && found {
			p.saved, p.synced = prev, true
		}
	}
	m.mu.Lock()
	m.observeLocked(observed, id)
	snap := Persisted{Identity: m.current, LastChange: m.lastChange, ObservedAt: m.now()}
	m.mu.Unlock()
	if p.synced && sameHistory(p.saved, snap) {
		return
	}
	if err := p.store.SaveIdentity(ctx, p.key, snap); err != nil {
		m.logFn("WARN", "ha: saving the node identity failed (retried next check): %v", err)
		return
	}
	p.saved, p.synced = snap, true
}

func sameHistory(a, b Persisted) bool {
	return a.Identity.Role == b.Identity.Role && a.Identity.TimelineID == b.Identity.TimelineID &&
		a.Identity.SystemID == b.Identity.SystemID &&
		a.Identity.StartedAt.Equal(b.Identity.StartedAt) && a.LastChange.Equal(b.LastChange)
}

// reconcileLocked compares the persisted history with the first
// observation. Unreadable history fails closed: the cooldown opens.
func (m *Monitor) reconcileLocked(now Identity, prev Persisted, found bool, err error) {
	at := m.now()
	switch {
	case err != nil:
		m.lastChange = at
		m.logFn("WARN", "ha: the node's last identity could not be read (%v); a failover "+
			"while pg_sage was not running cannot be ruled out: failover cooldown opened", err)
		return
	case !found:
		return
	}
	if !prev.LastChange.IsZero() {
		m.lastChange = prev.LastChange
	}
	// The next observation compares against the persisted role, so a role
	// change is recorded as a flip.
	m.lastKnown = prev.Identity.Role
	change := nodeDiff(prev.Identity, now)
	if change == "" && prev.Identity.Role == now.Role {
		return
	}
	if change == "" {
		change = fmt.Sprintf("role %s -> %s", prev.Identity.Role, now.Role)
	}
	m.lastChange = at
	m.logFn("WARN", "ha: the node changed while pg_sage was not running (%s): "+
		"failover cooldown opened", change)
}

// nodeChangeLocked opens the cooldown when the node's timeline or system
// identifier changed under the same role.
func (m *Monitor) nodeChangeLocked(at time.Time, id Identity) {
	if change := nodeDiff(m.current, id); change != "" {
		m.lastChange = at
		m.logFn("WARN", "ha: node %s with the same role (a failover behind the same "+
			"address?): failover cooldown opened", change)
	}
}

// nodeDiff describes a timeline or system identifier change between two
// identities, comparing only fields known on both sides.
func nodeDiff(a, b Identity) string {
	switch {
	case a.SystemID != "" && b.SystemID != "" && a.SystemID != b.SystemID:
		return fmt.Sprintf("system identifier %s -> %s", a.SystemID, b.SystemID)
	case a.TimelineID > 0 && b.TimelineID > 0 && a.TimelineID != b.TimelineID:
		return fmt.Sprintf("timeline %d -> %d", a.TimelineID, b.TimelineID)
	}
	return ""
}
