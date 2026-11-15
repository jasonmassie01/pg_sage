package schema

import (
	"context"
	"strings"
	"testing"
)

// evid-01a on a real server: bootstrap twice is clean and writes nothing,
// and the checks refuse a negative counter, an hour off the hour and an
// unknown epoch source. No concurrent-writer test: Bootstrap holds its
// advisory lock.

func TestQueryRollupMigration_BootstrapTwiceAndConstraints(t *testing.T) {
	pool := freshSchemaPool(t, "rollup")
	ctx := context.Background()
	for run := range 2 {
		if err := Bootstrap(ctx, pool); err != nil {
			t.Fatalf("bootstrap run %d: %v", run, err)
		}
	}
	var rows int64
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM sage.query_rollup_hourly) +
		(SELECT count(*) FROM sage.query_rollup_coverage)`).Scan(&rows); err != nil {
		t.Fatalf("count rollup rows: %v", err)
	}
	if rows != 0 {
		t.Fatalf("bootstrap wrote %d rollup rows, want 0", rows)
	}
	hourly := func(calls int64, hour string) error {
		_, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_hourly (queryid, hour,
			calls, exec_ms, rows, shared_blks_hit, shared_blks_read, temp_blks_read,
			temp_blks_written, wal_bytes, epoch_break, pairs)
			VALUES (42, $2::timestamptz, $1, 1.5, 1, 1, 1, 0, 0, 0, false, 1)`, calls, hour)
		return err
	}
	if err := hourly(5, "2026-11-01T10:00:00Z"); err != nil {
		t.Fatalf("a valid hourly row was refused: %v", err)
	}
	if err := hourly(-1, "2026-11-01T11:00:00Z"); err == nil ||
		!strings.Contains(err.Error(), "check") {
		t.Fatalf("a negative calls value must fail the CHECK, got %v", err)
	}
	if err := hourly(1, "2026-11-01T11:30:00Z"); err == nil {
		t.Fatal("an hour that is not on the hour must be refused")
	}
	if err := hourly(1, "2026-11-01T10:00:00Z"); err == nil ||
		!strings.Contains(err.Error(), "query_rollup_hourly_key") {
		t.Fatalf("a second row for (queryid, hour) must hit the unique key, got %v", err)
	}
	_, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_coverage (hour,
		sampled_exec_ms, sampled_queryids, epoch_breaks, snapshots, epoch_source)
		VALUES ('2026-11-01T10:00:00Z', 0, 0, 0, 0, 'guessed')`)
	if err == nil {
		t.Fatal("an unknown epoch_source must be refused")
	}
}
