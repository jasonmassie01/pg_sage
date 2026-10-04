package rca

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Dogfood lifeos-1: 143 incidents stayed open for months. An incident not
// re-detected within rca.stale_after_hours resolves as stale; an incident
// whose subject is a backend resolves once that backend is gone. Both
// are recorded as pg_sage resolutions and never touch an operator's.

var staleNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func openIncident(id string, lastSeen time.Time) Incident {
	return Incident{ID: id, DetectedAt: lastSeen.Add(-time.Hour), LastDetectedAt: lastSeen,
		Severity: "warning", RootCause: "Autovacuum falling behind -- dead tuple " +
			"accumulation", SignalIDs: []string{"vacuum_blocked"},
		AffectedObjects: []string{"public.t"}, Source: "deterministic",
		DatabaseName: "orders", OccurrenceCount: 1}
}

func staleEngine(hours int, incs ...Incident) *Engine {
	cfg := testRCACfg()
	cfg.StaleAfterHours = hours
	eng := NewEngine(cfg, func(string, string, ...any) {})
	eng.incidents = append(eng.incidents, incs...)
	return eng
}

func byID(eng *Engine, id string) Incident {
	for _, inc := range eng.incidents {
		if inc.ID == id {
			return inc
		}
	}
	return Incident{}
}

func TestStale_ResolvesOnlyIncidentsOlderThanTheWindow(t *testing.T) {
	eng := staleEngine(24,
		openIncident("old", staleNow.Add(-25*time.Hour)),
		openIncident("edge", staleNow.Add(-24*time.Hour)),
		openIncident("fresh", staleNow.Add(-23*time.Hour)))
	eng.mu.Lock()
	eng.resolveStale(staleNow)
	eng.mu.Unlock()
	old := byID(eng, "old")
	if old.ResolvedAt == nil || old.ResolvedBy != ResolvedByStale ||
		!strings.HasPrefix(old.ResolutionReason, "stale:") ||
		!strings.Contains(old.ResolutionReason, "24h") ||
		!strings.Contains(old.ResolutionReason, "2026-10-01T11:00:00Z") {
		t.Fatalf("old incident = %+v, want resolved as stale with the window and "+
			"the last detection", old)
	}
	for _, id := range []string{"edge", "fresh"} {
		if inc := byID(eng, id); inc.ResolvedAt != nil {
			t.Fatalf("%s resolved at the boundary or inside the window: %+v", id, inc)
		}
	}
}

// Nil/zero: a config without the key uses the 24 h default, never
// "resolve everything".
func TestStale_ZeroConfigUsesTheDefaultWindow(t *testing.T) {
	eng := staleEngine(0, openIncident("a", staleNow.Add(-23*time.Hour)),
		openIncident("b", staleNow.Add(-25*time.Hour)))
	eng.mu.Lock()
	eng.resolveStale(staleNow)
	eng.mu.Unlock()
	if byID(eng, "a").ResolvedAt != nil || byID(eng, "b").ResolvedAt == nil {
		t.Fatalf("incidents = %+v", eng.incidents)
	}
}

// A zero LastDetectedAt (legacy rows) falls back to DetectedAt.
func TestStale_FallsBackToDetectedAt(t *testing.T) {
	inc := openIncident("legacy", staleNow.Add(-48*time.Hour))
	inc.LastDetectedAt = time.Time{}
	eng := staleEngine(24, inc)
	eng.mu.Lock()
	eng.resolveStale(staleNow)
	eng.mu.Unlock()
	if byID(eng, "legacy").ResolvedBy != ResolvedByStale {
		t.Fatalf("legacy incident = %+v", byID(eng, "legacy"))
	}
}

// State transition: an operator resolution is never overwritten.
func TestStale_LeavesOperatorResolutionsAlone(t *testing.T) {
	inc := openIncident("op", staleNow.Add(-72*time.Hour))
	at := staleNow.Add(-time.Hour)
	inc.ResolvedAt, inc.ResolvedBy, inc.ResolutionReason = &at, "user:ops@x", "fixed"
	eng := staleEngine(24, inc)
	eng.mu.Lock()
	eng.resolveStale(staleNow)
	eng.mu.Unlock()
	got := byID(eng, "op")
	if got.ResolvedBy != "user:ops@x" || !got.ResolvedAt.Equal(at) ||
		got.ResolutionReason != "fixed" {
		t.Fatalf("operator resolution changed: %+v", got)
	}
}

// A full cycle applies staleness even during the restart grace period
// (it is measured from stored detections, not from cleared cycles).
func TestStale_AppliesDuringTheGracePeriod(t *testing.T) {
	eng := staleEngine(24, openIncident("old", time.Now().Add(-30*24*time.Hour)))
	if eng.gracePeriodLeft == 0 {
		t.Fatal("test needs a fresh engine inside its grace period")
	}
	eng.Analyze(quietSnapshot(), nil, testConfig(), nil)
	if byID(eng, "old").ResolvedBy != ResolvedByStale {
		t.Fatalf("incident = %+v, want resolved as stale", byID(eng, "old"))
	}
}

func TestBackendSubject_StructuredLegacyAndInvalid(t *testing.T) {
	start := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	vac := func(chain ...ChainLink) *Incident {
		inc := openIncident("x", staleNow)
		inc.CausalChain = chain
		return &inc
	}
	cases := []struct {
		name string
		inc  *Incident
		want BackendRef
		ok   bool
	}{
		{"structured", vac(ChainLink{Signal: "vacuum_blocked",
			Evidence: "PID 7 in state: idle in transaction",
			Blocker:  &BlockerIdentity{PID: 4242, BackendStart: start}}),
			BackendRef{PID: 4242, BackendStart: start}, true},
		{"legacy evidence", vac(ChainLink{Signal: "vacuum_blocked",
			Evidence: "PID 6686 in state: idle in transaction"}),
			BackendRef{PID: 6686}, true},
		{"no holder", vac(ChainLink{Signal: "vacuum_blocked",
			Evidence: "Tables with high dead tuples: [public.t]"}), BackendRef{}, false},
		{"malformed", vac(ChainLink{Signal: "vacuum_blocked",
			Evidence: "PID x6686 in state: idle in transaction"}), BackendRef{}, false},
		{"zero pid", vac(ChainLink{Signal: "vacuum_blocked",
			Evidence: "PID 0 in state: idle in transaction"}), BackendRef{}, false},
		{"overflow", vac(ChainLink{Signal: "vacuum_blocked",
			Evidence: "PID 99999999999999999999 in state: idle in transaction"}),
			BackendRef{}, false},
		{"lock chain is not a backend subject", vac(ChainLink{Signal: "lock_contention",
			Blocker: &BlockerIdentity{PID: 9, BackendStart: start}}), BackendRef{}, false},
		{"empty chain", vac(), BackendRef{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := backendSubject(c.inc)
			if ok != c.ok || got.PID != c.want.PID ||
				!got.BackendStart.Equal(c.want.BackendStart) {
				t.Fatalf("backendSubject = %+v %v, want %+v %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestBackendGone_Decision(t *testing.T) {
	start := time.Date(2026, 9, 1, 8, 0, 0, 123456000, time.UTC)
	other := start.Add(-time.Hour)
	live := map[int]*time.Time{10: &start, 11: nil, 12: &other}
	cases := []struct {
		ref  BackendRef
		gone bool
	}{
		{BackendRef{PID: 99}, true},                       // no such backend
		{BackendRef{PID: 10, BackendStart: start}, false}, // same session
		{BackendRef{PID: 10}, false},                      // legacy: pid alone
		{BackendRef{PID: 11, BackendStart: start}, false}, // start not visible
		{BackendRef{PID: 12, BackendStart: start}, true},  // pid reused
		{BackendRef{PID: 10, BackendStart: start.Add(400 * time.Nanosecond)}, false},
	}
	for _, c := range cases {
		if got := backendGone(c.ref, live); got != c.gone {
			t.Errorf("backendGone(%+v) = %v, want %v", c.ref, got, c.gone)
		}
	}
}

func vacuumSnapshot(pid int, start time.Time) *collector.Snapshot {
	idle := "idle in transaction"
	return &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{TotalBackends: 10, MaxConnections: 100,
			CacheHitRatio: 0.999},
		Tables: []collector.TableStats{{SchemaName: "public", RelName: "t",
			NLiveTup: 1000, NDeadTup: 9000, TableBytes: 64 << 20}},
		Locks: []collector.LockInfo{{PID: pid, State: &idle, BackendStart: &start}},
	}
}

func TestTreeVacuumBlocked_RecordsTheHolderIdentity(t *testing.T) {
	start := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	eng := testEngine()
	incs := eng.Analyze(vacuumSnapshot(4242, start), nil, testConfig(), nil)
	for _, inc := range incs {
		if !strings.Contains(inc.RootCause, "PID 4242") {
			continue
		}
		ref, ok := backendSubject(&inc)
		if !ok || ref.PID != 4242 || !ref.BackendStart.Equal(start) {
			t.Fatalf("holder identity = %+v %v from chain %+v", ref, ok, inc.CausalChain)
		}
		return
	}
	t.Fatalf("no idle-in-transaction incident in %+v", incs)
}

type fakeBackends struct {
	mu    sync.Mutex
	live  map[int]*time.Time
	err   error
	calls [][]int
}

func (f *fakeBackends) lookup(_ context.Context, pids []int) (map[int]*time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]int(nil), pids...))
	return f.live, f.err
}

// Ordering: the gone holder's incident resolves before this cycle's
// detections merge, so the new holder opens its own incident instead of
// refreshing the old one under the old PID.
func TestSubjectGone_ResolvesBeforeNewDetectionsMerge(t *testing.T) {
	start := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	eng := testEngine()
	eng.WithDatabaseName("orders")
	first := eng.Analyze(vacuumSnapshot(111, start), nil, testConfig(), nil)
	if len(first) != 1 {
		t.Fatalf("first cycle = %+v", first)
	}
	later := start.Add(time.Hour)
	fb := &fakeBackends{live: map[int]*time.Time{222: &later}}
	eng.backendLookup = fb.lookup
	got := eng.Analyze(vacuumSnapshot(222, later), nil, testConfig(), nil)
	old := byID(eng, first[0].ID)
	if old.ResolvedBy != ResolvedBySubjectGone ||
		!strings.Contains(old.ResolutionReason, "111") {
		t.Fatalf("old incident = %+v, want resolved: backend 111 is gone", old)
	}
	if len(got) != 1 || got[0].ID == first[0].ID ||
		!strings.Contains(got[0].RootCause, "PID 222") {
		t.Fatalf("active = %+v, want a new incident for PID 222", got)
	}
	if len(fb.calls) != 1 || len(fb.calls[0]) != 1 || fb.calls[0][0] != 111 {
		t.Fatalf("lookups = %v, want one lookup of pid 111", fb.calls)
	}
}

// Error propagation: when backends cannot be listed nothing is resolved
// and the failure is logged with what was attempted.
func TestSubjectGone_LookupFailureResolvesNothing(t *testing.T) {
	var logs []string
	var mu sync.Mutex
	eng := NewEngine(testRCACfg(), func(level, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, level+": "+msg)
	})
	start := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	first := eng.Analyze(vacuumSnapshot(111, start), nil, testConfig(), nil)
	eng.backendLookup = (&fakeBackends{err: errors.New("connection refused")}).lookup
	eng.Analyze(quietSnapshot(), nil, testConfig(), nil)
	if inc := byID(eng, first[0].ID); inc.ResolvedBy == ResolvedBySubjectGone {
		t.Fatalf("resolved without knowing the backends: %+v", inc)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, l := range logs {
		if strings.HasPrefix(l, "warn") && strings.Contains(l, "backend") {
			return
		}
	}
	t.Fatalf("logs = %v, want a warning about the backend lookup", logs)
}

// No lookup when no open incident names a backend (empty input).
func TestSubjectGone_NoSubjectsNoLookup(t *testing.T) {
	eng := staleEngine(24, openIncident("plain", time.Now()))
	fb := &fakeBackends{live: map[int]*time.Time{}}
	eng.backendLookup = fb.lookup
	eng.Analyze(quietSnapshot(), nil, testConfig(), nil)
	if len(fb.calls) != 0 {
		t.Fatalf("lookups = %v, want none", fb.calls)
	}
}

// Concurrent cycles, readers and stale resolution under -race.
func TestStale_ConcurrentCyclesAndReaders(t *testing.T) {
	var incs []Incident
	for i := 0; i < 50; i++ {
		incs = append(incs, openIncident(newUUID(), time.Now().Add(-48*time.Hour)))
	}
	eng := staleEngine(24, incs...)
	eng.backendLookup = (&fakeBackends{live: map[int]*time.Time{}}).lookup
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			eng.Analyze(quietSnapshot(), nil, testConfig(), nil)
		}()
		go func() {
			defer wg.Done()
			_ = eng.ActiveIncidents()
		}()
	}
	wg.Wait()
	if n := len(eng.ActiveIncidents()); n != 0 {
		t.Fatalf("%d stale incidents still open", n)
	}
}
