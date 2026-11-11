package selfcost_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/selfcost"
	"github.com/pg-sage/sidecar/internal/selfmonitor"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
)

// footprint-08 integration (real PostgreSQL with pg_stat_statements): the
// statements one collector cycle sends reach pg_stat_statements with their
// component, so the meter attributes them to the collector.

// sagePool is a pool configured like pg_sage's own (tagged statements,
// pg_sage application name), on a bootstrapped schema.
func sagePool(t *testing.T, ctx context.Context) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	selfmonitor.ConfigurePool(cfg)
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

func collectorConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Collector.IntervalSeconds = 60
	cfg.Collector.BatchSize = 100
	cfg.Collector.MaxQueries = 50
	cfg.HasWALColumns = true
	cfg.HasPlanTimeColumns = true
	return cfg
}

// runOneCycle runs the collector until its first snapshot (Run collects
// at once), then stops it.
func runOneCycle(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").
		Scan(&version); err != nil {
		t.Fatalf("server version: %v", err)
	}
	c := collector.New(pool, collectorConfig(), version, func(string, string, ...any) {})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); c.Run(runCtx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(60 * time.Second)
	for c.LatestSnapshot() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the collector produced no snapshot within 60 s")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestComponent_CollectorCycleIsAttributed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	pool := sagePool(t, ctx)
	first, err := selfcost.Read(ctx, pool)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !first.StatementsKnown {
		t.Skip("pg_stat_statements not preloaded on this server")
	}
	pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
		before, err := selfcost.Read(ctx, pool)
		if err != nil {
			return []string{fmt.Sprintf("Read before: %v", err)}
		}
		runOneCycle(t, ctx, pool)
		after, err := selfcost.Read(ctx, pool)
		if err != nil {
			return []string{fmt.Sprintf("Read after: %v", err)}
		}
		return collectorProblems(before, after)
	})
}

// collectorProblems checks the window's collector entries: at least one
// entry tagged collector did work, and the meter's rows name it.
func collectorProblems(before, after selfcost.Reading) []string {
	var problems []string
	tagged := 0
	for k, c := range after.Statements {
		if selfcost.Component(c.Text) != "collector" {
			continue
		}
		if c.Calls > before.Statements[k].Calls {
			tagged++
		}
	}
	if tagged == 0 {
		problems = append(problems, "no pg_stat_statements entry with component=collector "+
			"did work during the collector cycle")
	}
	m := selfcost.NewMeter()
	m.Observe(before, time.Minute)
	m.Observe(after, time.Minute)
	found := false
	for _, row := range m.ByComponent() {
		if row.Component == "collector" && row.CallsPerHour > 0 && row.DBTimeMsPerHour > 0 {
			found = true
		}
	}
	if !found {
		problems = append(problems, fmt.Sprintf("by_component %+v has no collector row "+
			"with calls and database time", m.ByComponent()))
	}
	return problems
}
