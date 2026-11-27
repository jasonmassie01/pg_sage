//go:build integration

package advisor

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/managedparam"
	"github.com/pg-sage/sidecar/internal/schema"
)

// CHECK-01 and CHECK-02 (advisors-04): a model that plants managed_change
// in bloat and rewrite output leaves no such key in sage.findings, so the
// managed-change worker has nothing to propose from those findings.
func TestInjectedManagedChange_NeverPersistedOrProposed_LivePG(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	clean := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM sage.findings
			WHERE object_identifier LIKE 'adv04_%'`); err != nil {
			t.Fatalf("clean findings: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)

	srv, mgr := mockLLMServer(t, `[{"object_identifier":"adv04_bloat","rationale":"r",`+
		`"recommended_sql":"VACUUM public.orders",`+reservedInjection+`}]`)
	defer srv.Close()
	bloat, err := analyzeBloat(ctx, mgr, bloatSnapshot(), &config.Config{}, noopLog)
	if err != nil || len(bloat) != 1 {
		t.Fatalf("analyzeBloat = %d findings, %v; want 1", len(bloat), err)
	}
	rewrite := rewriteFindings(parseLLMFindings(`[{"object_identifier":"adv04_rewrite",`+
		`"rationale":"r","suggested_rewrite":"SELECT 1",`+reservedInjection+`}]`,
		"query_rewrite", noopLog))
	all := GateConfigFindings(append(bloat, rewrite...), 16<<30, "rds", "postgres", nil)
	if len(all) != 2 {
		t.Fatalf("gated findings = %d, want 2", len(all))
	}
	if err := analyzer.UpsertFindings(ctx, pool, all); err != nil {
		t.Fatalf("upsert findings: %v", err)
	}
	var planted int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.findings
		WHERE category IN ('query_rewrite','bloat_remediation')
		  AND object_identifier LIKE 'adv04_%' AND detail ? 'managed_change'`).
		Scan(&planted); err != nil {
		t.Fatalf("CHECK-01 query: %v", err)
	}
	if planted != 0 {
		t.Fatalf("CHECK-01: %d persisted findings carry managed_change", planted)
	}
	assertNoPersistedIntent(t, ctx, pool)
}

// assertNoPersistedIntent is CHECK-02's advisor half: no persisted
// injected finding carries a managed_change the worker could read. It
// reads the rows back rather than running a worker cycle, which would
// propose and supersede other packages' rows in the shared test database;
// managedparam's TestWorkerRefusesForeignCategoriesAndParameters covers
// the worker's own category refusal.
func assertNoPersistedIntent(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT object_identifier, detail FROM sage.findings
		WHERE object_identifier LIKE 'adv04_%' AND status = 'open'`)
	if err != nil {
		t.Fatalf("CHECK-02 query: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var ident string
		var detail map[string]any
		if err := rows.Scan(&ident, &detail); err != nil {
			t.Fatalf("CHECK-02 scan: %v", err)
		}
		n++
		if in, ok := managedparam.IntentFromDetail(detail); ok {
			t.Errorf("CHECK-02: %s carries a managed intent %+v", ident, in)
		}
		if _, ok := detail[analyzer.DetailApprovalRequired]; ok {
			t.Errorf("CHECK-02: %s carries a model approval_required: %v", ident, detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("CHECK-02 rows: %v", err)
	}
	if n != 2 {
		t.Fatalf("CHECK-02: read %d open injected findings, want 2", n)
	}
}

func bloatSnapshot() *collector.Snapshot {
	now := time.Now()
	lastVac := now.Add(-24 * time.Hour)
	return &collector.Snapshot{
		CollectedAt: now,
		ConfigData:  &collector.ConfigSnapshot{},
		Tables: []collector.TableStats{{SchemaName: "public", RelName: "orders",
			NLiveTup: 7000, NDeadTup: 3000, TableBytes: 50 << 20, IndexBytes: 10 << 20,
			LastAutovacuum: &lastVac, AutovacuumCount: 5}},
		System: collector.SystemStats{DBSizeBytes: 500 << 20},
	}
}
