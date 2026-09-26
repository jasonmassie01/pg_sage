package analyzer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

// Audit probes use TestMain's per-process disposable database. No live runtime DSN.
func auditPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool, ctx
}

func TestAuditSuppressedFindingStaysSuppressed(t *testing.T) {
	pool, ctx := auditPool(t)
	f := Finding{Category: "audit_suppression", ObjectIdentifier: "public.orders",
		Severity: "warning", Title: "Persistent issue", Detail: map[string]any{"value": 1}}
	if err := UpsertFindings(ctx, pool, []Finding{f}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.findings SET status='suppressed'
		WHERE category=$1 AND object_identifier=$2`, f.Category, f.ObjectIdentifier); err != nil {
		t.Fatal(err)
	}
	if err := UpsertFindings(ctx, pool, []Finding{f}); err != nil {
		t.Fatal(err)
	}
	var open, suppressed int
	err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='open'),
		count(*) FILTER (WHERE status='suppressed') FROM sage.findings WHERE category=$1`,
		f.Category).Scan(&open, &suppressed)
	if err != nil {
		t.Fatal(err)
	}
	if open != 0 || suppressed != 1 {
		t.Fatalf("suppression resurrected: open=%d suppressed=%d; want 0 and 1", open, suppressed)
	}
}

func TestAuditUpdatedForwardSQLKeepsMatchingInverse(t *testing.T) {
	pool, ctx := auditPool(t)
	f := Finding{Category: "audit_inverse", ObjectIdentifier: "public.orders",
		Severity: "warning", Title: "Index candidate", Detail: map[string]any{},
		RecommendedSQL: "CREATE INDEX index_a ON public.orders (a)",
		RollbackSQL:    "DROP INDEX index_a"}
	if err := UpsertFindings(ctx, pool, []Finding{f}); err != nil {
		t.Fatal(err)
	}
	f.RecommendedSQL = "CREATE INDEX index_b ON public.orders (b)"
	f.RollbackSQL = "DROP INDEX index_b"
	if err := UpsertFindings(ctx, pool, []Finding{f}); err != nil {
		t.Fatal(err)
	}
	var forward, inverse string
	err := pool.QueryRow(ctx, `SELECT recommended_sql, rollback_sql FROM sage.findings
		WHERE category=$1 AND object_identifier=$2 AND status='open'`,
		f.Category, f.ObjectIdentifier).Scan(&forward, &inverse)
	if err != nil {
		t.Fatal(err)
	}
	if forward != f.RecommendedSQL || inverse != f.RollbackSQL {
		t.Fatalf("inconsistent proposal: forward=%q inverse=%q; want %q / %q",
			forward, inverse, f.RecommendedSQL, f.RollbackSQL)
	}
}
