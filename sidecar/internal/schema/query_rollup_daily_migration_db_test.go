package schema

import (
	"context"
	"strings"
	"testing"
)

// evid-01b on a real server: bootstrap twice is clean, the daily table's
// checks hold, and a progress row for sage.query_rollup_daily is refused
// by the old table_name check and accepted once the step ran (also through
// EnsureHistoryMigrationTables on a table created before it). No
// concurrent-writer test: Bootstrap holds its advisory lock.

// oldHistoryMigration is sage.history_migration as T3 created it.
const oldHistoryMigration = `CREATE TABLE sage.history_migration (
    database_id  integer NOT NULL,
    direction    text NOT NULL CHECK (direction IN ('meta', 'monitored')),
    table_name   text NOT NULL CHECK (table_name IN ('snapshots', 'query_store')),
    last_id      bigint NOT NULL DEFAULT 0,
    last_at      timestamptz,
    copied       bigint NOT NULL DEFAULT 0,
    skipped      bigint NOT NULL DEFAULT 0,
    started_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    PRIMARY KEY (database_id, direction, table_name))`

func TestQueryRollupDailyMigration_BootstrapTwiceAndConstraints(t *testing.T) {
	pool := freshSchemaPool(t, "rollupday")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var rows int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.query_rollup_daily`).
		Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("bootstrap left %d daily rows (err %v), want 0", rows, err)
	}
	daily := func(calls int64, hours int) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_daily (queryid, day, calls,
			exec_ms, rows, shared_blks_hit, shared_blks_read, temp_blks_read,
			temp_blks_written, wal_bytes, hours, epoch_breaks, plan_hashes, last_plan_hash)
			VALUES (42, '2026-11-01', $1, 1.5, 1, 1, 1, 0, 0, NULL, $2, 0, 1, 'p1')`,
			calls, hours)
		return err
	}
	if err := daily(5, 24); err != nil {
		t.Fatalf("a valid daily row was refused: %v", err)
	}
	if err := daily(5, 24); err == nil || !strings.Contains(err.Error(),
		"query_rollup_daily_key") {
		t.Fatalf("a second row for (queryid, day) must hit the unique key, got %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM sage.query_rollup_daily`); err != nil {
		t.Fatal(err)
	}
	if err := daily(-1, 24); err == nil || !strings.Contains(err.Error(), "check") {
		t.Fatalf("a negative calls value must fail the CHECK, got %v", err)
	}
	if err := daily(1, 25); err == nil {
		t.Fatal("a day has at most 24 UTC hours: hours = 25 must be refused")
	}
}

func TestHistoryMigrationRollupTables_OldCheckRefusesNewAdmits(t *testing.T) {
	pool := freshSchemaPool(t, "histmigcheck")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS sage"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, oldHistoryMigration); err != nil {
		t.Fatalf("create the T3 progress table: %v", err)
	}
	insert := func(table string) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.history_migration (database_id,
			direction, table_name) VALUES (7, 'meta', $1)
			ON CONFLICT (database_id, direction, table_name) DO NOTHING`, table)
		return err
	}
	if err := insert("query_rollup_daily"); err == nil || !strings.Contains(err.Error(),
		"check") {
		t.Fatalf("the old check must refuse query_rollup_daily, got %v", err)
	}
	if err := EnsureHistoryMigrationTables(ctx, pool); err != nil {
		t.Fatalf("ensure the progress tables: %v", err)
	}
	for _, table := range []string{"snapshots", "query_store", "query_rollup_daily",
		"query_rollup_coverage"} {
		if err := insert(table); err != nil {
			t.Fatalf("after the step a %s progress row must insert: %v", table, err)
		}
	}
	if err := insert("query_rollup_hourly"); err == nil {
		t.Fatal("the hourly table is rebuilt, never copied: its progress row is refused")
	}
	if err := Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap over the replaced check: %v", err)
	}
	var checks int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		WHERE conrelid = 'sage.history_migration'::regclass AND contype = 'c'
		  AND pg_get_constraintdef(oid) LIKE '%table_name%'`).Scan(&checks); err != nil {
		t.Fatal(err)
	}
	if checks != 1 {
		t.Fatalf("%d table_name checks, want exactly one (the replaced one)", checks)
	}
}
