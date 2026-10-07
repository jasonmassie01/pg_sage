//go:build perfgate

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// perfHistoryDatabaseID is the monitored database's id in the gate's store;
// perfHistoryOthers are two more databases whose history shares the
// store's tables, so every history read must find its rows by index.
const perfHistoryDatabaseID = 1

var perfHistoryOthers = []int{2, 3}

// TestPerfGateHistoryMeta is the performance gate with history.store:
// meta. The same fixture as TestPerfGate is built, then its snapshots and
// query store are moved into a second database (the meta store) with the
// real migration, next to two other databases' copies. The standalone
// runtime then runs with its history in the store (only the history
// placement differs from TestPerfGate), and both databases are charged
// the same budgets: the monitored database's phases and the store's.
//
// Run: go test -tags=perfgate -run '^TestPerfGateHistoryMeta$' ./cmd/pg_sage_sidecar
func TestPerfGateHistoryMeta(t *testing.T) {
	scale, err := perfgate.ScaleFromEnv(os.Getenv)
	if err != nil {
		t.Fatalf("scale: %v", err)
	}
	timing, err := perfgate.TimingFromEnv(os.Getenv)
	if err != nil {
		t.Fatalf("timing: %v", err)
	}
	ctx := context.Background()
	dsn := testdb.CreateDatabase(t, "perfgate")
	metaDSN := testdb.CreateDatabase(t, "perfgate_meta")
	harness, metaHarness := perfHarnessPool(t, dsn), perfHarnessPool(t, metaDSN)
	buildPerfFixture(t, ctx, harness, scale)
	buildPerfStore(t, ctx, harness, metaHarness)
	phases := runPerfRuntimeWithStore(t, ctx, harness, metaHarness, dsn, metaDSN, scale,
		timing)
	budgets := perfgate.DefaultBudgets()
	offenders, err := perfgate.Evaluate(phases, budgets)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	report := perfgate.RenderMarkdown(scale, budgets, phases, offenders)
	writePerfReport(t, report)
	if len(offenders) > 0 {
		t.Errorf("performance gate (history in the meta database): %d offenders", len(offenders))
	}
}

// buildPerfStore bootstraps the store and moves the fixture's history in.
func buildPerfStore(t *testing.T, ctx context.Context, harness, meta *pgxpool.Pool) {
	t.Helper()
	start := time.Now()
	if err := perfgate.Prepare(ctx, meta); err != nil {
		t.Fatalf("prepare meta database: %v", err)
	}
	if err := schema.Bootstrap(ctx, meta); err != nil {
		t.Fatalf("bootstrap meta database: %v", err)
	}
	if err := schema.BootstrapHistoryStore(ctx, meta); err != nil {
		t.Fatalf("bootstrap history store: %v", err)
	}
	err := perfgate.MoveHistoryToStore(ctx, harness, meta, perfHistoryDatabaseID,
		perfHistoryOthers)
	if err != nil {
		t.Fatalf("move history to the store: %v", err)
	}
	if err := perfgate.AnalyzeSage(ctx, meta); err != nil {
		t.Fatalf("analyze store: %v", err)
	}
	t.Logf("history moved to the store in %s", time.Since(start).Round(time.Second))
}
