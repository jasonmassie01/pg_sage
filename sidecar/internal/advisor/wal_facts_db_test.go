//go:build integration

package advisor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/collector"
)

// advisors-05 on real PostgreSQL: the window-start rows come from
// sage.snapshots through the history store, the system row decoded as the
// collector wrote it and the tables row through snapstore.DataSQL.

func insertTablesRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	tables []collector.TableStats, age time.Duration) {
	t.Helper()
	data, err := json.Marshal(tables)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	err = pool.QueryRow(ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES (now() - make_interval(secs => $1), 'tables', $2) RETURNING id`,
		age.Seconds(), data).Scan(&id)
	if err != nil {
		t.Fatalf("insert tables snapshot: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM sage.snapshots WHERE id = $1`, id)
	})
}

func TestWindowHistory_LivePG_SystemStart(t *testing.T) {
	pool := runsPool(t)
	ctx := context.Background()
	clearSystemRows(t, ctx, pool)
	hist := pgWindowHistory{db: pool}
	if _, found, err := hist.readSystemStart(ctx, 24*time.Hour); err != nil || found {
		t.Fatalf("empty history: found=%v err=%v, want no row", found, err)
	}
	insertSystemRow(t, ctx, pool, walCounters(1, 1, 1), 30*time.Hour) // outside
	insertSystemRow(t, ctx, pool, walCounters(100, 50, 10<<30), 20*time.Hour)
	insertSystemRow(t, ctx, pool, walCounters(120, 51, 15<<30), time.Hour)
	start, found, err := hist.readSystemStart(ctx, 24*time.Hour)
	if err != nil || !found {
		t.Fatalf("readSystemStart: found=%v err=%v", found, err)
	}
	s := start.sys
	if s.CheckpointsRequested != 100 || s.CheckpointsTimed != 50 || s.WALBytes != 10<<30 ||
		!s.CheckpointStatsEpoch.Equal(ckptEpoch) || !s.WALStatsEpoch.Equal(walStatEpoch) {
		t.Errorf("start row = %+v, want the 20h-old row decoded", s)
	}
	if age := time.Since(start.at); age < 19*time.Hour || age > 21*time.Hour {
		t.Errorf("start row at %v (%s ago), want about 20h ago", start.at, age)
	}
	snap := walWindowSnap("1024", "300", walCounters(130, 52, 22<<30))
	snap.CollectedAt = time.Now()
	f, err := walFacts(ctx, hist, snap, 24*time.Hour)
	if err != nil || !f.known || f.requestedRatio == nil || *f.requestedRatio != 0.9375 {
		t.Fatalf("facts = %+v err=%v, want a known 30/32 ratio", f, err)
	}
}

func TestWindowHistory_LivePG_TablesStart(t *testing.T) {
	pool := runsPool(t)
	ctx := context.Background()
	const clear = `DELETE FROM sage.snapshots WHERE category = 'tables'`
	if _, err := pool.Exec(ctx, clear); err != nil {
		t.Fatal(err)
	}
	hist := pgWindowHistory{db: pool}
	keys := []tableKey{{"public", "a"}, {"app", "b"}, {"public", "missing"}}
	if _, found, err := hist.readTablesStart(ctx, 24*time.Hour, keys); err != nil || found {
		t.Fatalf("empty history: found=%v err=%v", found, err)
	}
	insertTablesRow(t, ctx, pool, []collector.TableStats{
		{SchemaName: "public", RelName: "a", NLiveTup: 1, NDeadTup: 1}}, 30*time.Hour)
	insertTablesRow(t, ctx, pool, []collector.TableStats{
		{SchemaName: "public", RelName: "a", NLiveTup: 900, NDeadTup: 100},
		{SchemaName: "app", RelName: "b", NLiveTup: 500, NDeadTup: 500},
		{SchemaName: "public", RelName: "other", NLiveTup: 7, NDeadTup: 7},
	}, 12*time.Hour)
	insertTablesRow(t, ctx, pool, []collector.TableStats{
		{SchemaName: "public", RelName: "a", NLiveTup: 5, NDeadTup: 5}}, time.Hour)
	start, found, err := hist.readTablesStart(ctx, 24*time.Hour, keys)
	if err != nil || !found {
		t.Fatalf("readTablesStart: found=%v err=%v", found, err)
	}
	want := map[tableKey]deadCount{{"public", "a"}: {live: 900, dead: 100},
		{"app", "b"}: {live: 500, dead: 500}}
	if len(start.tables) != len(want) {
		t.Fatalf("tables = %v, want %v (filtered to the asked names)", start.tables, want)
	}
	for k, v := range want {
		if start.tables[k] != v {
			t.Errorf("%v = %+v, want %+v", k, start.tables[k], v)
		}
	}
	if age := time.Since(start.at); age < 11*time.Hour || age > 13*time.Hour {
		t.Errorf("start row %s old, want the 12h-old row", age)
	}
}
