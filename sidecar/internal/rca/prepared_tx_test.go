package rca

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// ---------------------------------------------------------------------------
// detectOrphanedPreparedTx
// ---------------------------------------------------------------------------

func TestDetectOrphanedPreparedTx_EmptyPreparedXacts(t *testing.T) {
	eng := newTestEngine()
	snap := &collector.Snapshot{
		CollectedAt:   time.Now(),
		PreparedXacts: nil,
	}
	sig := eng.detectOrphanedPreparedTx(snap)
	if sig != nil {
		t.Errorf("expected nil for empty PreparedXacts, got %+v", sig)
	}
}

func TestDetectOrphanedPreparedTx_EmptySlice(t *testing.T) {
	eng := newTestEngine()
	snap := &collector.Snapshot{
		CollectedAt:   time.Now(),
		PreparedXacts: []collector.PreparedTransaction{},
	}
	sig := eng.detectOrphanedPreparedTx(snap)
	if sig != nil {
		t.Errorf("expected nil for empty slice, got %+v", sig)
	}
}

func TestDetectOrphanedPreparedTx_SingleBelowCritical(t *testing.T) {
	eng := newTestEngine()
	now := time.Now()
	snap := &collector.Snapshot{
		CollectedAt: now,
		PreparedXacts: []collector.PreparedTransaction{
			{
				GID:      "tx_abc_123",
				Prepared: now.Add(-time.Hour),
				Owner:    "app_user",
				Database: "mydb",
				XIDAge:   50_000_000, // < 100M => warning
			},
		},
	}

	sig := eng.detectOrphanedPreparedTx(snap)
	if sig == nil {
		t.Fatal("expected signal for single prepared tx, got nil")
	}
	if sig.ID != "orphaned_prepared_tx" {
		t.Errorf("ID = %q, want orphaned_prepared_tx", sig.ID)
	}
	if sig.Severity != "warning" {
		t.Errorf("Severity = %q, want warning", sig.Severity)
	}

	// Verify metrics.
	count := intMetric(sig, "count")
	if count != 1 {
		t.Errorf("count metric = %d, want 1", count)
	}
	gid := stringMetric(sig, "oldest_gid")
	if gid != "tx_abc_123" {
		t.Errorf("oldest_gid = %q, want tx_abc_123", gid)
	}
	age := intMetric(sig, "max_xid_age")
	if age != 50_000_000 {
		t.Errorf("max_xid_age = %d, want 50000000", age)
	}
}

func TestDetectOrphanedPreparedTx_SingleAboveCritical(t *testing.T) {
	eng := newTestEngine()
	now := time.Now()
	snap := &collector.Snapshot{
		CollectedAt: now,
		PreparedXacts: []collector.PreparedTransaction{
			{
				GID:      "old_tx_999",
				Prepared: now.Add(-24 * time.Hour),
				Owner:    "admin",
				Database: "proddb",
				XIDAge:   150_000_000, // > 100M => critical
			},
		},
	}

	sig := eng.detectOrphanedPreparedTx(snap)
	if sig == nil {
		t.Fatal("expected signal for critical-age prepared tx, got nil")
	}
	if sig.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", sig.Severity)
	}
	gid := stringMetric(sig, "oldest_gid")
	if gid != "old_tx_999" {
		t.Errorf("oldest_gid = %q, want old_tx_999", gid)
	}
}

func TestDetectOrphanedPreparedTx_MultipleFindsOldest(t *testing.T) {
	eng := newTestEngine()
	now := time.Now()
	snap := &collector.Snapshot{
		CollectedAt: now,
		PreparedXacts: []collector.PreparedTransaction{
			{
				GID:      "tx_young",
				Prepared: now.Add(-10 * time.Minute),
				Owner:    "app",
				Database: "db1",
				XIDAge:   1_000_000, // youngest
			},
			{
				GID:      "tx_oldest",
				Prepared: now.Add(-48 * time.Hour),
				Owner:    "batch",
				Database: "db2",
				XIDAge:   200_000_000, // oldest, > 100M
			},
			{
				GID:      "tx_middle",
				Prepared: now.Add(-2 * time.Hour),
				Owner:    "app",
				Database: "db1",
				XIDAge:   75_000_000, // middle
			},
		},
	}

	sig := eng.detectOrphanedPreparedTx(snap)
	if sig == nil {
		t.Fatal("expected signal for multiple prepared txs, got nil")
	}

	// Should pick the one with highest XIDAge.
	gid := stringMetric(sig, "oldest_gid")
	if gid != "tx_oldest" {
		t.Errorf("oldest_gid = %q, want tx_oldest (highest XIDAge)",
			gid)
	}
	age := intMetric(sig, "max_xid_age")
	if age != 200_000_000 {
		t.Errorf("max_xid_age = %d, want 200000000", age)
	}
	count := intMetric(sig, "count")
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	// 200M > 100M => critical
	if sig.Severity != "critical" {
		t.Errorf("Severity = %q, want critical", sig.Severity)
	}
}

func TestDetectOrphanedPreparedTx_ExactBoundary(t *testing.T) {
	eng := newTestEngine()
	now := time.Now()

	// Exactly at the boundary: 100,000,000
	snap := &collector.Snapshot{
		CollectedAt: now,
		PreparedXacts: []collector.PreparedTransaction{
			{
				GID:      "tx_boundary",
				Prepared: now.Add(-time.Hour),
				Owner:    "user1",
				Database: "db",
				XIDAge:   100_000_000, // exactly 100M, NOT > 100M
			},
		},
	}

	sig := eng.detectOrphanedPreparedTx(snap)
	if sig == nil {
		t.Fatal("expected signal at boundary, got nil")
	}
	// 100M is NOT > 100M, so severity should be "warning"
	if sig.Severity != "warning" {
		t.Errorf("Severity = %q, want warning (boundary: 100M is not > 100M)",
			sig.Severity)
	}
}

// ---------------------------------------------------------------------------
// treeOrphanedPreparedTx
// ---------------------------------------------------------------------------

func TestTreeOrphanedPreparedTx_SingleTx(t *testing.T) {
	eng := newTestEngine()
	now := time.Now()
	snap := &collector.Snapshot{
		CollectedAt: now,
		PreparedXacts: []collector.PreparedTransaction{
			{
				GID:      "orphan_gid_1",
				Prepared: now.Add(-2 * time.Hour),
				Owner:    "admin",
				Database: "proddb",
				XIDAge:   42_000_000,
			},
		},
	}

	sig := &Signal{
		ID:       "orphaned_prepared_tx",
		FiredAt:  now,
		Severity: "warning",
		Metrics: map[string]any{
			"count":       1,
			"oldest_gid":  "orphan_gid_1",
			"max_xid_age": 42_000_000,
		},
	}

	inc := eng.treeOrphanedPreparedTx(sig, snap)

	// Root cause mentions orphaned prepared transaction.
	if !strings.Contains(inc.RootCause, "Orphaned prepared transaction") {
		t.Errorf("RootCause = %q, want substring 'Orphaned prepared transaction'",
			inc.RootCause)
	}
	if !strings.Contains(inc.RootCause, "orphan_gid_1") {
		t.Errorf("RootCause = %q, want GID 'orphan_gid_1'", inc.RootCause)
	}
	if !strings.Contains(inc.RootCause, "42000000") {
		t.Errorf("RootCause = %q, want xid_age '42000000'", inc.RootCause)
	}

	// RecommendedSQL contains pg_prepared_xacts.
	if !strings.Contains(inc.RecommendedSQL, "pg_prepared_xacts") {
		t.Errorf("RecommendedSQL = %q, want pg_prepared_xacts",
			inc.RecommendedSQL)
	}

	// CausalChain has 2 links.
	if len(inc.CausalChain) != 2 {
		t.Fatalf("CausalChain len = %d, want 2", len(inc.CausalChain))
	}
	if inc.CausalChain[0].Order != 1 {
		t.Errorf("CausalChain[0].Order = %d, want 1", inc.CausalChain[0].Order)
	}
	if inc.CausalChain[1].Order != 2 {
		t.Errorf("CausalChain[1].Order = %d, want 2", inc.CausalChain[1].Order)
	}

	// ActionRisk is high_risk.
	if inc.ActionRisk != "high_risk" {
		t.Errorf("ActionRisk = %q, want high_risk", inc.ActionRisk)
	}

	// SignalIDs should contain orphaned_prepared_tx.
	if len(inc.SignalIDs) != 1 || inc.SignalIDs[0] != "orphaned_prepared_tx" {
		t.Errorf("SignalIDs = %v, want [orphaned_prepared_tx]", inc.SignalIDs)
	}

	// Severity should match the signal.
	if inc.Severity != "warning" {
		t.Errorf("Severity = %q, want warning", inc.Severity)
	}
}

func TestTreeOrphanedPreparedTx_CriticalSeverity(t *testing.T) {
	eng := newTestEngine()
	now := time.Now()
	snap := &collector.Snapshot{CollectedAt: now}

	sig := &Signal{
		ID:       "orphaned_prepared_tx",
		FiredAt:  now,
		Severity: "critical",
		Metrics: map[string]any{
			"count":       2,
			"oldest_gid":  "old_tx",
			"max_xid_age": 200_000_000,
		},
	}

	inc := eng.treeOrphanedPreparedTx(sig, snap)

	if inc.Severity != "critical" {
		t.Errorf("Severity = %q, want critical (passed through from signal)",
			inc.Severity)
	}
}

// ---------------------------------------------------------------------------
// treeVacuumBlocked Branch 2: prepared transactions
// ---------------------------------------------------------------------------

func TestTreeVacuumBlocked_Branch2_PreparedXacts(t *testing.T) {
	eng := newTestEngine()
	now := time.Now()

	// Snapshot with PreparedXacts but NO idle-in-tx locks.
	snap := &collector.Snapshot{
		CollectedAt: now,
		System: collector.SystemStats{
			MaxConnections: 100,
			CacheHitRatio:  0.999,
		},
		Tables: []collector.TableStats{
			{
				SchemaName: "public",
				RelName:    "orders",
				NLiveTup:   1000,
				NDeadTup:   200, // triggers vacuum_blocked
			},
		},
		Locks: []collector.LockInfo{
			// active state, NOT idle in transaction
			{PID: 5555, State: strPtr("active")},
		},
		PreparedXacts: []collector.PreparedTransaction{
			{
				GID:      "orphan_tx_42",
				Prepared: now.Add(-6 * time.Hour),
				Owner:    "batch_user",
				Database: "proddb",
				XIDAge:   80_000_000,
			},
		},
	}

	// Build the vacuum_blocked signal manually (as detectVacuumBlocked would).
	sig := &Signal{
		ID:       "vacuum_blocked",
		FiredAt:  now,
		Severity: "warning",
		Metrics: map[string]any{
			"blocked_tables": []string{"public.orders"},
			"dead_tuple_pct": 10,
		},
	}

	inc := eng.treeVacuumBlocked(sig, snap)

	// Root cause mentions orphaned prepared transaction and the GID.
	if !strings.Contains(inc.RootCause, "Orphaned prepared transaction") {
		t.Errorf("RootCause = %q, want substring 'Orphaned prepared transaction'",
			inc.RootCause)
	}
	if !strings.Contains(inc.RootCause, "orphan_tx_42") {
		t.Errorf("RootCause = %q, want GID 'orphan_tx_42'", inc.RootCause)
	}

	// RecommendedSQL contains ROLLBACK PREPARED.
	if !strings.Contains(inc.RecommendedSQL, "ROLLBACK PREPARED") {
		t.Errorf("RecommendedSQL = %q, want 'ROLLBACK PREPARED'",
			inc.RecommendedSQL)
	}
	if !strings.Contains(inc.RecommendedSQL, "orphan_tx_42") {
		t.Errorf("RecommendedSQL = %q, want GID in ROLLBACK",
			inc.RecommendedSQL)
	}

	// Branch 2 always sets severity to "critical".
	if inc.Severity != "critical" {
		t.Errorf("Severity = %q, want critical (Branch 2 hardcodes critical)",
			inc.Severity)
	}

	// ActionRisk is high_risk.
	if inc.ActionRisk != "high_risk" {
		t.Errorf("ActionRisk = %q, want high_risk", inc.ActionRisk)
	}
}

func TestTreeVacuumBlocked_Branch2_FindsOldestPreparedTx(t *testing.T) {
	eng := newTestEngine()
	now := time.Now()

	snap := &collector.Snapshot{
		CollectedAt: now,
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "t1", NLiveTup: 1000, NDeadTup: 200},
		},
		// No idle-in-tx locks at all.
		Locks: nil,
		PreparedXacts: []collector.PreparedTransaction{
			{
				GID:      "young_tx",
				Prepared: now.Add(-time.Hour),
				Owner:    "user1",
				Database: "db1",
				XIDAge:   10_000_000,
			},
			{
				GID:      "oldest_tx",
				Prepared: now.Add(-24 * time.Hour),
				Owner:    "user2",
				Database: "db2",
				XIDAge:   90_000_000, // highest age
			},
		},
	}

	sig := &Signal{
		ID:       "vacuum_blocked",
		FiredAt:  now,
		Severity: "warning",
		Metrics: map[string]any{
			"blocked_tables": []string{"public.t1"},
			"dead_tuple_pct": 10,
		},
	}

	inc := eng.treeVacuumBlocked(sig, snap)

	// Should reference the oldest prepared tx (highest XIDAge).
	if !strings.Contains(inc.RootCause, "oldest_tx") {
		t.Errorf("RootCause = %q, want oldest_tx (highest XIDAge)",
			inc.RootCause)
	}
	if !strings.Contains(inc.RecommendedSQL, "oldest_tx") {
		t.Errorf("RecommendedSQL = %q, want oldest_tx in ROLLBACK",
			inc.RecommendedSQL)
	}
}

func TestTreeVacuumBlocked_Branch2_NotBranch1(t *testing.T) {
	// When idle-in-tx IS present, Branch 1 should fire instead of Branch 2.
	eng := newTestEngine()
	now := time.Now()
	idleState := "idle in transaction"

	snap := &collector.Snapshot{
		CollectedAt: now,
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "t1", NLiveTup: 1000, NDeadTup: 200},
		},
		Locks: []collector.LockInfo{
			{PID: 9999, State: &idleState},
		},
		PreparedXacts: []collector.PreparedTransaction{
			{
				GID:    "some_tx",
				Owner:  "user1",
				XIDAge: 50_000_000,
			},
		},
	}

	sig := &Signal{
		ID:       "vacuum_blocked",
		FiredAt:  now,
		Severity: "warning",
		Metrics: map[string]any{
			"blocked_tables": []string{"public.t1"},
		},
	}

	inc := eng.treeVacuumBlocked(sig, snap)

	// Should hit Branch 1 (idle-in-tx), NOT Branch 2 (prepared tx).
	if strings.Contains(inc.RootCause, "Orphaned prepared transaction") {
		t.Errorf("Should have hit Branch 1 (idle-in-tx), not Branch 2. "+
			"RootCause = %q", inc.RootCause)
	}
	if !strings.Contains(inc.RootCause, "Idle-in-transaction") {
		t.Errorf("Expected Branch 1 root cause mentioning idle-in-transaction, "+
			"got %q", inc.RootCause)
	}
}

func TestTreeVacuumBlocked_Branch3_NoPreparedTxNoIdle(t *testing.T) {
	// When neither idle-in-tx nor prepared xacts are present, Branch 3 fires.
	eng := newTestEngine()
	now := time.Now()

	snap := &collector.Snapshot{
		CollectedAt: now,
		Tables: []collector.TableStats{
			{SchemaName: "public", RelName: "t1", NLiveTup: 1000, NDeadTup: 200},
		},
		Locks:         nil,
		PreparedXacts: nil,
	}

	sig := &Signal{
		ID:       "vacuum_blocked",
		FiredAt:  now,
		Severity: "warning",
		Metrics: map[string]any{
			"blocked_tables": []string{"public.t1"},
			"dead_tuple_pct": 10,
		},
	}

	inc := eng.treeVacuumBlocked(sig, snap)

	// Branch 3: autovacuum falling behind.
	if !strings.Contains(inc.RootCause, "Autovacuum falling behind") {
		t.Errorf("Expected Branch 3 root cause, got %q", inc.RootCause)
	}
	// Branch 3 uses the signal's severity, not "critical".
	if inc.Severity != "warning" {
		t.Errorf("Severity = %q, want warning (Branch 3 uses signal severity)",
			inc.Severity)
	}
}

// strPtr returns a pointer to a string value. Helper for building
// test snapshots with optional string fields.
func strPtr(v string) *string { return &v }
