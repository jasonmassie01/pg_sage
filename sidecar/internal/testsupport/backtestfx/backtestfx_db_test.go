package backtestfx

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Integration (real PostgreSQL): Persist writes three snapshots a
// collection through snapstore and the hourly rollup counts every cycle
// but the one ending at the statistics reset.

func fixturePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(t.Context(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(t.Context(), pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, table := range []string{"snapshots", "query_rollup_hourly", "query_rollup_coverage"} {
		if _, err := pool.Exec(t.Context(), "DELETE FROM sage."+table); err != nil {
			t.Fatalf("clear sage.%s: %v", table, err)
		}
	}
	return pool
}

func TestPersistWritesSnapshotsAndTheHourlyRollup(t *testing.T) {
	pool := fixturePool(t)
	first := time.Now().UTC().Truncate(time.Hour).Add(-4 * time.Hour)
	const cycles = 8 // two hours of 15-minute cycles, every hour closed
	src := fixture(first, cycles, first.Add(time.Hour))
	Persist(t, pool, src, 15*time.Minute, 1)

	rows, err := pool.Query(t.Context(), `SELECT category, count(*) FROM sage.snapshots
		WHERE collected_at BETWEEN $1 AND $2 GROUP BY category ORDER BY category`,
		src.First, src.Last)
	if err != nil {
		t.Fatalf("count the snapshots: %v", err)
	}
	got := map[string]int{}
	for rows.Next() {
		var category string
		var n int
		if err := rows.Scan(&category, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[category] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the snapshot counts: %v", err)
	}
	for _, c := range []string{"queries", "tables", "system"} {
		if got[c] != cycles+1 {
			t.Errorf("%s snapshots = %d, want one per collection (%d)", c, got[c], cycles+1)
		}
	}

	// The cycle ending at the reset counts nothing; the other seven count.
	for q, perCycle := range map[int64]int64{1: 10, 2: 3} {
		var calls int64
		if err := pool.QueryRow(t.Context(), `SELECT coalesce(sum(calls), 0)
			FROM sage.query_rollup_hourly WHERE queryid = $1`, q).Scan(&calls); err != nil {
			t.Fatalf("sum the rollup of q%d: %v", q, err)
		}
		if want := perCycle * (cycles - 1); calls != want {
			t.Errorf("rollup calls of q%d = %d, want %d", q, calls, want)
		}
	}
}
