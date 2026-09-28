package ha

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedProbe returns queued pg_is_in_recovery() results in order.
type scriptedProbe struct {
	mu      sync.Mutex
	results []probeResult
}

type probeResult struct {
	inRecovery bool
	err        error
}

func (p *scriptedProbe) probe(context.Context) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.results) == 0 {
		return false, errors.New("script exhausted")
	}
	r := p.results[0]
	p.results = p.results[1:]
	return r.inRecovery, r.err
}

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

type levelLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *levelLog) fn(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, level+" "+fmt.Sprintf(msg, args...))
}

func newScripted(results ...probeResult) (*Monitor, *fakeClock, *levelLog) {
	logs := &levelLog{}
	m := New(nil, logs.fn)
	sp := &scriptedProbe{results: results}
	clock := &fakeClock{now: time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)}
	m.probe = sp.probe
	m.now = clock.Now
	return m, clock, logs
}

// G1-B21: before any successful probe, and after a failed probe, the role
// is unknown and mutations must be blocked (fail closed).
func TestMonitor_UnknownRoleFailsClosed(t *testing.T) {
	m, _, logs := newScripted(
		probeResult{err: errors.New("connection refused")},
		probeResult{inRecovery: false},
		probeResult{err: errors.New("timeout")},
	)
	if m.Role() != RoleUnknown || m.MutationsAllowed() {
		t.Fatalf("new monitor: role=%q mutations=%v, want unknown/blocked",
			m.Role(), m.MutationsAllowed())
	}
	if notPrimary := m.Check(context.Background()); !notPrimary {
		t.Fatal("Check on probe error returned 'primary'; must fail closed")
	}
	if m.Role() != RoleUnknown || m.MutationsAllowed() {
		t.Fatalf("after error: role=%q mutations=%v", m.Role(), m.MutationsAllowed())
	}
	if notPrimary := m.Check(context.Background()); notPrimary {
		t.Fatal("confirmed primary reported as not-primary")
	}
	if m.Role() != RolePrimary || !m.MutationsAllowed() {
		t.Fatalf("confirmed primary: role=%q mutations=%v", m.Role(), m.MutationsAllowed())
	}
	if notPrimary := m.Check(context.Background()); !notPrimary {
		t.Fatal("error after a known primary must not reuse the stale role")
	}
	if m.MutationsAllowed() {
		t.Fatal("mutations allowed while role is unknown")
	}
	if !strings.Contains(strings.Join(logs.entries, "|"), "WARN ") {
		t.Errorf("probe failures not logged at WARN: %q", logs.entries)
	}
}

// G1-B21: a replica never allows mutations.
func TestMonitor_ReplicaBlocksMutations(t *testing.T) {
	m, _, _ := newScripted(probeResult{inRecovery: true})
	if !m.Check(context.Background()) {
		t.Fatal("replica reported as primary")
	}
	if m.Role() != RoleReplica || m.MutationsAllowed() {
		t.Fatalf("replica: role=%q mutations=%v", m.Role(), m.MutationsAllowed())
	}
}

// G1-B21: failover flapping sampled every ~10 minutes (flips separated by
// stable samples) must reach safe mode; the old "N consecutive flips"
// rule was unreachable at that sampling rate.
func TestMonitor_SafeModeReachableAtRealSamplingRate(t *testing.T) {
	m, clock, logs := newScripted(
		probeResult{inRecovery: false}, // primary
		probeResult{inRecovery: false}, // stable
		probeResult{inRecovery: true},  // failover: flip 1
		probeResult{inRecovery: true},  // stable
		probeResult{inRecovery: false}, // failback: flip 2
	)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		m.Check(ctx)
		clock.now = clock.now.Add(10 * time.Minute)
	}
	if !m.InSafeMode() {
		t.Fatal("two failovers within 40 minutes did not enter safe mode")
	}
	if m.MutationsAllowed() {
		t.Fatal("mutations allowed during safe mode")
	}
	if !strings.Contains(strings.Join(logs.entries, "|"), "WARN entering safe mode") {
		t.Errorf("safe-mode entry not logged at WARN: %q", logs.entries)
	}
}

// G1-B21: safe mode exits only after the role has been stable for the
// cooldown AND enough consecutive confirmed samples.
func TestMonitor_SafeModeExitRequiresStableCooldown(t *testing.T) {
	results := []probeResult{{}, {inRecovery: true}, {}}
	for i := 0; i < 10; i++ {
		results = append(results, probeResult{})
	}
	m, clock, _ := newScripted(results...)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		m.Check(ctx)
		clock.now = clock.now.Add(time.Minute)
	}
	if !m.InSafeMode() {
		t.Fatal("expected safe mode after two quick flips")
	}
	for i := 0; i < stableThreshold; i++ {
		m.Check(ctx)
		clock.now = clock.now.Add(time.Minute)
	}
	if !m.InSafeMode() {
		t.Fatal("left safe mode before the cooldown elapsed")
	}
	clock.now = clock.now.Add(safeModeCooldown)
	m.Check(ctx)
	if m.InSafeMode() {
		t.Fatal("still in safe mode after cooldown and stable samples")
	}
	if !m.MutationsAllowed() {
		t.Fatal("stable primary after cooldown should allow mutations")
	}
}

// G1-B21: flips spread far apart (outside the window) are ordinary
// failovers, not flapping.
func TestMonitor_IsolatedFailoversDoNotEnterSafeMode(t *testing.T) {
	m, clock, _ := newScripted(
		probeResult{inRecovery: false},
		probeResult{inRecovery: true},
		probeResult{inRecovery: false},
	)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		m.Check(ctx)
		clock.now = clock.now.Add(flipWindow + time.Minute)
	}
	if m.InSafeMode() {
		t.Fatal("failovers hours apart treated as flapping")
	}
}
