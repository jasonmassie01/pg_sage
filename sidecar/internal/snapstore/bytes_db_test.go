package snapstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/partition"
	"github.com/pg-sage/sidecar/internal/testsupport/snapfixture"
)

// categoryBytes returns the stored jsonb bytes per category.
func categoryBytes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT category, sum(pg_column_size(data))::bigint
		FROM sage.snapshots GROUP BY category`)
	if err != nil {
		t.Fatalf("category bytes: %v", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var cat string
		var n int64
		if err := rows.Scan(&cat, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[cat] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func relationBytes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	// sage.snapshots is partitioned by day: its own size is 0, the
	// partitions hold the data.
	n, err := partition.Size(ctx, pool, partition.Snapshots)
	if err != nil {
		t.Fatalf("relation size: %v", err)
	}
	return n
}

// One hour of collection (60 cycles, one a minute) on a catalog of 250
// tables and 5,250 indexes: the delta store must write at least 10x fewer
// bytes than the legacy format, for the indexes category and for the whole
// sage.snapshots relation (heap, TOAST and indexes), and still read back
// every document identically.
func TestBytesPerHour_5000Indexes(t *testing.T) {
	deltaPool, legacyPool, ctx := goldenStores(t)
	sc := snapfixture.Scenario{
		Start:   time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Minute),
		Step:    time.Minute,
		Cycles:  60,
		Tables:  250,
		Indexes: 5000,
		Seed:    5000,
		Events:  snapfixture.Events{QueryChurn: 10},
	}
	cycles, err := sc.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	beforeD, beforeL := relationBytes(t, ctx, deltaPool), relationBytes(t, ctx, legacyPool)
	writeBoth(t, ctx, deltaPool, legacyPool, cycles)
	relD := relationBytes(t, ctx, deltaPool) - beforeD
	relL := relationBytes(t, ctx, legacyPool) - beforeL
	catD, catL := categoryBytes(t, ctx, deltaPool), categoryBytes(t, ctx, legacyPool)
	for _, cat := range []string{"indexes", "tables", "sequences", "queries",
		"foreign_keys", "partitions", "system", "locks", "config_data"} {
		t.Logf("%-12s legacy %10d B/h  delta %9d B/h  reduction %6.1fx", cat, catL[cat],
			catD[cat], ratio(catL[cat], catD[cat]))
	}
	t.Logf("relation     legacy %10d B/h  delta %9d B/h  reduction %6.1fx", relL, relD,
		ratio(relL, relD))
	if r := ratio(catL["indexes"], catD["indexes"]); r < 10 {
		t.Errorf("indexes reduction %.1fx, want >= 10x", r)
	}
	if r := ratio(relL, relD); r < 10 {
		t.Errorf("sage.snapshots reduction %.1fx, want >= 10x", r)
	}
	got, want := readAll(t, ctx, deltaPool), readAll(t, ctx, legacyPool)
	if len(got) != len(want) {
		t.Fatalf("%d vs %d rows", len(got), len(want))
	}
	for i := range want {
		if got[i].data != want[i].data {
			t.Fatalf("row %d (%s) differs", i, want[i].cat)
		}
	}
}

func ratio(legacy, delta int64) float64 {
	if delta <= 0 {
		return 0
	}
	return float64(legacy) / float64(delta)
}
