package selfcost

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/selfcost"))
}

func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

// runAsSage runs statements on a pool configured like pg_sage's own and
// closes it, so its backend flushes its table statistics on exit.
func runAsSage(t *testing.T, ctx context.Context, statements ...string) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	selfmonitor.ConfigurePool(cfg)
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	for _, s := range statements {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// waitFor polls Read until cond holds (statistics are flushed by
// backends asynchronously), failing after a deadline.
func waitFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	cond func(Reading) bool) Reading {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		r, err := Read(ctx, pool)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if cond(r) {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition never held; last reading %+v", r)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Read measures pg_sage's own cost from the server's statistics: DB time
// of tagged statements in pg_stat_statements, rows read and written in the
// sage schema, and the schema's size. Another application's statements do
// not count.
func TestRead_MeasuresOnlyPgSageWork(t *testing.T) {
	pool, ctx := livePool(t)
	before, err := Read(ctx, pool)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !before.StatementsKnown {
		t.Skip("pg_stat_statements not preloaded on this server")
	}
	if before.Database == "" || before.SchemaBytes <= 0 || before.At.IsZero() {
		t.Fatalf("reading identity/size missing: %+v", before)
	}
	app, err := pgx.Connect(ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Exec(ctx, "SELECT pg_sleep(0.4) AS selfcost_app_probe"); err != nil {
		t.Fatal(err)
	}
	_ = app.Close(ctx)
	runAsSage(t, ctx,
		// Its own shape: pg_stat_statements keys entries by normalized
		// statement and user, so an identical statement would share the
		// application's entry and text.
		"SELECT pg_sleep(0.1) AS selfcost_sage_probe, 2 AS sage_shape",
		`INSERT INTO sage.health_history (database_name, health_score)
		 SELECT 'selfcost_probe', g FROM generate_series(1, 5) g`,
		"SELECT count(*) FROM sage.health_history")
	after := waitFor(t, ctx, pool, func(r Reading) bool {
		return r.RowsWritten-before.RowsWritten >= 5 && r.RowsRead-before.RowsRead >= 5
	})
	dbMs := after.DBTimeMs - before.DBTimeMs
	if dbMs < 100 {
		t.Errorf("DB time delta = %.1f ms, want >= 100 (pg_sage's pg_sleep(0.1))", dbMs)
	}
	if dbMs >= 400 {
		t.Errorf("DB time delta = %.1f ms: the application's 400 ms statement was counted", dbMs)
	}
	if after.Calls-before.Calls < 3 {
		t.Errorf("calls delta = %d, want >= 3", after.Calls-before.Calls)
	}
	if !after.At.After(before.At) {
		t.Errorf("reading time did not advance: %v -> %v", before.At, after.At)
	}
	c := Between(before, after, time.Minute)
	if !c.Known || !c.DBTimeKnown || c.RowsWrittenPerCycle <= 0 {
		t.Errorf("cost between real readings = %+v", c)
	}
}

func TestRead_CanceledContextIsAnError(t *testing.T) {
	pool, ctx := livePool(t)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Read(canceled, pool); err == nil {
		t.Fatal("Read on a canceled context returned no error")
	}
}
