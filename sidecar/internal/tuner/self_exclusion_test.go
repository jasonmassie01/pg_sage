package tuner

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// pg_sage is tracked by pg_stat_statements now (perf v1.8.3): a slow
// statement pg_sage itself runs must never become a hint candidate, while
// the same kind of slow statement from an application still does.
func TestFetchCandidatesNeverOffersPgSageStatements(t *testing.T) {
	pool, ctx := requireTunerDB(t)
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_extension
		WHERE extname = 'pg_stat_statements')`).Scan(&exists); err != nil || !exists {
		t.Skip("pg_stat_statements not available")
	}
	cfg, err := pgxpool.ParseConfig(tunerTestDSN())
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	selfmonitor.ConfigurePool(cfg)
	sage, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pg_sage pool: %v", err)
	}
	defer sage.Close()
	app, err := pgx.Connect(ctx, tunerTestDSN())
	if err != nil {
		t.Fatalf("app connection: %v", err)
	}
	defer func() { _ = app.Close(context.Background()) }()
	for i := 0; i < 2; i++ {
		if _, err := sage.Exec(ctx, "SELECT pg_sleep(0.12) AS tuner_self_probe"); err != nil {
			t.Fatalf("pg_sage statement: %v", err)
		}
		if _, err := app.Exec(ctx,
			"SELECT pg_sleep(0.12) AS tuner_app_probe, 1 AS shape"); err != nil {
			t.Fatalf("application statement: %v", err)
		}
	}
	tu := New(pool, TunerConfig{MinQueryCalls: 1, PlanTimeRatio: 0.5}, nil, noopLog2)
	candidates, err := tu.fetchCandidates(ctx)
	if err != nil {
		t.Fatalf("fetchCandidates: %v", err)
	}
	var sawApp bool
	for _, c := range candidates {
		if strings.Contains(c.Query, "tuner_self_probe") {
			t.Fatalf("pg_sage's own statement offered for a hint: %q", c.Query)
		}
		sawApp = sawApp || strings.Contains(c.Query, "tuner_app_probe")
	}
	if !sawApp {
		t.Fatalf("the application's slow statement is missing from %d candidates: the "+
			"exclusion is too broad", len(candidates))
	}
}
