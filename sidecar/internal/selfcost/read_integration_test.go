package selfcost

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
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
	first, err := Read(ctx, pool)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !first.StatementsKnown {
		t.Skip("pg_stat_statements not preloaded on this server")
	}
	if first.Database == "" || first.SchemaBytes <= 0 || first.At.IsZero() {
		t.Fatalf("reading identity/size missing: %+v", first)
	}
	// Another package's pg_stat_statements_reset() on the shared server
	// makes the statement deltas negative: repeat the window then.
	pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
		before, after := measureAppAndSage(t, ctx, pool)
		return append(readProblems(before, after), entryProblems(t, ctx, pool, before, after)...)
	})
}

// measureAppAndSage reads, runs a 400 ms application statement and pg_sage
// work (a 100 ms statement, 5 sage rows written and read), and reads again.
func measureAppAndSage(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (Reading,
	Reading) {
	t.Helper()
	before, err := Read(ctx, pool)
	if err != nil {
		t.Fatalf("Read: %v", err)
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
	return before, after
}

// readProblems checks the window counted pg_sage's work. Whether the
// application's statement was counted is checked per entry (entryProblems):
// other packages' pg_sage-tagged work on the shared CI server also lands in
// the window's sums (PG14 CI: 405 ms with the application not counted).
func readProblems(before, after Reading) []string {
	var problems []string
	dbMs := after.DBTimeMs - before.DBTimeMs
	if dbMs < 100 {
		problems = append(problems, fmt.Sprintf(
			"DB time delta = %.1f ms, want >= 100 (pg_sage's pg_sleep(0.1))", dbMs))
	}
	if d := after.Calls - before.Calls; d < 3 {
		problems = append(problems, fmt.Sprintf("calls delta = %d, want >= 3", d))
	}
	if !after.At.After(before.At) {
		problems = append(problems, fmt.Sprintf("reading time did not advance: %v -> %v",
			before.At, after.At))
	}
	c := Between(before, after, time.Minute)
	if !c.Known || !c.DBTimeKnown || c.RowsWrittenPerCycle <= 0 {
		problems = append(problems, fmt.Sprintf("cost between real readings = %+v", c))
	}
	return problems
}

// entryProblems checks the readings by pg_stat_statements entry: the
// application's probe is never among pg_sage's statements, and pg_sage's
// probe is, with at least its 100 ms of sleep in the window.
func entryProblems(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	before, after Reading) []string {
	t.Helper()
	var problems []string
	for _, k := range probeKeys(t, ctx, pool, "selfcost_app_probe") {
		if _, counted := after.Statements[k]; counted {
			problems = append(problems, fmt.Sprintf(
				"the application's statement (queryid %d) was counted", k.QueryID))
		}
	}
	sage := probeKeys(t, ctx, pool, "selfcost_sage_probe")
	var sageMs float64
	for _, k := range sage {
		sageMs += after.Statements[k].TimeMs - before.Statements[k].TimeMs
	}
	if len(sage) == 0 || sageMs < 100 {
		problems = append(problems, fmt.Sprintf(
			"pg_sage's probe: %d entries, %.1f ms in the window, want >= 100", len(sage),
			sageMs))
	}
	return problems
}

// probeKeys returns the pg_stat_statements keys of this database's entries
// whose text contains marker.
func probeKeys(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	marker string) []StatementKey {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT userid, queryid, toplevel FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND queryid IS NOT NULL AND strpos(query, $1) > 0`, marker)
	if err != nil {
		t.Fatalf("probe entries %s: %v", marker, err)
	}
	keys, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (StatementKey, error) {
		var k StatementKey
		err := r.Scan(&k.UserID, &k.QueryID, &k.TopLevel)
		return k, err
	})
	if err != nil {
		t.Fatalf("scan probe entries %s: %v", marker, err)
	}
	return keys
}

func TestRead_CanceledContextIsAnError(t *testing.T) {
	pool, ctx := livePool(t)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Read(canceled, pool); err == nil {
		t.Fatal("Read on a canceled context returned no error")
	}
}
