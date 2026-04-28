package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/migration"
	"github.com/pg-sage/sidecar/internal/schema"
)

func requireMigrationSafetyDB(
	t *testing.T,
) (*pgxpool.Pool, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	dsn := os.Getenv("SAGE_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Skipf("database unavailable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("database unavailable: %v", err)
	}
	if err := schema.Bootstrap(ctx, pool); err != nil {
		pool.Close()
		t.Skipf("schema unavailable: %v", err)
	}
	schema.ReleaseAdvisoryLock(ctx, pool)
	t.Cleanup(pool.Close)
	return pool, ctx
}

func TestMigrationSafetyFindingStoreUpsertsOneOpenFinding(t *testing.T) {
	pool, ctx := requireMigrationSafetyDB(t)
	store := NewMigrationSafetyFindingStore(pool)
	input := migration.MigrationSafetyFinding{
		RuleID:           "ddl_index_not_concurrent",
		Severity:         "warning",
		ObjectType:       "migration",
		ObjectIdentifier: fmt.Sprintf("public.orders:test:%d", time.Now().UnixNano()),
		Title:            "Dangerous DDL: ddl_index_not_concurrent",
		Detail: map[string]any{
			"original_sql": "CREATE INDEX idx_orders_id ON orders (id)",
			"risk_score":   0.7,
		},
		Recommendation: "Review the safer migration path.",
		RecommendedSQL: "CREATE INDEX CONCURRENTLY idx_orders_id ON orders (id)",
		ImpactScore:    0.7,
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM sage.findings WHERE object_identifier = $1`,
			input.ObjectIdentifier)
	})

	firstID, err := store.UpsertMigrationSafetyFinding(ctx, input)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	input.Detail["risk_score"] = 0.9
	secondID, err := store.UpsertMigrationSafetyFinding(ctx, input)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if firstID != secondID {
		t.Fatalf("ids = %d then %d, want same", firstID, secondID)
	}

	var count int
	var occurrences int
	var recommendedSQL string
	err = pool.QueryRow(ctx,
		`SELECT count(*), max(occurrence_count),
		        max(COALESCE(recommended_sql, ''))
		   FROM sage.findings
		  WHERE category = $1 AND object_identifier = $2`,
		migration.SafetyFindingCategory, input.ObjectIdentifier,
	).Scan(&count, &occurrences, &recommendedSQL)
	if err != nil {
		t.Fatalf("query persisted finding: %v", err)
	}
	if count != 1 || occurrences != 2 {
		t.Fatalf("count=%d occurrences=%d, want 1 and 2",
			count, occurrences)
	}
	if recommendedSQL != input.RecommendedSQL {
		t.Fatalf("recommended_sql = %q", recommendedSQL)
	}
}
