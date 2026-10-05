//go:build perfgate

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// TestPerfGate is pg_sage's permanent performance gate. It builds a large
// synthetic monitored database (catalog plus pre-seeded sage history), runs
// the real standalone runtime in-process against it with pg_stat_statements
// tracking pg_sage's own sessions, and fails with a ranked offender list
// when pg_sage's SQL needs a DBA: a sequential scan of a large sage table,
// a slow statement, too much DB time or too many rows written per cycle, a
// catalog query over the incident budget, a slow API list endpoint, or
// too much sidecar CPU per cycle.
// Budgets live in internal/testsupport/perfgate/budgets.go.
//
// Run: go test -tags=perfgate -run '^TestPerfGate$' ./cmd/pg_sage_sidecar
// Scale and timing: PG_SAGE_PERF_SCALE=small|large, PG_SAGE_PERF_TABLES,
// PG_SAGE_PERF_HISTORY_ROWS, PG_SAGE_PERF_INTERVAL, PG_SAGE_PERF_WARMUP,
// PG_SAGE_PERF_WINDOW; PG_SAGE_PERF_ENDPOINT_SAMPLES (default 5) is the
// number of measured calls per API endpoint (median charged, after one
// warm-up call); PG_SAGE_PERF_REPORT names the markdown report file.
func TestPerfGate(t *testing.T) {
	scale, err := perfgate.ScaleFromEnv(os.Getenv)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	timing, err := perfgate.TimingFromEnv(os.Getenv)
	if err != nil {
		t.Fatalf("timing: %v", err)
	}
	dsn := testdb.CreateDatabase(t, "perfgate")
	ctx := context.Background()
	harness := perfHarnessPool(t, dsn)
	buildPerfFixture(t, ctx, harness, scale)
	phases := runPerfRuntime(t, ctx, harness, dsn, scale, timing)
	budgets := perfgate.DefaultBudgets()
	offenders, err := perfgate.Evaluate(phases, budgets)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	report := perfgate.RenderMarkdown(scale, budgets, phases, offenders)
	writePerfReport(t, report)
	if len(offenders) > 0 {
		t.Errorf("performance gate: %d offenders (ranked report above)", len(offenders))
	}
}

// buildPerfFixture creates the catalog and the sage history, then gathers
// statistics as autovacuum would have on a long-running deployment.
func buildPerfFixture(
	t *testing.T, ctx context.Context, harness *pgxpool.Pool, scale perfgate.Scale,
) {
	t.Helper()
	start := time.Now()
	if err := perfgate.Prepare(ctx, harness); err != nil {
		t.Fatalf("prepare (pg_stat_statements must be preloaded): %v", err)
	}
	if err := schema.Bootstrap(ctx, harness); err != nil {
		t.Fatalf("bootstrap sage schema: %v", err)
	}
	if err := perfgate.BuildCatalog(ctx, harness, scale); err != nil {
		t.Fatalf("build catalog: %v", err)
	}
	t.Logf("catalog: %d tables, %d indexes, %d sequences in %s", scale.Tables(),
		scale.Indexes(), scale.Sequences(), time.Since(start).Round(time.Second))
	seeded := time.Now()
	own := perfgate.NewBinding("startup:" + harness.Config().ConnConfig.Database)
	if err := perfgate.SeedHistory(ctx, harness, scale, own); err != nil {
		t.Fatalf("seed sage history: %v", err)
	}
	if err := perfgate.AnalyzeSage(ctx, harness); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	t.Logf("history: %d rows per growing table in %s", scale.HistoryRows,
		time.Since(seeded).Round(time.Second))
	logPerfFixtureSize(t, ctx, harness)
}

func logPerfFixtureSize(t *testing.T, ctx context.Context, harness *pgxpool.Pool) {
	t.Helper()
	var size string
	err := harness.QueryRow(ctx, "/* "+perfgate.HarnessTag+" */ "+
		"SELECT pg_size_pretty(pg_database_size(current_database()))").Scan(&size)
	if err != nil {
		t.Fatalf("fixture size: %v", err)
	}
	t.Logf("fixture database size: %s", size)
}

func writePerfReport(t *testing.T, report string) {
	t.Helper()
	t.Log("\n" + report)
	path := os.Getenv("PG_SAGE_PERF_REPORT")
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("report directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
		t.Fatalf("write report: %v", err)
	}
}
