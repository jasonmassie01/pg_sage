package rca

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
)

// No nil-config/malformed-SQL tests here: these focused regression probes target
// valid operational state transitions; existing RCA tests own detector input validation.
func preflightRCAPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), "DELETE FROM sage.incidents"); err != nil {
		t.Fatal(err)
	}
	return pool
}

func preflightRCAConfig() *config.Config {
	return &config.Config{
		RCA: config.RCAConfig{Enabled: true, DedupWindowMinutes: 30,
			EscalationCycles: 5, ResolutionCycles: 2, ConnectionSaturationPct: 80},
		Analyzer: config.AnalyzerConfig{CacheHitRatioWarning: 0.95},
	}
}

func preflightRCACycle(t *testing.T, e *Engine, pool *pgxpool.Pool, hot bool) {
	t.Helper()
	connections := 10
	if hot {
		connections = 85
	}
	snapshot := &collector.Snapshot{CollectedAt: time.Now(),
		System: collector.SystemStats{TotalBackends: connections,
			MaxConnections: 100, CacheHitRatio: 0.999}}
	// Adapted setup: production never calls Engine.Analyze directly. The
	// runtime adapter (cmd/pg_sage_sidecar/rca_adapter.go) hydrates the
	// engine from sage.incidents before every cycle; Hydrate is a no-op
	// after its first success. Mirror that production path here.
	if err := e.Hydrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	e.Analyze(snapshot, nil, preflightRCAConfig(), nil)
	if err := e.PersistIncidents(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
}

func preflightRCAState(t *testing.T, pool *pgxpool.Pool) (int, int) {
	t.Helper()
	var active, total int
	if err := pool.QueryRow(t.Context(), `SELECT
		count(*) FILTER (WHERE resolved_at IS NULL), count(*) FROM sage.incidents`).
		Scan(&active, &total); err != nil {
		t.Fatal(err)
	}
	t.Logf("persisted RCA active=%d total=%d", active, total)
	return active, total
}

func TestPreflightRCAInProcessDedupAndClearControl(t *testing.T) {
	pool := preflightRCAPool(t)
	cfg := preflightRCAConfig()
	e := NewEngine(&cfg.RCA, noopLog)
	preflightRCACycle(t, e, pool, true)
	preflightRCACycle(t, e, pool, true)
	if active, total := preflightRCAState(t, pool); active != 1 || total != 1 {
		t.Fatalf("in-process dedup active=%d total=%d; want 1/1", active, total)
	}
	for range 6 {
		preflightRCACycle(t, e, pool, false)
	}
	if active, total := preflightRCAState(t, pool); active != 0 || total != 1 {
		t.Fatalf("in-process clear active=%d total=%d; want 0/1", active, total)
	}
}

func TestPreflightRCARestartResumesExistingIdentity(t *testing.T) {
	pool := preflightRCAPool(t)
	cfg := preflightRCAConfig()
	preflightRCACycle(t, NewEngine(&cfg.RCA, noopLog), pool, true)
	// Production startup creates a new Engine and hydrates it through the
	// runtime adapter before its first cycle (see preflightRCACycle).
	preflightRCACycle(t, NewEngine(&cfg.RCA, noopLog), pool, true)
	if active, total := preflightRCAState(t, pool); active != 1 || total != 1 {
		t.Fatalf("restart duplicated incident identity: active=%d total=%d; want 1/1", active, total)
	}
}

func TestPreflightRCARestartClearsPersistedIncident(t *testing.T) {
	pool := preflightRCAPool(t)
	cfg := preflightRCAConfig()
	preflightRCACycle(t, NewEngine(&cfg.RCA, noopLog), pool, true)
	restarted := NewEngine(&cfg.RCA, noopLog)
	for range 8 {
		preflightRCACycle(t, restarted, pool, false)
	}
	if active, _ := preflightRCAState(t, pool); active != 0 {
		t.Fatalf("resolved workload left %d orphaned active incident after restart", active)
	}
}

func TestPreflightRCAManualResolutionSurvivesStaleConcurrentFlush(t *testing.T) {
	pool := preflightRCAPool(t)
	cfg := preflightRCAConfig()
	e := NewEngine(&cfg.RCA, noopLog)
	preflightRCACycle(t, e, pool, true)
	id := e.ActiveIncidents()[0].ID
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// The persisted transition used by the manual resolve API
	// (rca.ResolveIncident), held open in a transaction to force the race.
	if _, err := tx.Exec(t.Context(), `UPDATE sage.incidents SET resolved_at=now(),
		resolved_by='user:preflight@example.test'
		WHERE id=$1 AND resolved_at IS NULL`, id); err != nil {
		t.Fatal(err)
	}
	// Adapted setup: an unchanged incident is not written at all (perf F9.2),
	// so it cannot race the resolution. The engine sees the condition again
	// first, as the next cycle would, and its flush has a change to write.
	e.Analyze(&collector.Snapshot{CollectedAt: time.Now(),
		System: collector.SystemStats{TotalBackends: 85, MaxConnections: 100,
			CacheHitRatio: 0.999}}, nil, preflightRCAConfig(), nil)
	done := make(chan error, 1)
	go func() { done <- e.PersistIncidents(t.Context(), pool) }()
	preflightWaitForIncidentFlush(t, pool)
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var resolved bool
	if err := pool.QueryRow(t.Context(),
		"SELECT resolved_at IS NOT NULL FROM sage.incidents WHERE id=$1", id).
		Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if !resolved {
		t.Fatal("committed manual resolution was erased by a stale concurrent engine flush")
	}
}

// preflightWaitForIncidentFlush waits until the engine's write to the
// locked incident row is blocked. Adapted probe: an already-persisted
// incident is now written with a compare-and-swap UPDATE, not an upsert.
func preflightWaitForIncidentFlush(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database() AND pid<>pg_backend_pid()
			AND state='active' AND wait_event_type='Lock'
			AND (query LIKE '%INSERT INTO sage.incidents%'
			     OR query LIKE '%UPDATE sage.incidents%'))`).Scan(&waiting)
		if err == nil && waiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("incident flush never blocked on manual-resolution row lock")
}
