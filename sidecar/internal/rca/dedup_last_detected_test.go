package rca

import (
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// ---------------------------------------------------------------------------
// Dedup LastDetectedAt behavior
// ---------------------------------------------------------------------------

func TestDedup_NewIncident_LastDetectedAtEqualsDetectedAt(t *testing.T) {
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

	incidents := eng.Analyze(snap, nil, cfg, nil)
	if len(incidents) == 0 {
		t.Fatal("expected at least one incident")
	}

	inc := incidents[0]
	if inc.OccurrenceCount != 1 {
		t.Errorf("OccurrenceCount = %d, want 1 for new incident",
			inc.OccurrenceCount)
	}
	if inc.LastDetectedAt.IsZero() {
		t.Error("LastDetectedAt should not be zero for new incident")
	}
	if !inc.LastDetectedAt.Equal(inc.DetectedAt) {
		t.Errorf("new incident: LastDetectedAt (%v) != DetectedAt (%v)",
			inc.LastDetectedAt, inc.DetectedAt)
	}
}

func TestDedup_Match_DetectedAtUnchanged_LastDetectedAtUpdated(t *testing.T) {
	eng := testEngine()
	cfg := testConfig()

	t0 := time.Now()
	snap1 := &collector.Snapshot{
		CollectedAt: t0,
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	inc1 := eng.Analyze(snap1, nil, cfg, nil)
	if len(inc1) == 0 {
		t.Fatal("expected incident on first cycle")
	}
	originalDetectedAt := inc1[0].DetectedAt
	originalLastDetectedAt := inc1[0].LastDetectedAt

	// Second cycle: 5 minutes later, within dedup window.
	t1 := t0.Add(5 * time.Minute)
	snap2 := &collector.Snapshot{
		CollectedAt: t1,
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

	// DetectedAt should remain the original time.
	if !inc2[0].DetectedAt.Equal(originalDetectedAt) {
		t.Errorf("DetectedAt changed: original=%v, now=%v",
			originalDetectedAt, inc2[0].DetectedAt)
	}

	// LastDetectedAt should be updated to the new detection time.
	if inc2[0].LastDetectedAt.Equal(originalLastDetectedAt) {
		t.Error("LastDetectedAt should have been updated on dedup match")
	}
	// LastDetectedAt should be later than the original.
	if !inc2[0].LastDetectedAt.After(originalDetectedAt) {
		t.Errorf("LastDetectedAt (%v) should be after original DetectedAt (%v)",
			inc2[0].LastDetectedAt, originalDetectedAt)
	}

	// OccurrenceCount should be bumped.
	if inc2[0].OccurrenceCount < 2 {
		t.Errorf("OccurrenceCount = %d, want >= 2",
			inc2[0].OccurrenceCount)
	}
}

func TestDedup_WindowSliding_LastDetectedAtExtendsWindow(t *testing.T) {
	// Scenario: incident at T=0, dedup at T=10m (updates LastDetectedAt),
	// third detection at T=35m.
	//
	// Without LastDetectedAt sliding:
	//   T=35m - T=0 (DetectedAt) = 35m > 30m window => new incident (WRONG)
	//
	// With LastDetectedAt sliding:
	//   T=35m - T=10m (LastDetectedAt) = 25m < 30m window => dedup (CORRECT)

	eng := testEngine()
	cfg := testConfig()

	t0 := time.Now().Add(-40 * time.Minute) // anchor in the past

	// T=0: first detection.
	snap1 := &collector.Snapshot{
		CollectedAt: t0,
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	inc1 := eng.Analyze(snap1, nil, cfg, nil)
	if len(inc1) == 0 {
		t.Fatal("T=0: expected incident")
	}
	firstID := inc1[0].ID

	// T=10m: second detection, updates LastDetectedAt.
	snap2 := &collector.Snapshot{
		CollectedAt: t0.Add(10 * time.Minute),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	inc2 := eng.Analyze(snap2, nil, cfg, nil)
	if len(inc2) == 0 {
		t.Fatal("T=10m: expected incident")
	}
	if inc2[0].ID != firstID {
		t.Errorf("T=10m: expected same ID %q, got %q (should dedup)",
			firstID, inc2[0].ID)
	}

	// T=35m: third detection. 35m - 0m = 35m > 30m window,
	// but 35m - 10m = 25m < 30m window. Should still dedup.
	snap3 := &collector.Snapshot{
		CollectedAt: t0.Add(35 * time.Minute),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	inc3 := eng.Analyze(snap3, nil, cfg, nil)
	if len(inc3) == 0 {
		t.Fatal("T=35m: expected incident")
	}

	// The key assertion: same incident ID means dedup matched.
	if inc3[0].ID != firstID {
		t.Errorf("T=35m: expected same ID %q (sliding window), got %q. "+
			"LastDetectedAt sliding should keep the window open.",
			firstID, inc3[0].ID)
	}

	// OccurrenceCount should be >= 3.
	if inc3[0].OccurrenceCount < 3 {
		t.Errorf("OccurrenceCount = %d, want >= 3",
			inc3[0].OccurrenceCount)
	}

	// DetectedAt should still be the original time.
	if !inc3[0].DetectedAt.Equal(inc1[0].DetectedAt) {
		t.Errorf("DetectedAt shifted: original=%v, now=%v",
			inc1[0].DetectedAt, inc3[0].DetectedAt)
	}
}

func TestDedup_WindowExpiry_NoSliding(t *testing.T) {
	// Verify that without the sliding window fix, a single detection
	// followed by a detection well outside the window creates a new incident.
	// T=0: first detection. T=40m: second detection (> 30m from both
	// DetectedAt and LastDetectedAt since no intermediate update).

	eng := testEngine()
	cfg := testConfig()

	t0 := time.Now().Add(-50 * time.Minute)

	snap1 := &collector.Snapshot{
		CollectedAt: t0,
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	inc1 := eng.Analyze(snap1, nil, cfg, nil)
	if len(inc1) == 0 {
		t.Fatal("T=0: expected incident")
	}
	firstID := inc1[0].ID

	// T=40m: well outside the 30-min window from T=0 with no intermediate.
	snap2 := &collector.Snapshot{
		CollectedAt: t0.Add(40 * time.Minute),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	inc2 := eng.Analyze(snap2, nil, cfg, nil)
	if len(inc2) == 0 {
		t.Fatal("T=40m: expected incident")
	}

	// Should have a new incident (different ID) since 40m > 30m window.
	// Analyze returns all active incidents, so look for one with a
	// different ID than the first.
	foundNew := false
	for _, inc := range inc2 {
		if inc.ID != firstID {
			foundNew = true
			break
		}
	}
	if !foundNew {
		t.Errorf("T=40m: expected NEW incident (40m > 30m window), "+
			"but all %d incidents have original ID %q",
			len(inc2), firstID)
	}
}

func TestDedup_LastDetectedAt_FallsBackToDetectedAt(t *testing.T) {
	// When LastDetectedAt is zero (e.g., old incidents before the fix),
	// dedup should fall back to DetectedAt for the window check.
	eng := testEngine()

	now := time.Now()
	// Manually inject an incident with zero LastDetectedAt.
	eng.incidents = append(eng.incidents, Incident{
		ID:              "legacy-inc-1",
		DetectedAt:      now.Add(-10 * time.Minute),
		LastDetectedAt:  time.Time{}, // zero value
		Severity:        "warning",
		SignalIDs:       []string{"connections_high"},
		Source:          "deterministic",
		OccurrenceCount: 1,
	})

	// New incident with same signal IDs within 30m of DetectedAt.
	newInc := &Incident{
		DetectedAt: now,
		Severity:   "warning",
		SignalIDs:  []string{"connections_high"},
		Source:     "deterministic",
	}
	eng.dedup(newInc)

	// Should have matched the existing incident (10m < 30m window).
	eng.mu.Lock()
	totalIncidents := len(eng.incidents)
	existing := eng.incidents[0]
	eng.mu.Unlock()

	if totalIncidents != 1 {
		t.Errorf("expected 1 incident (deduped), got %d", totalIncidents)
	}
	if existing.OccurrenceCount != 2 {
		t.Errorf("OccurrenceCount = %d, want 2", existing.OccurrenceCount)
	}
	// LastDetectedAt should now be updated.
	if existing.LastDetectedAt.IsZero() {
		t.Error("LastDetectedAt should be updated after dedup match")
	}
	if !existing.LastDetectedAt.Equal(now) {
		t.Errorf("LastDetectedAt = %v, want %v", existing.LastDetectedAt, now)
	}
}
