package rca

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/notify"
)

// Post-test audit additions: supersede after a detection gap, memory
// bounds, external deletion, dispatch failure, persistence failure and
// boundary inputs.

func TestDedup_OpenMatchAfterGapIsSupersededAndLinked(t *testing.T) {
	eng := testEngine()
	t0 := time.Now().Add(-2 * time.Hour)
	first := buildIncident(t0, "warning", []string{"connections_high"},
		"saturated", nil, []string{"pg_stat_activity"}, "", "safe")
	eng.dedup(&first)
	oldID := eng.incidents[0].ID

	again := buildIncident(time.Now(), "warning",
		[]string{"connections_high"}, "saturated", nil,
		[]string{"pg_stat_activity"}, "", "safe")
	eng.dedup(&again)

	if len(eng.incidents) != 2 {
		t.Fatalf("incidents = %d, want 2 (superseded + new)",
			len(eng.incidents))
	}
	old := eng.incidents[0]
	if old.ResolvedAt == nil || old.ResolvedBy != ResolvedBySupersede {
		t.Errorf("old incident not superseded: %+v", old)
	}
	if eng.incidents[1].PreviousIncidentID != oldID {
		t.Errorf("new PreviousIncidentID = %q, want %q",
			eng.incidents[1].PreviousIncidentID, oldID)
	}
}

func TestDedup_WithinWindowBumpsSameIncident(t *testing.T) {
	eng := testEngine()
	now := time.Now()
	a := buildIncident(now.Add(-29*time.Minute), "warning",
		[]string{"vacuum_blocked"}, "x", nil, []string{"t"}, "", "safe")
	eng.dedup(&a)
	b := buildIncident(now, "critical",
		[]string{"vacuum_blocked"}, "x", nil, []string{"t"}, "", "safe")
	eng.dedup(&b)
	if len(eng.incidents) != 1 || eng.incidents[0].OccurrenceCount != 2 ||
		eng.incidents[0].Severity != "critical" {
		t.Fatalf("incidents = %+v, want one bumped critical", eng.incidents)
	}
}

func TestDedup_CapWarnsOnce(t *testing.T) {
	var mu sync.Mutex
	warns := 0
	eng := NewEngine(testRCACfg(), func(level, msg string, _ ...any) {
		mu.Lock()
		defer mu.Unlock()
		if level == "warn" && strings.Contains(msg, "open incidents") {
			warns++
		}
	})
	for i := 0; i < maxTrackedIncidents+10; i++ {
		inc := buildIncident(time.Now(), "warning", []string{"s"}, "x",
			nil, []string{fmt.Sprintf("o%d", i)}, "", "safe")
		eng.dedup(&inc)
	}
	if warns != 1 {
		t.Errorf("cap warnings = %d, want 1", warns)
	}
	if n := eng.activeCount(); n != maxTrackedIncidents {
		t.Errorf("active = %d, want %d", n, maxTrackedIncidents)
	}
}

func TestTrimResolvedOverflow_DropsOldestResolved(t *testing.T) {
	eng := testEngine()
	now := time.Now()
	for i := 0; i < 2*maxTrackedIncidents+50; i++ {
		inc := Incident{ID: fmt.Sprintf("r%d", i), ResolvedAt: &now}
		eng.incidents = append(eng.incidents, inc)
		eng.trackFor(inc.ID)
	}
	eng.incidents = append(eng.incidents, Incident{ID: "open"})
	eng.trimResolvedOverflow()
	if len(eng.incidents) != 2*maxTrackedIncidents {
		t.Fatalf("len = %d, want %d", len(eng.incidents),
			2*maxTrackedIncidents)
	}
	if eng.incidents[0].ID != "r51" {
		t.Errorf("first kept = %s, want r51 (oldest dropped)",
			eng.incidents[0].ID)
	}
	if eng.incidents[len(eng.incidents)-1].ID != "open" {
		t.Error("open incident was dropped")
	}
	if _, ok := eng.track["r0"]; ok {
		t.Error("track state of dropped incident retained")
	}
}

func TestPlanTier2_LowConfidenceSignalsAreCandidates(t *testing.T) {
	sigs := testSignals(3)
	tier1 := []Incident{
		{SignalIDs: []string{"sig_a"}, Confidence: 0.85},
		{SignalIDs: []string{"sig_b"}, Confidence: 0.5},
	}
	got := signalIDs(tier2Candidates(sigs, tier1))
	if !stringsEqual(got, []string{"sig_b", "sig_c"}) {
		t.Errorf("candidates = %v, want [sig_b sig_c]", got)
	}
}

func TestNormalizeCacheHitRatio_Invalid(t *testing.T) {
	for _, v := range []float64{-1, 0, 150} {
		if _, ok := normalizeCacheHitRatio(v); ok {
			t.Errorf("normalizeCacheHitRatio(%v) ok = true, want false", v)
		}
	}
}

func TestHydrateAndResolve_RejectBadInput(t *testing.T) {
	if err := testEngine().Hydrate(context.Background(), nil); err == nil {
		t.Error("Hydrate(nil pool) = nil, want error")
	}
	pool, ctx := lifecycleDB(t)
	err := ResolveIncident(ctx, pool,
		"00000000-0000-4000-8000-000000000000", "", "x")
	if err == nil || errors.Is(err, ErrIncidentNotFound) {
		t.Errorf("empty resolver err = %v, want identity error", err)
	}
}

func TestLifecycle_SupersededAfterGapPersistsLink(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	cycle(t, ctx, eng, pool, true)

	late := hotSnapshotForRegress()
	late.CollectedAt = time.Now().Add(2 * time.Hour) // detection gap
	eng.AnalyzeContext(ctx, late, nil, testConfig(), nil)
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("PersistIncidents: %v", err)
	}
	rows := incidentRows(t, ctx, pool, db)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(rows), rows)
	}
	old, cur := rows[0], rows[1]
	if old.resolvedAt == nil || old.resolvedBy == nil ||
		*old.resolvedBy != ResolvedBySupersede {
		t.Errorf("old row not superseded: %+v", old)
	}
	if cur.previousID == nil || *cur.previousID != old.id {
		t.Errorf("new row previous_incident_id = %v, want %s",
			cur.previousID, old.id)
	}
}

func TestLifecycle_ExternallyDeletedIncidentIsDropped(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	if err := eng.Hydrate(ctx, pool); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	rec := &recordingDispatcher{}
	eng.WithDispatcher(rec)
	cycle(t, ctx, eng, pool, true)
	if _, err := pool.Exec(ctx,
		"DELETE FROM sage.incidents WHERE database_name = $1", db); err != nil {
		t.Fatalf("delete: %v", err)
	}
	cycle(t, ctx, eng, pool, true)
	if n := len(eng.ActiveIncidents()); n != 0 {
		t.Errorf("active after external delete = %d, want 0", n)
	}
	if n := len(rec.byType("incident_resolved")); n != 0 {
		t.Errorf("resolved events = %d, want 0 for a deleted row", n)
	}
	cycle(t, ctx, eng, pool, true)
	if rows := incidentRows(t, ctx, pool, db); len(rows) != 1 {
		t.Errorf("rows after re-detection = %d, want 1", len(rows))
	}
}

type failingDispatcher struct{ calls int }

func (f *failingDispatcher) Dispatch(context.Context, notify.Event) error {
	f.calls++
	return errors.New("smtp down")
}

func TestLifecycle_DispatchFailureIsLoggedNotFatal(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	var mu sync.Mutex
	var logged []string
	eng := NewEngine(testRCACfg(), func(level, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logged = append(logged, level+":"+fmt.Sprintf(msg, args...))
	})
	db := "lc_dispatch_failure"
	eng.WithDatabaseName(db)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE database_name = $1", db)
	})
	fd := &failingDispatcher{}
	eng.WithDispatcher(fd)
	cycle(t, ctx, eng, pool, true)
	if fd.calls != 1 {
		t.Errorf("dispatch calls = %d, want 1", fd.calls)
	}
	mu.Lock()
	joined := strings.Join(logged, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "warn:rca: dispatch incident_detected") ||
		!strings.Contains(joined, "smtp down") {
		t.Errorf("dispatch failure not logged: %s", joined)
	}
	if rows := incidentRows(t, ctx, pool, db); len(rows) != 1 {
		t.Errorf("rows = %d, want 1 (state durable despite dispatch error)",
			len(rows))
	}
}

func TestLifecycle_PersistFailureKeepsStateAndRetries(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	rec := &recordingDispatcher{}
	eng.WithDispatcher(rec)
	eng.Analyze(hotSnapshotForRegress(), nil, testConfig(), nil)

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := eng.PersistIncidents(canceled, pool); err == nil {
		t.Fatal("PersistIncidents with canceled ctx = nil, want error")
	}
	if n := len(rec.byType("incident_detected")); n != 0 {
		t.Fatalf("detected events before durable write = %d, want 0", n)
	}
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("retry PersistIncidents: %v", err)
	}
	if n := len(rec.byType("incident_detected")); n != 1 {
		t.Errorf("detected events after retry = %d, want 1", n)
	}
	if rows := incidentRows(t, ctx, pool, db); len(rows) != 1 {
		t.Errorf("rows = %d, want 1", len(rows))
	}
}

func TestLifecycle_ResolveRacingPersistIsNotOverwritten(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	if err := eng.Hydrate(ctx, pool); err != nil {
		t.Fatalf("Hydrate: %v", err)
	}
	cycle(t, ctx, eng, pool, true)
	id := incidentRows(t, ctx, pool, db)[0].id

	// Operator resolves after the engine analyzed but before it persists.
	eng.AnalyzeContext(ctx, hotSnapshotForRegress(), nil, testConfig(), nil)
	if err := ResolveIncident(ctx, pool, id, "user:ops@example.com",
		"race"); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("PersistIncidents: %v", err)
	}
	rows := incidentRows(t, ctx, pool, db)
	if len(rows) != 1 || rows[0].resolvedAt == nil ||
		rows[0].reason == nil || *rows[0].reason != "race" {
		t.Fatalf("racing resolution overwritten: %+v", rows)
	}
	if n := len(eng.ActiveIncidents()); n != 0 {
		t.Errorf("engine still tracks resolved incident: %d active", n)
	}
}
