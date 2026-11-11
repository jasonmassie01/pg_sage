package schema

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// config-04b review: drift the fingerprint must notice, and the meta
// database's MigrateConfigSchema call after Bootstrap.

// No concurrent access tests here: parallel first boots are covered by
// TestBootstrap_ParallelBootsRecordEachStepOnce.

// fingerprintNow reads the current catalog fingerprint.
func fingerprintNow(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var fp string
	if err := pool.QueryRow(context.Background(), catalogFingerprintSQL).Scan(&fp); err != nil {
		t.Fatalf("read the catalog fingerprint: %v", err)
	}
	return fp
}

// CHECK-01 in the meta database: initMetaDB runs MigrateConfigSchema after
// Bootstrap; over a sealed ledger the pair issues no DDL.
func TestMigrateConfigSchema_AfterBootstrapIssuesNoDDL(t *testing.T) {
	pool := freshSchemaPool(t, "schema_ledger_meta")
	ctx := context.Background()
	if err := MigrateConfigSchema(ctx, pool); err != nil {
		t.Fatalf("first config migration: %v", err)
	}
	resetStatements(t, pool)
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("boot: %v", err)
	}
	if err := MigrateConfigSchema(ctx, pool); err != nil {
		t.Fatalf("config migration: %v", err)
	}
	if ddl := ddlStatementsSinceReset(t, pool); len(ddl) > 0 {
		t.Fatalf("a meta boot over a recorded ledger ran %d DDL statements:\n%s",
			len(ddl), strings.Join(ddl, "\n"))
	}
}

// State transition: MigrateConfigSchema over a drifted catalog must not
// seal it, or the next Bootstrap would trust the drift and not repair it.
func TestMigrateConfigSchema_DoesNotSealADriftedCatalog(t *testing.T) {
	pool := freshSchemaPool(t, "schema_ledger_meta_drift")
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		"ALTER TABLE sage.query_hints DROP COLUMN rewrite_rationale"); err != nil {
		t.Fatalf("drop the column: %v", err)
	}
	if err := MigrateConfigSchema(ctx, pool); err != nil {
		t.Fatalf("config migration: %v", err)
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("boot after the drift: %v", err)
	}
	var present bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_attribute
		WHERE attrelid = 'sage.query_hints'::regclass AND attname = 'rewrite_rationale'
		  AND NOT attisdropped)`).Scan(&present); err != nil || !present {
		t.Fatalf("rewrite_rationale present=%v (%v), want re-added", present, err)
	}
}

// State transition: a function setting a step pins (search_path, reset
// since) is drift: the next boot re-runs the step and pins it again.
func TestBootstrap_RepairsAResetFunctionSearchPath(t *testing.T) {
	pool := freshSchemaPool(t, "schema_ledger_searchpath")
	ctx := context.Background()
	before := fingerprintNow(t, pool)
	if _, err := pool.Exec(ctx,
		"ALTER FUNCTION sage.emergency_stop(boolean, text) RESET search_path"); err != nil {
		t.Fatalf("reset search_path: %v", err)
	}
	if after := fingerprintNow(t, pool); after == before {
		t.Fatal("the fingerprint did not change when a function's search_path was reset")
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("boot: %v", err)
	}
	var cfg string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(array_to_string(proconfig, ','), '')
		FROM pg_proc WHERE oid = 'sage.emergency_stop(boolean, text)'::regprocedure`).
		Scan(&cfg); err != nil {
		t.Fatalf("read proconfig: %v", err)
	}
	if !strings.Contains(cfg, "search_path=pg_catalog, pg_temp") {
		t.Fatalf("emergency_stop proconfig = %q, want the pinned search_path back", cfg)
	}
	assertBootRunsNoDDL(t, pool)
}

// Boundary: an index rebuilt with another column order (ASC for DESC)
// changes the fingerprint.
func TestCatalogFingerprint_SeesIndexColumnOptions(t *testing.T) {
	pool := freshSchemaPool(t, "schema_ledger_indoption")
	ctx := context.Background()
	before := fingerprintNow(t, pool)
	if _, err := pool.Exec(ctx, `DROP INDEX sage.idx_config_audit_time;
		CREATE INDEX idx_config_audit_time ON sage.config_audit (changed_at)`); err != nil {
		t.Fatalf("rebuild the index ascending: %v", err)
	}
	if after := fingerprintNow(t, pool); after == before {
		t.Fatal("the fingerprint did not change when an index lost its DESC option")
	}
}
