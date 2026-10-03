package optimizer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// static.md F11: the optimizer's context builder read columns, pg_stats
// and the collation with no server timeout. It now reads through
// catalogread; a slow read is cut off and the context is built without
// that part instead of hanging the optimizer cycle.

func ctxFixture(t *testing.T) (*pgxpool.Pool, *collector.Snapshot) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public.ctx_bounded")
	})
	if _, err := pool.Exec(context.Background(), `CREATE TABLE public.ctx_bounded
		(id int, v int); INSERT INTO public.ctx_bounded SELECT g, g % 7
		FROM generate_series(1, 500) g; ANALYZE public.ctx_bounded`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	snap := &collector.Snapshot{
		Tables: []collector.TableStats{{SchemaName: "public", RelName: "ctx_bounded",
			NLiveTup: 500}},
		Queries: []collector.QueryStats{{QueryID: 1, Calls: 100, MeanExecTime: 5,
			Query: "SELECT * FROM public.ctx_bounded WHERE v = $1"}},
	}
	return pool, snap
}

func TestBuildTableContexts_SlowCatalogReadsDegrade(t *testing.T) {
	pool, snap := ctxFixture(t)
	reader := catalogread.New(pool, catalogread.Timeouts{Statement: 200 * time.Millisecond})
	base, _, err := BuildTableContexts(context.Background(), reader, snap, nil, 0)
	if err != nil || len(base) != 1 || len(base[0].Columns) != 2 || len(base[0].ColStats) == 0 {
		t.Fatalf("baseline contexts = %+v (%v), want columns and stats", base, err)
	}
	var calls atomic.Int32
	slow := catalogread.WithBeforeStatement(context.Background(),
		func(ctx context.Context, tx pgx.Tx) error {
			calls.Add(1)
			_, err := tx.Exec(ctx, "SELECT pg_sleep(10)")
			return err
		})
	start := time.Now()
	got, _, err := BuildTableContexts(slow, reader, snap, nil, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("contexts under slow reads = %+v (%v), want the table without details",
			got, err)
	}
	if got[0].Columns != nil || got[0].ColStats != nil || got[0].Collation != "" {
		t.Fatalf("timed-out reads produced data: %+v", got[0])
	}
	if calls.Load() != 3 {
		t.Fatalf("%d reads went through the bounded helper, want 3 (collation, "+
			"columns, stats)", calls.Load())
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("context build under slow reads took %s", el)
	}
}

func TestOptimizer_CatalogReadTimeouts(t *testing.T) {
	o := New(nil, nil, nil, &config.OptimizerConfig{}, 170000, 0,
		func(string, string, ...any) {})
	if o.catalogTimeouts != catalogread.Default() {
		t.Fatalf("default timeouts = %+v", o.catalogTimeouts)
	}
	want := catalogread.Timeouts{Statement: time.Second, Lock: 2 * time.Second}
	o = New(nil, nil, nil, &config.OptimizerConfig{}, 170000, 0,
		func(string, string, ...any) {}, WithCatalogReadTimeouts(want))
	if o.catalogTimeouts != want {
		t.Fatalf("timeouts = %+v, want %+v", o.catalogTimeouts, want)
	}
}
