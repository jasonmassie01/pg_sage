package collector

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// pg_sage is tracked by pg_stat_statements now (perf v1.8.3). Statements
// it sends through its own pools carry the tag, so the top-queries
// snapshot every analysis reads (optimizer, analyzer, query store, SLO
// latency proxy) never contains them; the application's statements stay.
func TestCollectQueriesExcludesStatementsFromPgSagePools(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension
		WHERE extname = 'pg_stat_statements')`).Scan(&exists); err != nil || !exists {
		t.Skip("pg_stat_statements not available")
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	selfmonitor.ConfigurePool(cfg)
	sage, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pg_sage pool: %v", err)
	}
	defer sage.Close()
	// A catalog read with no sage. reference and no literal tag in the
	// source: before universal tagging it reached the snapshot.
	if _, err := sage.Exec(ctx, `SELECT count(*) AS coll_self_probe FROM pg_class
		WHERE relkind = 'r'`); err != nil {
		t.Fatalf("pg_sage statement: %v", err)
	}
	app, err := pgx.Connect(ctx, os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("app connection: %v", err)
	}
	defer func() { _ = app.Close(context.Background()) }()
	if _, err := app.Exec(ctx, `SELECT count(*) AS coll_app_probe, 1 AS shape FROM pg_class
		WHERE relkind = 'i'`); err != nil {
		t.Fatalf("application statement: %v", err)
	}
	cfgC := testConfig()
	cfgC.Collector.MaxQueries = 5000
	queries, err := New(pool, cfgC, 170000, noopLog).collectQueries(ctx)
	if err != nil {
		t.Fatalf("collectQueries: %v", err)
	}
	var sawApp bool
	for _, q := range queries {
		if strings.Contains(q.Query, "coll_self_probe") {
			t.Fatalf("pg_sage's own statement in the top-queries snapshot: %q", q.Query)
		}
		sawApp = sawApp || strings.Contains(q.Query, "coll_app_probe")
	}
	if !sawApp {
		t.Fatalf("application statement missing from %d collected queries", len(queries))
	}
}
