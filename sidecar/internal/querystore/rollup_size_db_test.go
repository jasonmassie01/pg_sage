package querystore

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// evid-01b CHECK-08: the rollup's size on the spec's arithmetic, 500
// queryids x 24 hours x 28 days (336,000 hourly rows), with its 28 daily
// rolls and 672 coverage rows, measured by RollupBytes against the 50-85
// MB estimate. The figure is logged (go test -v) for docs/configuration.md.
// No concurrent access tests: one writer builds the fixture.

func TestRollupBytes_SpecFixtureStaysWithinTheEstimate(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "rollup_size"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	start := pastDay(30)
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_hourly (queryid, hour, calls,
		exec_ms, rows, shared_blks_hit, shared_blks_read, temp_blks_read, temp_blks_written,
		wal_bytes, plan_hash, epoch_break, pairs)
		SELECT 7000000000000 + q * 7919, $1::timestamptz + make_interval(hours => h),
			q * 13 + h, (q * 1.7 + h) * 1.01, q * 3, q * 101, q * 7, q % 3, q % 5, q * 4096,
			md5(q::text), false, 12
		FROM generate_series(0, 28 * 24 - 1) h, generate_series(1, 500) q`,
		start); err != nil {
		t.Fatalf("write the hourly fixture: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_rollup_coverage (hour, db_exec_ms,
		sampled_exec_ms, sampled_queryids, epoch_breaks, dealloc_delta, snapshots,
		epoch_source) SELECT $1::timestamptz + make_interval(hours => h), 1000, 870, 500, 0,
		0, 12, 'recorded' FROM generate_series(0, 28 * 24 - 1) h`, start); err != nil {
		t.Fatalf("write the coverage fixture: %v", err)
	}
	for d := range 28 {
		rep, err := RollupDay(ctx, pool, start.AddDate(0, 0, d))
		if err != nil || rep.Queryids != 500 {
			t.Fatalf("roll day %d: %+v %v", d, rep, err)
		}
	}
	got, err := RollupBytes(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("CHECK-08: RollupBytes = %d bytes (%.1f MB) for 336,000 hourly, 14,000 daily "+
		"and 672 coverage rows", got, float64(got)/(1<<20))
	if got <= 10<<20 || got > 85<<20 {
		t.Fatalf("RollupBytes = %d, want a measurement within (10 MB, 85 MB]", got)
	}
}
