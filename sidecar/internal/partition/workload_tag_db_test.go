package partition

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// partitionStatementMarks pick out statements only this package runs.
var partitionStatementMarks = []string{
	"pg_try_advisory_lock(pg_catalog.hashtext",
	"pg_get_indexdef(i.indexrelid)",
	"pg_get_serial_sequence",
	"relpartbound",
	"pg_advisory_xact_lock(",
}

// The conversion and partition upkeep run on the monitored database, so
// pg_stat_statements records them. They are pg_sage's own statements and
// must carry its tag, or Ask Sage and the MCP top_queries tool list them
// as the operator's workload (t2-test-round2).
// Other packages' tests share the server and database and reset
// pg_stat_statements (some scoped to this database, which leaves
// pg_stat_statements_info unchanged), so a probe statement run first tells
// interference from a real miss: the attempt repeats only when the probe
// is gone (t2-test-round3).
// The package's functions run on a pool configured as pg_sage's own
// (selfmonitor.ConfigurePool), as in production: its wire tagger puts the
// tag after the first keyword, where PostgreSQL 18 keeps it. The setup and
// the reads use the plain pool, so they stay out of the measurement.
// No concurrent access test: the statements are constants; the
// concurrent conversion paths are covered in convert_safe_db_test.go.
func TestStatementsCarryThePgSageTag(t *testing.T) {
	pool, ctx := requireDB(t)
	exec(t, ctx, pool, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	const attempts = 3
	for i := 1; i <= attempts; i++ {
		recorded, probed := partitionStatements(t, ctx, pool)
		if !probed {
			t.Logf("attempt %d: pg_stat_statements was reset or evicted during the "+
				"measurement (probe gone); repeating", i)
			continue
		}
		for _, q := range recorded {
			if !selfmonitor.IsStatementText(q) {
				t.Errorf("an untagged pg_sage statement reads as workload: %s", q)
			}
		}
		if len(recorded) < len(partitionStatementMarks) {
			t.Fatalf("premise: %d partition statements recorded, want at least %d",
				len(recorded), len(partitionStatementMarks))
		}
		return
	}
	t.Fatalf("pg_stat_statements was reset or evicted during each of %d attempts", attempts)
}

// partitionStatements resets this database's statistics, runs a probe, a
// conversion, upkeep and a listing, and returns the recorded statements
// that carry a partition mark and whether the probe survived.
func partitionStatements(t *testing.T, ctx context.Context,
	pool *pgxpool.Pool) ([]string, bool) {
	t.Helper()
	exec(t, ctx, pool, `SELECT pg_stat_statements_reset(0, (SELECT oid FROM pg_database
		WHERE datname = current_database()), 0)`)
	var b [4]byte
	_, _ = rand.Read(b[:])
	probe := fmt.Sprintf("partition_tag_probe_%x", b)
	exec(t, ctx, pool, "SELECT 1 AS "+probe)
	tbl := scratch(t, ctx, pool, []string{"id"})
	fill(t, ctx, pool, tbl, 200)
	sage := sagePool(t, ctx, pool)
	if res, err := Convert(ctx, sage, tbl); err != nil || !res.Converted {
		t.Fatalf("Convert = %+v, %v", res, err)
	}
	if _, err := Ensure(ctx, sage, tbl, time.Now(), 3); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if parts, err := List(ctx, sage, tbl); err != nil || len(parts) == 0 {
		t.Fatalf("List = %v, %v", parts, err)
	}
	return recordedStatements(t, ctx, pool, probe)
}

// sagePool is a pool on pool's database configured as pg_sage's own.
func sagePool(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config()
	selfmonitor.ConfigurePool(cfg)
	sage, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect a pg_sage pool: %v", err)
	}
	t.Cleanup(sage.Close)
	return sage
}

// recordedStatements reads this database's pg_stat_statements texts.
func recordedStatements(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	probe string) ([]string, bool) {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT query FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`)
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	defer rows.Close()
	var marked []string
	probed := false
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			t.Fatalf("scan statement: %v", err)
		}
		probed = probed || strings.Contains(q, probe)
		if hasPartitionMark(q) {
			marked = append(marked, q)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	return marked, probed
}

func hasPartitionMark(q string) bool {
	for _, m := range partitionStatementMarks {
		if strings.Contains(q, m) {
			return true
		}
	}
	return false
}
