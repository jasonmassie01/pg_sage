package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// forecast-06: sage.size_history was written only by the retired v0.9
// growth forecaster, which had no production caller. A fresh bootstrap no
// longer creates it, and the ddlDropSizeHistory step drops it from an
// upgraded schema; a second run is a no-op.

// legacySizeHistory is the v2.4.0 shape of the table, with a row in it.
const legacySizeHistory = `
CREATE TABLE IF NOT EXISTS sage.size_history (
    id              BIGSERIAL PRIMARY KEY,
    collected_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    metric_type     TEXT NOT NULL CHECK (metric_type IN ('database', 'table', 'wal_slot')),
    object_name     TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL,
    dead_tuple_pct  NUMERIC(5,2),
    database_name   TEXT
);
CREATE INDEX IF NOT EXISTS idx_size_history_lookup
    ON sage.size_history (metric_type, object_name, collected_at DESC);
INSERT INTO sage.size_history (metric_type, object_name, size_bytes)
VALUES ('database', 'app', 1000);
`

func sizeHistoryExists(t *testing.T, ctx context.Context, pool *pgxpool.Pool) bool {
	t.Helper()
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT to_regclass('sage.size_history') IS NOT NULL`).Scan(&exists); err != nil {
		t.Fatalf("look up sage.size_history: %v", err)
	}
	return exists
}

// The drop is a T2 step (after every T1 step); later T2 steps follow it.
func TestSizeHistoryDrop_IsATrancheTwoStepAndIdempotentSQL(t *testing.T) {
	steps := migrationSteps()
	// The T2 steps run after every T1 step and before the T3 to T7 steps.
	firstT2 := len(steps) - len(trancheTwoMigrations()) - len(trancheThreeMigrations()) -
		len(trancheFourMigrations()) - len(trancheFiveMigrations()) -
		len(trancheSixMigrations()) - len(trancheSevenMigrations())
	var last migrationStep
	for _, s := range steps[firstT2 : firstT2+len(trancheTwoMigrations())] {
		if s.name == "ddlDropSizeHistory" {
			last = s
		}
	}
	if last.name != "ddlDropSizeHistory" {
		t.Fatal("ddlDropSizeHistory is not a T2 step (after every T1 step)")
	}
	if !strings.Contains(last.sql, "DROP TABLE IF EXISTS sage.size_history") ||
		!strings.Contains(last.sql, "lock_timeout") {
		t.Fatalf("ddlDropSizeHistory = %q, want an IF EXISTS drop under lock_timeout",
			last.sql)
	}
	if strings.Contains(fullSchemaDDL, "size_history") {
		t.Fatal("the full schema DDL still creates sage.size_history")
	}
	for _, table := range expectedTables {
		if table.name == "size_history" || strings.Contains(table.ddl, "size_history") {
			t.Fatalf("bootstrap still creates sage.size_history (%s)", table.name)
		}
	}
}

// Integration: a fresh bootstrap leaves no size_history; an upgraded
// schema that has one (with rows) loses it, and running again is clean.
func TestSizeHistoryDrop_RunsOnceThenIsANoOp(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	bootstrapWithRetry(t, ctx, pool)
	if sizeHistoryExists(t, ctx, pool) {
		t.Fatal("a fresh bootstrap created sage.size_history")
	}

	if _, err := pool.Exec(ctx, legacySizeHistory); err != nil {
		t.Fatalf("simulating the pre-forecast-06 schema: %v", err)
	}
	if !sizeHistoryExists(t, ctx, pool) {
		t.Fatal("the legacy table was not created")
	}
	bootstrapWithRetry(t, ctx, pool)
	if sizeHistoryExists(t, ctx, pool) {
		t.Fatal("bootstrap did not drop the legacy sage.size_history")
	}
	bootstrapWithRetry(t, ctx, pool)
	if sizeHistoryExists(t, ctx, pool) {
		t.Fatal("a second bootstrap recreated sage.size_history")
	}
}
