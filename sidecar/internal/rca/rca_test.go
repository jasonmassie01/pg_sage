package rca

import (
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testRCACfg() *config.RCAConfig {
	return &config.RCAConfig{
		Enabled:                  true,
		DedupWindowMinutes:       30,
		EscalationCycles:         5,
		ResolutionCycles:         2,
		ConnectionSaturationPct:  80,
		ReplicationLagThresholdS: 30,
		WALSpikeMultiplier:       2.0,
	}
}

func testConfig() *config.Config {
	return &config.Config{
		RCA: *testRCACfg(),
		Analyzer: config.AnalyzerConfig{
			IdleInTxTimeoutMinutes: 5,
			CacheHitRatioWarning:   0.95,
			TableBloatDeadTuplePct: 10,
		},
	}
}

func testEngine() *Engine {
	return NewEngine(testRCACfg(), func(string, string, ...any) {})
}

// quietSnapshot returns a snapshot with no signals firing.
func quietSnapshot() *collector.Snapshot {
	return &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  10,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
}

// burnGracePeriod runs enough cycles to exhaust the grace period.
// Uses hot snapshots so the signal keeps firing and clearCount stays
// at 0. After the initial Analyze (which decremented gracePeriodLeft
// from 3 to 2), we need 4 more hot cycles to be safe.
func burnGracePeriod(eng *Engine, cfg *config.Config) {
	hot := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	for i := 0; i < 4; i++ {
		eng.Analyze(hot, nil, cfg, nil)
	}
}

// ---------------------------------------------------------------------------
// TestNewEngine
// ---------------------------------------------------------------------------

func TestNewEngine(t *testing.T) {
	rcaCfg := testRCACfg()
	eng := NewEngine(rcaCfg, func(string, string, ...any) {})

	if eng.cfg != rcaCfg {
		t.Error("cfg pointer mismatch")
	}
	if len(eng.incidents) != 0 {
		t.Errorf("incidents should be empty, got %d", len(eng.incidents))
	}
	if len(eng.clearCounts) != 0 {
		t.Errorf("clearCounts should be empty, got %d", len(eng.clearCounts))
	}
	if eng.cycleCount != 0 {
		t.Errorf("cycleCount = %d, want 0", eng.cycleCount)
	}
	// gracePeriodLeft = ResolutionCycles + 1 = 3
	if eng.gracePeriodLeft != 3 {
		t.Errorf("gracePeriodLeft = %d, want 3", eng.gracePeriodLeft)
	}
	if eng.logFn == nil {
		t.Error("logFn should not be nil")
	}
}

// ---------------------------------------------------------------------------
// TestAnalyze_NoSignals
// ---------------------------------------------------------------------------

func TestAnalyze_NoSignals(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()
	snap := quietSnapshot()

	incidents := eng.Analyze(snap, snap, cfg, nil)
	if len(incidents) != 0 {
		t.Errorf("expected no incidents from quiet snapshot, got %d",
			len(incidents))
	}
}

// ---------------------------------------------------------------------------
// TestAnalyze_ConnectionsHigh_IdleInTx
// ---------------------------------------------------------------------------

func TestAnalyze_ConnectionsHigh_IdleInTx(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	snap := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:     85,
			MaxConnections:    100,
			IdleInTransaction: 40,
			CacheHitRatio:     0.999,
		},
		ConfigData: &collector.ConfigSnapshot{
			ConnectionStates: []collector.ConnectionState{
				{
					State:              "idle in transaction",
					Count:              40,
					AvgDurationSeconds: 600.0, // 10 minutes > 5 min threshold
				},
			},
		},
	}

	incidents := eng.Analyze(snap, nil, cfg, nil)
	if len(incidents) == 0 {
		t.Fatal("expected at least one incident")
	}

	// Find the connections_high incident — it should be the idle-in-tx
	// combined tree result.
	var found *Incident
	for i := range incidents {
		for _, sid := range incidents[i].SignalIDs {
			if sid == "connections_high" {
				found = &incidents[i]
				break
			}
		}
		if found != nil {
			break
		}
	}
	if found == nil {
		t.Fatal("no incident with connections_high signal")
	}

	// The tree should fire the idle-in-tx branch.
	if len(found.SignalIDs) < 2 {
		t.Fatalf("expected 2 signal IDs, got %v", found.SignalIDs)
	}
	hasIdleSig := false
	for _, sid := range found.SignalIDs {
		if sid == "idle_in_tx_elevated" {
			hasIdleSig = true
		}
	}
	if !hasIdleSig {
		t.Errorf("expected idle_in_tx_elevated in SignalIDs: %v",
			found.SignalIDs)
	}
	if found.RootCause == "" {
		t.Error("RootCause should not be empty")
	}
	if len(found.CausalChain) < 2 {
		t.Errorf("CausalChain should have >= 2 links, got %d",
			len(found.CausalChain))
	}
}

// ---------------------------------------------------------------------------
// TestAnalyze_CacheHitDrop_WorkingSet
// ---------------------------------------------------------------------------

func TestAnalyze_CacheHitDrop_WorkingSet(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	// No specific query spike: previous and current have similar reads.
	prev := &collector.Snapshot{
		CollectedAt: time.Now().Add(-time.Minute),
		System: collector.SystemStats{
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
		Queries: []collector.QueryStats{
			{QueryID: 1, SharedBlksRead: 100, Query: "SELECT 1"},
		},
	}
	curr := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			MaxConnections: 100,
			CacheHitRatio:  0.80, // below 0.95
		},
		Queries: []collector.QueryStats{
			{QueryID: 1, SharedBlksRead: 150, Query: "SELECT 1"}, // 1.5x, not 10x
		},
	}

	incidents := eng.Analyze(curr, prev, cfg, nil)

	var cacheInc *Incident
	for i := range incidents {
		for _, sid := range incidents[i].SignalIDs {
			if sid == "cache_hit_ratio_drop" {
				cacheInc = &incidents[i]
				break
			}
		}
		if cacheInc != nil {
			break
		}
	}
	if cacheInc == nil {
		t.Fatal("expected cache_hit_ratio_drop incident")
	}

	// Should be the "working set exceeds shared_buffers" branch.
	wantSubstr := "Working set"
	if cacheInc.RootCause == "" {
		t.Fatal("RootCause should not be empty")
	}
	found := false
	if len(cacheInc.RootCause) > 0 {
		for _, substr := range []string{"Working set", "working set",
			"shared_buffers"} {
			if contains(cacheInc.RootCause, substr) {
				found = true
				break
			}
		}
	}
	if !found {
		t.Errorf("RootCause should mention working set/shared_buffers, "+
			"got %q (looking for %q)", cacheInc.RootCause, wantSubstr)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// TestAnalyze_WALSpike
// ---------------------------------------------------------------------------

func TestAnalyze_WALSpike(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	prev := &collector.Snapshot{
		CollectedAt: time.Now().Add(-time.Minute),
		System: collector.SystemStats{
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
		Queries: []collector.QueryStats{
			{QueryID: 1, WALBytes: 1000},
		},
	}
	curr := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
		Queries: []collector.QueryStats{
			{QueryID: 1, WALBytes: 3000}, // 3x spike
		},
	}

	incidents := eng.Analyze(curr, prev, cfg, nil)

	var walInc *Incident
	for i := range incidents {
		for _, sid := range incidents[i].SignalIDs {
			if sid == "wal_growth_spike" {
				walInc = &incidents[i]
				break
			}
		}
		if walInc != nil {
			break
		}
	}
	if walInc == nil {
		t.Fatal("expected wal_growth_spike incident")
	}

	wantSubstr := "WAL"
	if !contains(walInc.RootCause, wantSubstr) {
		t.Errorf("RootCause should mention WAL, got %q", walInc.RootCause)
	}
	if walInc.Severity != "warning" {
		t.Errorf("Severity = %q, want warning", walInc.Severity)
	}
}

// ---------------------------------------------------------------------------
// TestDedup_SameIncidentUpdates
// ---------------------------------------------------------------------------

func TestDedup_SameIncidentUpdates(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	snap := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}

	// First cycle: creates incident.
	inc1 := eng.Analyze(snap, nil, cfg, nil)
	if len(inc1) == 0 {
		t.Fatal("expected incident on first cycle")
	}
	firstID := inc1[0].ID

	// Second cycle: same signals within dedup window -> should update,
	// not create new.
	snap2 := &collector.Snapshot{
		CollectedAt: time.Now().Add(time.Minute), // within 30-min window
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	inc2 := eng.Analyze(snap2, nil, cfg, nil)
	if len(inc2) == 0 {
		t.Fatal("expected incident on second cycle")
	}

	// Should be same incident ID (deduped).
	if inc2[0].ID != firstID {
		t.Errorf("expected same incident ID %q, got %q", firstID, inc2[0].ID)
	}
	if inc2[0].OccurrenceCount < 2 {
		t.Errorf("OccurrenceCount = %d, want >= 2",
			inc2[0].OccurrenceCount)
	}
}

// ---------------------------------------------------------------------------
// TestDedup_DifferentSourcesNotMerged
// ---------------------------------------------------------------------------

func TestDedup_DifferentSourcesNotMerged(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	// Snapshot that fires connections_high.
	connSnap := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	eng.Analyze(connSnap, nil, cfg, nil)

	// Snapshot that fires cache_hit_drop (different source).
	cacheSnap := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  10,
			MaxConnections: 100,
			CacheHitRatio:  0.80,
		},
	}
	active := eng.Analyze(cacheSnap, nil, cfg, nil)

	// Should have at least 2 different incidents: one from connections_high
	// and one from cache_hit_ratio_drop. The connections_high one may have
	// auto-resolved if we're past grace period, but within first few cycles
	// it should still be active.
	// Actually: connections_high did not fire in the second cycle (50% < 80%),
	// but the incident is still active. The cache one is new.
	if len(active) < 2 {
		// The first incident (connections_high) should still be active
		// because auto-resolve needs ResolutionCycles=2 consecutive clear
		// cycles, and grace period is still active.
		sources := make(map[string]bool)
		for _, inc := range active {
			sources[inc.Source] = true
		}
		t.Errorf("expected at least 2 incidents from different sources, "+
			"got %d: sources=%v", len(active), sources)
	}
}

// ---------------------------------------------------------------------------
// TestDedup_OutsideWindowCreatesNew
// ---------------------------------------------------------------------------

func TestDedup_OutsideWindowCreatesNew(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	// First incident.
	snap1 := &collector.Snapshot{
		CollectedAt: time.Now().Add(-time.Hour), // old
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	eng.Analyze(snap1, nil, cfg, nil)
	firstCount := len(eng.incidents)

	// Same signals but outside 30-minute dedup window.
	snap2 := &collector.Snapshot{
		CollectedAt: time.Now(), // 1 hour later > 30 min window
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	eng.Analyze(snap2, nil, cfg, nil)

	// Should have created a new incident.
	if len(eng.incidents) <= firstCount {
		t.Errorf("expected new incident outside dedup window; "+
			"had %d, now %d", firstCount, len(eng.incidents))
	}
}

// ---------------------------------------------------------------------------
// TestAutoResolve_SignalsClear
// ---------------------------------------------------------------------------

func TestAutoResolve_SignalsClear(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	// Create an incident.
	hotSnap := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	eng.Analyze(hotSnap, nil, cfg, nil)

	// Burn grace period with quiet cycles.
	burnGracePeriod(eng, cfg)

	// Verify incident is still active (grace period just ended, but we
	// need ResolutionCycles=2 consecutive clear cycles).
	active := eng.ActiveIncidents()
	if len(active) == 0 {
		t.Fatal("incident should still be active during grace burn")
	}

	// Now run 2 clear cycles (ResolutionCycles=2).
	quiet := quietSnapshot()
	eng.Analyze(quiet, quiet, cfg, nil)
	eng.Analyze(quiet, quiet, cfg, nil)

	active = eng.ActiveIncidents()
	if len(active) != 0 {
		t.Errorf("expected all incidents resolved after 2 clear cycles, "+
			"got %d active", len(active))
	}
}

// ---------------------------------------------------------------------------
// TestAutoResolve_GracePeriod
// ---------------------------------------------------------------------------

func TestAutoResolve_GracePeriod(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	// Create an incident.
	hotSnap := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	eng.Analyze(hotSnap, nil, cfg, nil)

	// Grace period is ResolutionCycles+1 = 3 cycles.
	// Run quiet cycles but don't exceed grace period.
	quiet := quietSnapshot()
	eng.Analyze(quiet, quiet, cfg, nil) // cycle 2, grace=2
	eng.Analyze(quiet, quiet, cfg, nil) // cycle 3, grace=1

	// During grace period, incidents should not auto-resolve even with
	// clear signals.
	active := eng.ActiveIncidents()
	if len(active) == 0 {
		t.Error("incident should not be resolved during grace period")
	}
}

// ---------------------------------------------------------------------------
// TestAutoResolve_SignalRefires
// ---------------------------------------------------------------------------

func TestAutoResolve_SignalRefires(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	hotSnap := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	eng.Analyze(hotSnap, nil, cfg, nil)

	// Burn grace period.
	burnGracePeriod(eng, cfg)

	// 1 clear cycle.
	quiet := quietSnapshot()
	eng.Analyze(quiet, quiet, cfg, nil)

	// Signal refires — should reset clear counter.
	hotSnap2 := &collector.Snapshot{
		CollectedAt: time.Now().Add(time.Second), // within dedup window
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	eng.Analyze(hotSnap2, nil, cfg, nil)

	// Another clear cycle — only 1 since refire, not enough.
	eng.Analyze(quiet, quiet, cfg, nil)

	active := eng.ActiveIncidents()
	if len(active) == 0 {
		t.Error("incident should NOT be resolved — clear counter was " +
			"reset by refire")
	}

	// One more clear cycle (2 total since refire) should resolve.
	eng.Analyze(quiet, quiet, cfg, nil)

	active = eng.ActiveIncidents()
	if len(active) != 0 {
		t.Errorf("incident should be resolved after 2 clear cycles " +
			"post-refire")
	}
}

// ---------------------------------------------------------------------------
// TestEscalate_WarningToCritical
// ---------------------------------------------------------------------------

func TestEscalate_WarningToCritical(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	// Create a warning-level incident and keep it firing for
	// EscalationCycles=5 occurrences.
	for i := 0; i < 5; i++ {
		snap := &collector.Snapshot{
			CollectedAt: time.Now().Add(time.Duration(i) * time.Second),
			System: collector.SystemStats{
				TotalBackends:  85, // 85% > 80% threshold => warning (< 90%)
				MaxConnections: 100,
				CacheHitRatio:  0.999,
			},
		}
		eng.Analyze(snap, nil, cfg, nil)
	}

	active := eng.ActiveIncidents()
	if len(active) == 0 {
		t.Fatal("expected active incidents")
	}

	// Find the connections_high incident.
	var connInc *Incident
	for i := range active {
		for _, sid := range active[i].SignalIDs {
			if sid == "connections_high" {
				connInc = &active[i]
				break
			}
		}
		if connInc != nil {
			break
		}
	}
	if connInc == nil {
		t.Fatal("no connections_high incident found")
	}

	if connInc.Severity != "critical" {
		t.Errorf("Severity = %q, want critical after %d cycles",
			connInc.Severity, cfg.RCA.EscalationCycles)
	}
	if connInc.EscalatedAt == nil {
		t.Error("EscalatedAt should be set after escalation")
	}
}

// ---------------------------------------------------------------------------
// TestEscalate_SkipsResolved
// ---------------------------------------------------------------------------

func TestEscalate_SkipsResolved(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	// Create and then resolve an incident.
	hotSnap := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	eng.Analyze(hotSnap, nil, cfg, nil)

	// Bypass grace period directly (burnGracePeriod would fire hot
	// signals that increment OccurrenceCount and trigger escalation).
	eng.mu.Lock()
	eng.gracePeriodLeft = 0
	eng.mu.Unlock()

	// Resolve via 2 quiet cycles.
	quiet := quietSnapshot()
	eng.Analyze(quiet, quiet, cfg, nil)
	eng.Analyze(quiet, quiet, cfg, nil)

	// Verify resolved.
	if len(eng.ActiveIncidents()) != 0 {
		t.Fatal("incident should be resolved")
	}

	// The resolved incident should not have been escalated.
	eng.mu.Lock()
	for _, inc := range eng.incidents {
		if inc.ResolvedAt != nil && inc.EscalatedAt != nil {
			t.Errorf("resolved incident %q should not have been escalated",
				inc.ID)
		}
	}
	eng.mu.Unlock()
}

// ---------------------------------------------------------------------------
// TestActiveIncidents_ExcludesResolved
// ---------------------------------------------------------------------------

func TestActiveIncidents_ExcludesResolved(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	// Create an incident.
	hotSnap := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	eng.Analyze(hotSnap, nil, cfg, nil)

	active := eng.ActiveIncidents()
	if len(active) == 0 {
		t.Fatal("expected at least one active incident")
	}

	// Resolve by burning grace period + clear cycles.
	burnGracePeriod(eng, cfg)
	quiet := quietSnapshot()
	eng.Analyze(quiet, quiet, cfg, nil)
	eng.Analyze(quiet, quiet, cfg, nil)

	active = eng.ActiveIncidents()
	if len(active) != 0 {
		t.Errorf("expected no active incidents after resolution, got %d",
			len(active))
	}

	// But the incident should still exist in the internal slice.
	eng.mu.Lock()
	total := len(eng.incidents)
	eng.mu.Unlock()
	if total == 0 {
		t.Error("internal incidents slice should not be empty; " +
			"resolved incidents are kept")
	}
}

// ---------------------------------------------------------------------------
// TestConcurrentAccess
// ---------------------------------------------------------------------------

func TestConcurrentAccess(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	var wg sync.WaitGroup
	const goroutines = 10

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			snap := &collector.Snapshot{
				CollectedAt: time.Now().Add(
					time.Duration(n) * time.Millisecond),
				System: collector.SystemStats{
					TotalBackends:  85,
					MaxConnections: 100,
					CacheHitRatio:  0.999,
				},
			}
			// Mix of Analyze and ActiveIncidents calls.
			_ = eng.Analyze(snap, nil, cfg, nil)
			_ = eng.ActiveIncidents()
		}(i)
	}

	wg.Wait()

	// If we get here without a race condition panic, the mutex is working.
	// Also verify the engine is in a consistent state.
	active := eng.ActiveIncidents()
	if active == nil {
		t.Error("ActiveIncidents should return non-nil slice")
	}

	eng.mu.Lock()
	totalIncidents := len(eng.incidents)
	eng.mu.Unlock()
	if totalIncidents == 0 {
		t.Error("expected at least one incident from concurrent Analyze calls")
	}
}

// ---------------------------------------------------------------------------
// TestAnalyze_ConnectionsHigh_GradualGrowth
// ---------------------------------------------------------------------------

func TestAnalyze_ConnectionsHigh_GradualGrowth(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	// No idle-in-tx signal, no churn spike => gradual growth branch.
	prev := &collector.Snapshot{
		CollectedAt: time.Now().Add(-time.Minute),
		System: collector.SystemStats{
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
		ConfigData: &collector.ConfigSnapshot{
			ConnectionChurn: 10,
		},
	}
	curr := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
		ConfigData: &collector.ConfigSnapshot{
			ConnectionChurn: 12, // not 2x of 10
		},
	}

	incidents := eng.Analyze(curr, prev, cfg, nil)

	var connInc *Incident
	for i := range incidents {
		for _, sid := range incidents[i].SignalIDs {
			if sid == "connections_high" {
				connInc = &incidents[i]
				break
			}
		}
		if connInc != nil {
			break
		}
	}
	if connInc == nil {
		t.Fatal("expected connections_high incident")
	}
	if !contains(connInc.RootCause, "Gradual") &&
		!contains(connInc.RootCause, "gradual") {
		t.Errorf("expected gradual growth root cause, got %q",
			connInc.RootCause)
	}
}

// ---------------------------------------------------------------------------
// TestAnalyze_ConnectionsHigh_ConnectionStorm
// ---------------------------------------------------------------------------

func TestAnalyze_ConnectionsHigh_ConnectionStorm(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	// Churn doubled => connection storm branch.
	prev := &collector.Snapshot{
		CollectedAt: time.Now().Add(-time.Minute),
		System: collector.SystemStats{
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
		ConfigData: &collector.ConfigSnapshot{
			ConnectionChurn: 10,
		},
	}
	curr := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
		ConfigData: &collector.ConfigSnapshot{
			ConnectionChurn: 25, // > 10*2
		},
	}

	incidents := eng.Analyze(curr, prev, cfg, nil)

	var connInc *Incident
	for i := range incidents {
		for _, sid := range incidents[i].SignalIDs {
			if sid == "connections_high" {
				connInc = &incidents[i]
				break
			}
		}
		if connInc != nil {
			break
		}
	}
	if connInc == nil {
		t.Fatal("expected connections_high incident")
	}
	if !contains(connInc.RootCause, "storm") &&
		!contains(connInc.RootCause, "Storm") {
		t.Errorf("expected connection storm root cause, got %q",
			connInc.RootCause)
	}
}

// ---------------------------------------------------------------------------
// TestAnalyze_CacheHitDrop_SpecificQuery
// ---------------------------------------------------------------------------

func TestAnalyze_CacheHitDrop_SpecificQuery(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	prev := &collector.Snapshot{
		CollectedAt: time.Now().Add(-time.Minute),
		System: collector.SystemStats{
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
		Queries: []collector.QueryStats{
			{QueryID: 42, SharedBlksRead: 100, Query: "SELECT big"},
		},
	}
	curr := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			MaxConnections: 100,
			CacheHitRatio:  0.80,
		},
		Queries: []collector.QueryStats{
			{QueryID: 42, SharedBlksRead: 5000, Query: "SELECT big"},
			// 5000 > 100*10 => specific query branch
		},
	}

	incidents := eng.Analyze(curr, prev, cfg, nil)

	var cacheInc *Incident
	for i := range incidents {
		for _, sid := range incidents[i].SignalIDs {
			if sid == "cache_hit_ratio_drop" {
				cacheInc = &incidents[i]
				break
			}
		}
		if cacheInc != nil {
			break
		}
	}
	if cacheInc == nil {
		t.Fatal("expected cache_hit_ratio_drop incident")
	}
	if !contains(cacheInc.RootCause, "42") {
		t.Errorf("expected query ID 42 in root cause, got %q",
			cacheInc.RootCause)
	}
}

// ---------------------------------------------------------------------------
// TestAnalyze_VacuumBlocked_XminHolder
// ---------------------------------------------------------------------------

func TestAnalyze_VacuumBlocked_XminHolder(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()
	idleState := "idle in transaction"

	curr := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
		Tables: []collector.TableStats{
			{
				SchemaName: "public",
				RelName:    "orders",
				NLiveTup:   1000,
				NDeadTup:   200,
			},
		},
		Locks: []collector.LockInfo{
			{PID: 1234, State: &idleState},
		},
	}

	incidents := eng.Analyze(curr, nil, cfg, nil)

	var vacInc *Incident
	for i := range incidents {
		for _, sid := range incidents[i].SignalIDs {
			if sid == "vacuum_blocked" {
				vacInc = &incidents[i]
				break
			}
		}
		if vacInc != nil {
			break
		}
	}
	if vacInc == nil {
		t.Fatal("expected vacuum_blocked incident")
	}
	if !contains(vacInc.RootCause, "1234") {
		t.Errorf("expected PID 1234 in root cause, got %q",
			vacInc.RootCause)
	}
	if vacInc.RecommendedSQL == "" {
		t.Error("expected RecommendedSQL for xmin holder branch")
	}
}

// ---------------------------------------------------------------------------
// TestAnalyze_LockContention
// ---------------------------------------------------------------------------

func TestAnalyze_LockContention(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	snap := quietSnapshot()
	findings := []analyzer.Finding{
		{
			Category: "lock_chain",
			Severity: "warning",
			Detail:   map[string]any{"total_blocked": 5},
		},
	}

	incidents := eng.Analyze(snap, nil, cfg, findings)

	var lockInc *Incident
	for i := range incidents {
		for _, sid := range incidents[i].SignalIDs {
			if sid == "lock_contention" {
				lockInc = &incidents[i]
				break
			}
		}
		if lockInc != nil {
			break
		}
	}
	if lockInc == nil {
		t.Fatal("expected lock_contention incident")
	}
	if !contains(lockInc.RootCause, "Lock") &&
		!contains(lockInc.RootCause, "lock") {
		t.Errorf("expected lock-related root cause, got %q",
			lockInc.RootCause)
	}
}

// ---------------------------------------------------------------------------
// TestDedup_DefaultWindow
// ---------------------------------------------------------------------------

func TestDedup_DefaultWindow(t *testing.T) {
	// DedupWindowMinutes=0 should default to 30.
	rcaCfg := testRCACfg()
	rcaCfg.DedupWindowMinutes = 0
	eng := NewEngine(rcaCfg, func(string, string, ...any) {})
	cfg := testConfig()
	cfg.RCA.DedupWindowMinutes = 0

	snap := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	eng.Analyze(snap, nil, cfg, nil)

	snap2 := &collector.Snapshot{
		CollectedAt: time.Now().Add(5 * time.Minute),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	eng.Analyze(snap2, nil, cfg, nil)

	// Should have deduped (within default 30-min window).
	eng.mu.Lock()
	count := len(eng.incidents)
	eng.mu.Unlock()
	if count != 1 {
		t.Errorf("expected 1 incident (deduped), got %d", count)
	}
}

// ---------------------------------------------------------------------------
// TestEscalate_DefaultCycles
// ---------------------------------------------------------------------------

func TestEscalate_DefaultCycles(t *testing.T) {
	// EscalationCycles=0 should default to 5.
	rcaCfg := testRCACfg()
	rcaCfg.EscalationCycles = 0
	eng := NewEngine(rcaCfg, func(string, string, ...any) {})
	cfg := testConfig()
	cfg.RCA.EscalationCycles = 0

	for i := 0; i < 5; i++ {
		snap := &collector.Snapshot{
			CollectedAt: time.Now().Add(time.Duration(i) * time.Second),
			System: collector.SystemStats{
				TotalBackends:  85,
				MaxConnections: 100,
				CacheHitRatio:  0.999,
			},
		}
		eng.Analyze(snap, nil, cfg, nil)
	}

	active := eng.ActiveIncidents()
	escalated := false
	for _, inc := range active {
		if inc.Severity == "critical" && inc.EscalatedAt != nil {
			escalated = true
		}
	}
	if !escalated {
		t.Error("expected escalation after 5 cycles with default config")
	}
}

// ---------------------------------------------------------------------------
// TestAutoResolve_DefaultResolutionCycles
// ---------------------------------------------------------------------------

func TestAutoResolve_DefaultResolutionCycles(t *testing.T) {
	// ResolutionCycles=0 should default to 2.
	rcaCfg := testRCACfg()
	rcaCfg.ResolutionCycles = 0
	eng := NewEngine(rcaCfg, func(string, string, ...any) {})
	cfg := testConfig()
	cfg.RCA.ResolutionCycles = 0

	hotSnap := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	eng.Analyze(hotSnap, nil, cfg, nil)

	// Grace period = ResolutionCycles + 1 = 0 + 1 = 1; but the default
	// needed for auto-resolve is 2. Burn grace period and then 2 clear.
	quiet := quietSnapshot()
	for i := 0; i < 4; i++ {
		eng.Analyze(quiet, quiet, cfg, nil)
	}

	active := eng.ActiveIncidents()
	if len(active) != 0 {
		t.Errorf("expected all resolved with default resolution cycles, "+
			"got %d active", len(active))
	}
}

// ---------------------------------------------------------------------------
// TestCycleCount_Increments
// ---------------------------------------------------------------------------

func TestCycleCount_Increments(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()
	quiet := quietSnapshot()

	eng.Analyze(quiet, quiet, cfg, nil)
	eng.Analyze(quiet, quiet, cfg, nil)
	eng.Analyze(quiet, quiet, cfg, nil)

	eng.mu.Lock()
	count := eng.cycleCount
	eng.mu.Unlock()

	if count != 3 {
		t.Errorf("cycleCount = %d, want 3", count)
	}
}
