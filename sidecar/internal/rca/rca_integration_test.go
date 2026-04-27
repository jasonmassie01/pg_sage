//go:build integration

package rca

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
)

// ---------------------------------------------------------------------------
// Test DB helper (sync.Once pattern)
// ---------------------------------------------------------------------------

var (
	integPool     *pgxpool.Pool
	integPoolOnce sync.Once
	integPoolErr  error
)

func requireDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	integPoolOnce.Do(func() {
		dsn := os.Getenv("SAGE_DATABASE_URL")
		if dsn == "" {
			dsn = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			integPoolErr = err
			return
		}
		if err := pool.Ping(ctx); err != nil {
			pool.Close()
			integPoolErr = err
			return
		}
		if err := schema.Bootstrap(ctx, pool); err != nil {
			pool.Close()
			integPoolErr = err
			return
		}
		schema.ReleaseAdvisoryLock(ctx, pool)
		integPool = pool
	})
	if integPoolErr != nil {
		t.Skipf("database unavailable: %v", integPoolErr)
	}
	return integPool
}

// cleanIncidents removes all rows from sage.incidents before each test.
func cleanIncidents(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, "DELETE FROM sage.incidents")
	if err != nil {
		t.Fatalf("cleanup sage.incidents: %v", err)
	}
}

// hotSnapshot returns a snapshot that triggers the connections_high signal
// (85/100 = 85%, threshold is 80%).
func hotSnapshot() *collector.Snapshot {
	return &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  85,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
}

// integConfig returns a config that fires connections_high at 80%.
func integConfig() *config.Config {
	return &config.Config{
		RCA: config.RCAConfig{
			Enabled:                 true,
			DedupWindowMinutes:      30,
			EscalationCycles:        5,
			ResolutionCycles:        2,
			ConnectionSaturationPct: 80,
		},
		Analyzer: config.AnalyzerConfig{
			CacheHitRatioWarning: 0.95,
		},
	}
}

// (noopLog is defined in tier2_test.go; re-used here under the integration tag.)

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestPersistIncidents_Insert(t *testing.T) {
	pool := requireDB(t)
	cleanIncidents(t, pool)
	ctx := context.Background()

	cfg := integConfig()
	eng := NewEngine(&cfg.RCA, noopLog)

	// Generate an incident via Analyze.
	snap := hotSnapshot()
	eng.Analyze(snap, nil, cfg, nil)

	// Persist.
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("PersistIncidents: %v", err)
	}

	// Read back the incident. There should be exactly one.
	eng.mu.Lock()
	if len(eng.incidents) == 0 {
		eng.mu.Unlock()
		t.Fatal("expected at least one incident after Analyze")
	}
	inc := eng.incidents[0]
	eng.mu.Unlock()

	var (
		severity        string
		rootCause       string
		source          string
		occurrenceCount int
		confidence      float64
		actionRisk      *string
		databaseName    *string
		resolvedAt      *time.Time
		escalatedAt     *time.Time
	)
	err := pool.QueryRow(ctx,
		`SELECT severity, root_cause, source, occurrence_count,
		        confidence, action_risk, database_name, resolved_at, escalated_at
		 FROM sage.incidents WHERE id = $1::uuid`, inc.ID,
	).Scan(&severity, &rootCause, &source, &occurrenceCount,
		&confidence, &actionRisk, &databaseName, &resolvedAt, &escalatedAt)
	if err != nil {
		t.Fatalf("reading back incident: %v", err)
	}

	if severity != inc.Severity {
		t.Errorf("severity = %q, want %q", severity, inc.Severity)
	}
	if rootCause != inc.RootCause {
		t.Errorf("root_cause = %q, want %q", rootCause, inc.RootCause)
	}
	if source != "deterministic" {
		t.Errorf("source = %q, want %q", source, "deterministic")
	}
	if occurrenceCount != 1 {
		t.Errorf("occurrence_count = %d, want 1", occurrenceCount)
	}
	if confidence != inc.Confidence {
		t.Errorf("confidence = %f, want %f", confidence, inc.Confidence)
	}
	if resolvedAt != nil {
		t.Error("resolved_at should be nil for active incident")
	}
	if escalatedAt != nil {
		t.Error("escalated_at should be nil for new incident")
	}
}

func TestPersistIncidents_Upsert(t *testing.T) {
	pool := requireDB(t)
	cleanIncidents(t, pool)
	ctx := context.Background()

	cfg := integConfig()
	eng := NewEngine(&cfg.RCA, noopLog)

	snap := hotSnapshot()

	// First Analyze + Persist.
	eng.Analyze(snap, nil, cfg, nil)
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("first PersistIncidents: %v", err)
	}

	eng.mu.Lock()
	incID := eng.incidents[0].ID
	eng.mu.Unlock()

	// Second Analyze cycle — same signal fires, dedup increments
	// OccurrenceCount.
	snap2 := hotSnapshot()
	eng.Analyze(snap2, nil, cfg, nil)
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("second PersistIncidents: %v", err)
	}

	var occurrenceCount int
	err := pool.QueryRow(ctx,
		`SELECT occurrence_count FROM sage.incidents WHERE id = $1::uuid`,
		incID,
	).Scan(&occurrenceCount)
	if err != nil {
		t.Fatalf("reading back incident: %v", err)
	}
	if occurrenceCount < 2 {
		t.Errorf("occurrence_count = %d, want >= 2", occurrenceCount)
	}
}

func TestPersistIncidents_Resolved(t *testing.T) {
	pool := requireDB(t)
	cleanIncidents(t, pool)
	ctx := context.Background()

	cfg := integConfig()
	eng := NewEngine(&cfg.RCA, noopLog)

	// Generate an incident.
	snap := hotSnapshot()
	eng.Analyze(snap, nil, cfg, nil)
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("PersistIncidents (hot): %v", err)
	}

	eng.mu.Lock()
	incID := eng.incidents[0].ID
	// Bypass the grace period so auto-resolve logic kicks in.
	eng.gracePeriodLeft = 0
	eng.mu.Unlock()

	// Run quiet cycles to trigger auto-resolve. ResolutionCycles = 2,
	// so we need 2 consecutive quiet cycles.
	quiet := &collector.Snapshot{
		CollectedAt: time.Now(),
		System: collector.SystemStats{
			TotalBackends:  10,
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
	}
	for i := 0; i < cfg.RCA.ResolutionCycles; i++ {
		eng.Analyze(quiet, nil, cfg, nil)
	}

	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("PersistIncidents (resolved): %v", err)
	}

	var resolvedAt *time.Time
	err := pool.QueryRow(ctx,
		`SELECT resolved_at FROM sage.incidents WHERE id = $1::uuid`,
		incID,
	).Scan(&resolvedAt)
	if err != nil {
		t.Fatalf("reading back incident: %v", err)
	}
	if resolvedAt == nil {
		t.Error("resolved_at should NOT be nil after auto-resolve")
	}
}

func TestPersistIncidents_Escalated(t *testing.T) {
	pool := requireDB(t)
	cleanIncidents(t, pool)
	ctx := context.Background()

	cfg := integConfig()
	eng := NewEngine(&cfg.RCA, noopLog)

	snap := hotSnapshot()

	// Run enough hot Analyze cycles so OccurrenceCount >= EscalationCycles (5).
	// First Analyze creates the incident (occurrence_count=1). Each
	// subsequent Analyze with the same signal deduplicates and increments.
	for i := 0; i < cfg.RCA.EscalationCycles; i++ {
		snap.CollectedAt = time.Now()
		eng.Analyze(snap, nil, cfg, nil)
	}

	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("PersistIncidents: %v", err)
	}

	eng.mu.Lock()
	incID := eng.incidents[0].ID
	eng.mu.Unlock()

	var severity string
	var escalatedAt *time.Time
	err := pool.QueryRow(ctx,
		`SELECT severity, escalated_at FROM sage.incidents WHERE id = $1::uuid`,
		incID,
	).Scan(&severity, &escalatedAt)
	if err != nil {
		t.Fatalf("reading back incident: %v", err)
	}
	if escalatedAt == nil {
		t.Error("escalated_at should NOT be nil after escalation")
	}
	if severity != "critical" {
		t.Errorf("severity = %q, want %q after escalation", severity, "critical")
	}
}

func TestPersistIncidents_EmptyIncidents(t *testing.T) {
	pool := requireDB(t)
	cleanIncidents(t, pool)
	ctx := context.Background()

	cfg := integConfig()
	eng := NewEngine(&cfg.RCA, noopLog)

	// No Analyze calls — incidents slice is empty.
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("PersistIncidents on empty engine: %v", err)
	}

	var count int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sage.incidents`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("counting incidents: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 rows in sage.incidents, got %d", count)
	}
}

func TestPersistIncidents_CausalChain(t *testing.T) {
	pool := requireDB(t)
	cleanIncidents(t, pool)
	ctx := context.Background()

	cfg := integConfig()
	eng := NewEngine(&cfg.RCA, noopLog)

	snap := hotSnapshot()
	eng.Analyze(snap, nil, cfg, nil)
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("PersistIncidents: %v", err)
	}

	eng.mu.Lock()
	inc := eng.incidents[0]
	eng.mu.Unlock()

	// Read the raw causal_chain JSONB from the database.
	var chainJSON []byte
	err := pool.QueryRow(ctx,
		`SELECT causal_chain FROM sage.incidents WHERE id = $1::uuid`,
		inc.ID,
	).Scan(&chainJSON)
	if err != nil {
		t.Fatalf("reading causal_chain: %v", err)
	}

	var chain []ChainLink
	if err := json.Unmarshal(chainJSON, &chain); err != nil {
		t.Fatalf("unmarshalling causal_chain: %v", err)
	}

	if len(chain) == 0 {
		t.Fatal("causal_chain should not be empty")
	}

	// The connections_high tree produces at least one chain link.
	if len(chain) != len(inc.CausalChain) {
		t.Errorf("chain length = %d, want %d",
			len(chain), len(inc.CausalChain))
	}
	for i, link := range chain {
		expected := inc.CausalChain[i]
		if link.Order != expected.Order {
			t.Errorf("chain[%d].Order = %d, want %d",
				i, link.Order, expected.Order)
		}
		if link.Signal != expected.Signal {
			t.Errorf("chain[%d].Signal = %q, want %q",
				i, link.Signal, expected.Signal)
		}
		if link.Description != expected.Description {
			t.Errorf("chain[%d].Description = %q, want %q",
				i, link.Description, expected.Description)
		}
		if link.Evidence != expected.Evidence {
			t.Errorf("chain[%d].Evidence = %q, want %q",
				i, link.Evidence, expected.Evidence)
		}
	}
}

func TestNewUUID_Format(t *testing.T) {
	uuidRe := regexp.MustCompile(
		`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
	)
	for i := 0; i < 100; i++ {
		id := newUUID()
		if !uuidRe.MatchString(id) {
			t.Fatalf("newUUID() = %q does not match UUID v4 format", id)
		}
	}
}
