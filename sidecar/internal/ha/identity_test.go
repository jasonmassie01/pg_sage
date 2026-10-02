package ha

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// Sage SRE follow-ups B: a failover that happened while pg_sage was down
// is invisible to a monitor that only sees the flips it observes, so the
// earned-autonomy failover cooldown (M7, CHECK-40) would not open after a
// restart. The monitor persists the node's last observed identity (role,
// timeline, system identifier, start) and compares it at startup: a
// changed role, timeline or system identifier opens the cooldown. A plain
// restart does not. History that cannot be read fails closed (the
// cooldown opens).

type memStore struct {
	mu      sync.Mutex
	rows    map[string]Persisted
	loadErr error
	saveErr error
	loads   int
	saves   int
}

func newMemStore() *memStore { return &memStore{rows: map[string]Persisted{}} }

func (s *memStore) LoadIdentity(_ context.Context, key string) (Persisted, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	if s.loadErr != nil {
		return Persisted{}, false, s.loadErr
	}
	p, ok := s.rows[key]
	return p, ok, nil
}

func (s *memStore) SaveIdentity(_ context.Context, key string, p Persisted) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saves++
	s.rows[key] = p
	return nil
}

func (s *memStore) row(key string) (Persisted, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.rows[key]
	return p, ok
}

func (s *memStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads, s.saves
}

var (
	bootA   = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	bootB   = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	primary = Identity{Role: RolePrimary, TimelineID: 1, SystemID: "7001", StartedAt: bootA}
)

// scriptedIdentity serves identities in order; the last one repeats.
type scriptedIdentity struct {
	mu  sync.Mutex
	ids []Identity
	err error
}

func (s *scriptedIdentity) probe(context.Context) (Identity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return Identity{}, s.err
	}
	id := s.ids[0]
	if len(s.ids) > 1 {
		s.ids = s.ids[1:]
	}
	return id, nil
}

// newIdentityMonitor is a monitor over a scripted node: each identity is
// one check (its role drives pg_is_in_recovery()).
func newIdentityMonitor(store IdentityStore, key string, ids ...Identity) (*Monitor,
	*fakeClock, *levelLog) {
	var results []probeResult
	for _, id := range ids {
		results = append(results, probeResult{inRecovery: id.Role == RoleReplica})
	}
	m, clock, logs := newScripted(results...)
	m.identity = (&scriptedIdentity{ids: ids}).probe
	if store != nil {
		m.WithIdentityStore(store, key)
	}
	return m, clock, logs
}

func with(id Identity, f func(*Identity)) Identity {
	f(&id)
	return id
}

func TestIdentity_FirstStartPersistsWithoutAChange(t *testing.T) {
	store := newMemStore()
	m, clock, _ := newIdentityMonitor(store, "db:1", primary)
	m.Check(context.Background())
	if !m.LastRoleChange().IsZero() || m.InSafeMode() || !m.MutationsAllowed() {
		t.Fatalf("first start: change %v safe %v", m.LastRoleChange(), m.InSafeMode())
	}
	p, ok := store.row("db:1")
	if !ok || p.Identity != primary || !p.LastChange.IsZero() || !p.ObservedAt.Equal(clock.now) {
		t.Fatalf("persisted = %+v (%v)", p, ok)
	}
	if got := m.Identity(); got != primary {
		t.Fatalf("Identity() = %+v", got)
	}
}

// A restart of PostgreSQL (or of pg_sage) with the same role, timeline
// and system identifier is not a failover.
func TestIdentity_RestartIsNotAFailover(t *testing.T) {
	store := newMemStore()
	store.rows["db:1"] = Persisted{Identity: primary, ObservedAt: bootA}
	restarted := with(primary, func(i *Identity) { i.StartedAt = bootB })
	m, _, _ := newIdentityMonitor(store, "db:1", restarted)
	m.Check(context.Background())
	if !m.LastRoleChange().IsZero() {
		t.Fatalf("a restart opened the failover cooldown at %v", m.LastRoleChange())
	}
	if p, _ := store.row("db:1"); !p.Identity.StartedAt.Equal(bootB) {
		t.Fatalf("the new start time was not persisted: %+v", p)
	}
}

func TestIdentity_ChangeWhileDownOpensTheCooldown(t *testing.T) {
	cases := map[string]struct {
		before, now Identity
		flip        bool // a role change counts toward flapping
	}{
		"replica promoted": {with(primary, func(i *Identity) { i.Role = RoleReplica }),
			with(primary, func(i *Identity) { i.TimelineID = 2 }), true},
		"primary demoted": {primary, with(primary, func(i *Identity) {
			i.Role = RoleReplica
		}), true},
		"timeline changed": {primary, with(primary, func(i *Identity) {
			i.TimelineID, i.StartedAt = 2, bootB
		}), false},
		"another cluster": {primary, with(primary, func(i *Identity) {
			i.SystemID, i.StartedAt = "9999", bootB
		}), false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			store := newMemStore()
			store.rows["db:1"] = Persisted{Identity: c.before, ObservedAt: bootA}
			m, clock, logs := newIdentityMonitor(store, "db:1", c.now)
			m.Check(context.Background())
			if got := m.LastRoleChange(); !got.Equal(clock.now) {
				t.Fatalf("LastRoleChange = %v, want the startup check at %v", got, clock.now)
			}
			if m.InSafeMode() {
				t.Fatal("one change while down is not flapping")
			}
			if p, _ := store.row("db:1"); !p.LastChange.Equal(clock.now) ||
				p.Identity != c.now {
				t.Fatalf("persisted = %+v", p)
			}
			if got := len(m.flips); (got == 1) != c.flip {
				t.Fatalf("flips = %d, want a flip only for a role change (%v)", got, c.flip)
			}
			if !logged(logs, "WARN", "while pg_sage was not running") {
				t.Fatalf("logs = %v, want a warning about the change while down",
					logs.entries)
			}
		})
	}
}

// A change pg_sage saw shortly before it restarted keeps its cooldown.
func TestIdentity_RecentChangeSurvivesARestart(t *testing.T) {
	store := newMemStore()
	m0, clock, _ := newIdentityMonitor(store, "db:1", primary)
	changed := clock.now.Add(-10 * time.Minute)
	store.rows["db:1"] = Persisted{Identity: primary, LastChange: changed,
		ObservedAt: clock.now.Add(-time.Minute)}
	m0.Check(context.Background())
	if got := m0.LastRoleChange(); !got.Equal(changed) {
		t.Fatalf("LastRoleChange = %v, want the persisted %v", got, changed)
	}
	if p, _ := store.row("db:1"); !p.LastChange.Equal(changed) {
		t.Fatalf("the persisted change moved: %+v", p)
	}
}

// Fields unknown on either side (an older row, an identity probe that
// failed) are not compared; the role still is.
func TestIdentity_UnknownFieldsAreNotCompared(t *testing.T) {
	store := newMemStore()
	store.rows["db:1"] = Persisted{Identity: Identity{Role: RolePrimary}, ObservedAt: bootA}
	m, _, _ := newIdentityMonitor(store, "db:1",
		with(primary, func(i *Identity) { i.TimelineID = 7 }))
	m.Check(context.Background())
	if !m.LastRoleChange().IsZero() {
		t.Fatalf("an unknown persisted timeline opened the cooldown: %v", m.LastRoleChange())
	}
	store2 := newMemStore()
	store2.rows["db:1"] = Persisted{Identity: primary, ObservedAt: bootA}
	m2, _, logs := newScripted(probeResult{}, probeResult{})
	m2.identity = (&scriptedIdentity{err: errors.New("pg_control_system denied")}).probe
	m2.WithIdentityStore(store2, "db:1")
	m2.Check(context.Background())
	m2.Check(context.Background())
	if !m2.LastRoleChange().IsZero() || m2.Role() != RolePrimary {
		t.Fatalf("identity probe failure: change %v role %s", m2.LastRoleChange(), m2.Role())
	}
	if n := countLogged(logs, "WARN", "identity"); n != 1 {
		t.Fatalf("identity failure logged %d times, want once: %v", n, logs.entries)
	}
}

// Without its history the monitor cannot rule a failover out: the
// cooldown opens (fail closed), once.
func TestIdentity_UnreadableHistoryFailsClosed(t *testing.T) {
	store := newMemStore()
	store.loadErr = errors.New("connection refused")
	m, clock, logs := newIdentityMonitor(store, "db:1", primary, primary)
	m.Check(context.Background())
	if got := m.LastRoleChange(); !got.Equal(clock.now) {
		t.Fatalf("unreadable history: LastRoleChange = %v, want %v", got, clock.now)
	}
	clock.now = clock.now.Add(time.Minute)
	m.Check(context.Background())
	if loads, _ := store.counts(); loads != 1 {
		t.Fatalf("history loaded %d times, want once", loads)
	}
	if !logged(logs, "WARN", "connection refused") {
		t.Fatalf("logs = %v, want the load failure", logs.entries)
	}
}

func TestIdentity_SaveFailureIsRetried(t *testing.T) {
	store := newMemStore()
	store.saveErr = errors.New("disk full")
	m, _, logs := newIdentityMonitor(store, "db:1", primary, primary)
	m.Check(context.Background())
	if _, ok := store.row("db:1"); ok {
		t.Fatal("a failed save stored a row")
	}
	if !logged(logs, "WARN", "disk full") {
		t.Fatalf("logs = %v, want the save failure", logs.entries)
	}
	store.mu.Lock()
	store.saveErr = nil
	store.mu.Unlock()
	m.Check(context.Background())
	if p, ok := store.row("db:1"); !ok || p.Identity != primary {
		t.Fatalf("the save was not retried: %+v (%v)", p, ok)
	}
}

// Stable checks write nothing after the first.
func TestIdentity_NoWriteWhenNothingChanged(t *testing.T) {
	store := newMemStore()
	m, clock, _ := newIdentityMonitor(store, "db:1", primary)
	for i := 0; i < 5; i++ {
		m.Check(context.Background())
		clock.now = clock.now.Add(time.Minute)
	}
	if loads, saves := store.counts(); loads != 1 || saves != 1 {
		t.Fatalf("loads %d saves %d, want 1 and 1", loads, saves)
	}
}

// A timeline or system identifier change seen while running (a failover
// behind a DSN that follows the primary) opens the cooldown without
// counting toward flapping: the role did not flip.
func TestIdentity_ChangeWhileRunning(t *testing.T) {
	store := newMemStore()
	tl2 := with(primary, func(i *Identity) { i.TimelineID = 2 })
	tl3 := with(primary, func(i *Identity) { i.TimelineID = 3 })
	m, clock, logs := newIdentityMonitor(store, "db:1", primary, tl2, tl2, tl3)
	ctx := context.Background()
	m.Check(ctx)
	clock.now = clock.now.Add(time.Minute)
	at := clock.now
	m.Check(ctx)
	if !m.LastRoleChange().Equal(at) || m.InSafeMode() || !m.MutationsAllowed() {
		t.Fatalf("timeline change: change %v safe %v", m.LastRoleChange(), m.InSafeMode())
	}
	clock.now = clock.now.Add(time.Minute)
	m.Check(ctx)
	clock.now = clock.now.Add(time.Minute)
	m.Check(ctx)
	if m.InSafeMode() || len(m.flips) != 0 {
		t.Fatalf("timeline changes counted as role flips: %d", len(m.flips))
	}
	if p, _ := store.row("db:1"); p.Identity.TimelineID != 3 || !p.LastChange.Equal(clock.now) {
		t.Fatalf("persisted = %+v", p)
	}
	if !logged(logs, "WARN", "timeline") {
		t.Fatalf("logs = %v, want the timeline change", logs.entries)
	}
}

// A role probe that fails first: nothing is loaded or compared until the
// role is known.
func TestIdentity_HistoryWaitsForAKnownRole(t *testing.T) {
	store := newMemStore()
	store.rows["db:1"] = Persisted{Identity: with(primary, func(i *Identity) {
		i.Role = RoleReplica
	}), ObservedAt: bootA}
	m, clock, _ := newScripted(probeResult{err: errors.New("timeout")}, probeResult{})
	m.identity = (&scriptedIdentity{ids: []Identity{primary}}).probe
	m.WithIdentityStore(store, "db:1")
	m.Check(context.Background())
	if loads, _ := store.counts(); loads != 0 || !m.LastRoleChange().IsZero() {
		t.Fatalf("history read before the role was known (loads %d)", loads)
	}
	m.Check(context.Background())
	if loads, _ := store.counts(); loads != 1 || !m.LastRoleChange().Equal(clock.now) {
		t.Fatalf("loads %d change %v", loads, m.LastRoleChange())
	}
}

func TestIdentity_KeysAreIsolated(t *testing.T) {
	store := newMemStore()
	store.rows["db:1"] = Persisted{Identity: with(primary, func(i *Identity) {
		i.Role = RoleReplica
	}), ObservedAt: bootA}
	store.rows["db:2"] = Persisted{Identity: primary, ObservedAt: bootA}
	a, _, _ := newIdentityMonitor(store, "db:1", primary)
	b, _, _ := newIdentityMonitor(store, "db:2", primary)
	a.Check(context.Background())
	b.Check(context.Background())
	if a.LastRoleChange().IsZero() || !b.LastRoleChange().IsZero() {
		t.Fatalf("db:1 change %v, db:2 change %v", a.LastRoleChange(), b.LastRoleChange())
	}
}

func TestIdentity_EmptyKeyDisablesPersistence(t *testing.T) {
	store := newMemStore()
	m, _, logs := newIdentityMonitor(store, "", primary)
	m.Check(context.Background())
	if loads, saves := store.counts(); loads != 0 || saves != 0 {
		t.Fatalf("an empty key used the store (%d loads, %d saves)", loads, saves)
	}
	if !logged(logs, "WARN", "key") {
		t.Fatalf("logs = %v, want a warning about the empty key", logs.entries)
	}
}

// Concurrent first checks load the history once and record one change
// (run with -race).
func TestIdentity_ConcurrentChecksLoadOnce(t *testing.T) {
	store := newMemStore()
	store.rows["db:1"] = Persisted{Identity: with(primary, func(i *Identity) {
		i.Role = RoleReplica
	}), ObservedAt: bootA}
	var results []probeResult
	for i := 0; i < 16; i++ {
		results = append(results, probeResult{})
	}
	m, _, _ := newScripted(results...)
	m.identity = (&scriptedIdentity{ids: []Identity{primary}}).probe
	m.WithIdentityStore(store, "db:1")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Check(context.Background())
		}()
	}
	wg.Wait()
	if loads, _ := store.counts(); loads != 1 {
		t.Fatalf("history loaded %d times", loads)
	}
	if m.LastRoleChange().IsZero() || len(m.flips) != 1 {
		t.Fatalf("change %v flips %d, want one change", m.LastRoleChange(), len(m.flips))
	}
}

func logged(l *levelLog, level, substr string) bool { return countLogged(l, level, substr) > 0 }

func countLogged(l *levelLog, level, substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.entries {
		if strings.HasPrefix(e, level+" ") && strings.Contains(e, substr) {
			n++
		}
	}
	return n
}
