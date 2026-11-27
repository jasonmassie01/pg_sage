package ha

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fleet-04: one monitor per database, read through Snapshot by every
// consumer. A snapshot reports the last check; one older than three
// cadences reads as unknown, which fails closed.

// countingProbe answers the role probe from a script; the last answer
// repeats. It counts calls.
type countingProbe struct {
	mu      sync.Mutex
	answers []probeAnswer
	calls   atomic.Int64
}

type probeAnswer struct {
	inRecovery bool
	err        error
}

func (s *countingProbe) probe(context.Context) (bool, error) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.answers[0]
	if len(s.answers) > 1 {
		s.answers = s.answers[1:]
	}
	return a.inRecovery, a.err
}

func primaryAnswer() probeAnswer { return probeAnswer{} }
func replicaAnswer() probeAnswer { return probeAnswer{inRecovery: true} }

// manualClock is a settable clock for the snapshot's age.
type manualClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *manualClock) Set(at time.Time) {
	c.mu.Lock()
	c.at = at
	c.mu.Unlock()
}

func scriptedMonitor(t *testing.T, answers ...probeAnswer) (*Monitor, *countingProbe,
	*manualClock) {
	t.Helper()
	sp := &countingProbe{answers: answers}
	clock := &manualClock{at: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)}
	m := NewFromProbe(sp.probe, noopLog)
	m.now = clock.Now
	return m, sp, clock
}

func TestSnapshot_BeforeTheFirstCheckIsUnknown(t *testing.T) {
	m, sp, _ := scriptedMonitor(t, primaryAnswer())
	s := m.Snapshot()
	if s.Role != RoleUnknown || s.SafeMode || !s.CheckedAt.IsZero() ||
		!s.LastChange.IsZero() {
		t.Fatalf("unchecked snapshot = %+v, want unknown and zero times", s)
	}
	if s.Writable(time.Now(), 30*time.Minute) {
		t.Fatal("an unchecked monitor reads as writable")
	}
	if sp.calls.Load() != 0 {
		t.Fatalf("Snapshot probed %d times, want 0", sp.calls.Load())
	}
}

func TestSnapshot_ReportsTheLastCheckWithoutProbing(t *testing.T) {
	m, sp, clock := scriptedMonitor(t, primaryAnswer())
	if m.Check(context.Background()) {
		t.Fatal("a primary probe reported not writable")
	}
	checked := clock.Now()
	clock.Set(checked.Add(30 * time.Second))
	for i := 0; i < 10; i++ {
		s := m.Snapshot()
		if s.Role != RolePrimary || s.SafeMode || !s.CheckedAt.Equal(checked) {
			t.Fatalf("snapshot = %+v, want primary checked at %v", s, checked)
		}
	}
	if got := sp.calls.Load(); got != 1 {
		t.Fatalf("one check and ten snapshots probed %d times, want 1", got)
	}
}

// Boundary: exactly three cadences old is still valid; one nanosecond
// more reads as unknown.
func TestSnapshot_StaleAfterThreeCadences(t *testing.T) {
	m, _, clock := scriptedMonitor(t, primaryAnswer())
	m.WithCadence(20 * time.Second)
	m.Check(context.Background())
	checked := clock.Now()
	clock.Set(checked.Add(60 * time.Second))
	if s := m.Snapshot(); s.Role != RolePrimary {
		t.Fatalf("snapshot exactly 3 cadences old = %q, want primary", s.Role)
	}
	clock.Set(checked.Add(60*time.Second + time.Nanosecond))
	s := m.Snapshot()
	if s.Role != RoleUnknown {
		t.Fatalf("snapshot past 3 cadences = %q, want unknown", s.Role)
	}
	if !s.CheckedAt.Equal(checked) {
		t.Fatalf("stale snapshot CheckedAt = %v, want the last check %v", s.CheckedAt, checked)
	}
	if s.Writable(clock.Now(), time.Minute) {
		t.Fatal("a stale snapshot reads as writable")
	}
}

// A cadence that is not set (or not positive) is the default, 60 s.
func TestSnapshot_DefaultCadence(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		m, _, clock := scriptedMonitor(t, primaryAnswer())
		if d != 0 {
			m.WithCadence(d)
		}
		if got := m.Cadence(); got != DefaultCheckInterval || DefaultCheckInterval != time.Minute {
			t.Fatalf("cadence after WithCadence(%v) = %v, want 60s", d, got)
		}
		m.Check(context.Background())
		checked := clock.Now()
		clock.Set(checked.Add(3 * time.Minute))
		if s := m.Snapshot(); s.Role != RolePrimary {
			t.Fatalf("default cadence: 180s old = %q, want primary", s.Role)
		}
		clock.Set(checked.Add(3*time.Minute + time.Second))
		if s := m.Snapshot(); s.Role != RoleUnknown {
			t.Fatalf("default cadence: 181s old = %q, want unknown", s.Role)
		}
	}
}

// Error propagation: a failing probe records the check and leaves the
// role unknown, so every reader sees not writable on that tick.
func TestSnapshot_FailedProbeIsUnknownAtThatCheck(t *testing.T) {
	m, _, clock := scriptedMonitor(t, primaryAnswer(),
		probeAnswer{err: errors.New("connection refused")})
	m.Check(context.Background())
	clock.Set(clock.Now().Add(time.Second))
	if !m.Check(context.Background()) {
		t.Fatal("a failed probe did not fail closed")
	}
	s := m.Snapshot()
	if s.Role != RoleUnknown || !s.CheckedAt.Equal(clock.Now()) {
		t.Fatalf("after a failed probe snapshot = %+v, want unknown checked at %v",
			s, clock.Now())
	}
	if s.Writable(clock.Now(), time.Minute) {
		t.Fatal("a failed probe reads as writable")
	}
}

// State transitions: two flips inside the window put the one monitor in
// safe mode, and the snapshot carries it with the last change.
func TestSnapshot_CarriesSafeModeAndTheLastChange(t *testing.T) {
	m, _, clock := scriptedMonitor(t, primaryAnswer(), replicaAnswer(), primaryAnswer())
	ctx := context.Background()
	m.Check(ctx)
	clock.Set(clock.Now().Add(time.Minute))
	m.Check(ctx)
	clock.Set(clock.Now().Add(time.Minute))
	m.Check(ctx)
	s := m.Snapshot()
	if s.Role != RolePrimary || !s.SafeMode || !s.LastChange.Equal(clock.Now()) {
		t.Fatalf("after two flips snapshot = %+v, want primary in safe mode changed at %v",
			s, clock.Now())
	}
	if s.Writable(clock.Now().Add(time.Hour), time.Minute) {
		t.Fatal("a primary in safe mode reads as writable")
	}
}

func TestSnapshot_NilMonitorIsUnknown(t *testing.T) {
	var m *Monitor
	s := m.Snapshot()
	if s.Role != RoleUnknown || s.Writable(time.Now(), 0) {
		t.Fatalf("nil monitor snapshot = %+v, want unknown and not writable", s)
	}
	if m.Cadence() != DefaultCheckInterval {
		t.Fatalf("nil monitor cadence = %v, want the default", m.Cadence())
	}
}

// Invalid input: a monitor without a probe fails closed instead of
// panicking.
func TestNewFromProbe_NilProbeFailsClosed(t *testing.T) {
	m := NewFromProbe(nil, noopLog)
	if !m.Check(context.Background()) {
		t.Fatal("a monitor without a probe reported writable")
	}
	if s := m.Snapshot(); s.Role != RoleUnknown || s.CheckedAt.IsZero() {
		t.Fatalf("no-probe snapshot = %+v, want unknown with the check recorded", s)
	}
}

func TestStatusWritable(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	cooldown := 30 * time.Minute
	cases := []struct {
		name string
		s    Status
		want bool
	}{
		{"primary", Status{Role: RolePrimary, CheckedAt: now}, true},
		{"replica", Status{Role: RoleReplica, CheckedAt: now}, false},
		{"unknown", Status{Role: RoleUnknown, CheckedAt: now}, false},
		{"empty role", Status{CheckedAt: now}, false},
		{"safe mode", Status{Role: RolePrimary, SafeMode: true, CheckedAt: now}, false},
		{"inside cooldown", Status{Role: RolePrimary, CheckedAt: now,
			LastChange: now.Add(-cooldown + time.Nanosecond)}, false},
		{"cooldown just over", Status{Role: RolePrimary, CheckedAt: now,
			LastChange: now.Add(-cooldown)}, true},
		{"old change", Status{Role: RolePrimary, CheckedAt: now,
			LastChange: now.Add(-2 * time.Hour)}, true},
		{"change in the future", Status{Role: RolePrimary, CheckedAt: now,
			LastChange: now.Add(time.Minute)}, false},
	}
	for _, tc := range cases {
		if got := tc.s.Writable(now, cooldown); got != tc.want {
			t.Errorf("%s: Writable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Concurrent access: 50 readers take snapshots while checks flip the
// role. Every snapshot is a consistent role (never a torn value).
func TestSnapshot_ConcurrentReadersDuringCheck(t *testing.T) {
	answers := make([]probeAnswer, 0, 200)
	for i := 0; i < 200; i++ {
		answers = append(answers, probeAnswer{inRecovery: i%2 == 1})
	}
	m, sp, _ := scriptedMonitor(t, answers...)
	ctx := context.Background()
	stop := make(chan struct{})
	var readers sync.WaitGroup
	var bad atomic.Int64
	for i := 0; i < 50; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				switch m.Snapshot().Role {
				case RolePrimary, RoleReplica, RoleUnknown:
				default:
					bad.Add(1)
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		m.Check(ctx)
	}
	close(stop)
	readers.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d snapshots carried an invalid role", bad.Load())
	}
	if sp.calls.Load() != 200 {
		t.Fatalf("200 checks probed %d times", sp.calls.Load())
	}
	if !m.Snapshot().SafeMode {
		t.Fatal("199 flips did not put the monitor in safe mode")
	}
}

// Concurrent checks are serialized: two checks never interleave their
// observation, so a slow probe cannot record an older answer last.
func TestCheck_ConcurrentChecksAreSerialized(t *testing.T) {
	var inFlight, maxInFlight atomic.Int64
	m := NewFromProbe(func(context.Context) (bool, error) {
		n := inFlight.Add(1)
		for {
			old := maxInFlight.Load()
			if n <= old || maxInFlight.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		inFlight.Add(-1)
		return false, nil
	}, noopLog)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Check(context.Background())
		}()
	}
	wg.Wait()
	if got := maxInFlight.Load(); got != 1 {
		t.Fatalf("%d probes ran at once, want 1", got)
	}
}

// The failover cooldown consumers apply: the default (30 m) when unset,
// zero or negative, or on a nil monitor; otherwise what was set.
func TestFailoverCooldown_DefaultsAndOverride(t *testing.T) {
	m, _, _ := scriptedMonitor(t, primaryAnswer())
	if m.FailoverCooldown() != DefaultFailoverCooldown ||
		DefaultFailoverCooldown != 30*time.Minute {
		t.Fatalf("unset cooldown = %v, want 30m", m.FailoverCooldown())
	}
	for _, d := range []time.Duration{0, -time.Minute} {
		if got := m.WithFailoverCooldown(d).FailoverCooldown(); got != DefaultFailoverCooldown {
			t.Fatalf("cooldown after WithFailoverCooldown(%v) = %v, want the default", d, got)
		}
	}
	if got := m.WithFailoverCooldown(45 * time.Minute).FailoverCooldown(); got != 45*time.Minute {
		t.Fatalf("cooldown = %v, want 45m", got)
	}
	var none *Monitor
	if none.FailoverCooldown() != DefaultFailoverCooldown {
		t.Fatal("a nil monitor has no default cooldown")
	}
}
