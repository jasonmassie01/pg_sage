//go:build perfgate

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/firstlook"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// First-look budgets: one catalog pass over 10k+ relations finishes well
// inside the first minute, no statement exceeds the catalog incident
// budget, and no user table is scanned.
const (
	firstLookSmallBudget = 5 * time.Second
	firstLookLargeBudget = 20 * time.Second
	firstLookMinRelation = 10000
)

// firstLookScale is the catalog the gate builds: small runs 1,500 tables
// (about 10.5k relations: table, toast table and index, primary key, two
// indexes, sequence); large runs the nightly 5,000-table catalog.
func firstLookScale(t *testing.T) (perfgate.Scale, time.Duration) {
	t.Helper()
	switch os.Getenv(perfgate.EnvScale) {
	case "", "small":
		return perfgate.Scale{Name: "first-look-small", Schemas: 30, CloneSchemas: 12,
			TablesPerSchema: 50, IndexesPerTable: 3, HistoryRows: 100}, firstLookSmallBudget
	case "large":
		return perfgate.LargeScale(), firstLookLargeBudget
	}
	t.Fatalf("%s=%q: want small or large", perfgate.EnvScale, os.Getenv(perfgate.EnvScale))
	return perfgate.Scale{}, 0
}

// Run: go test -tags=perfgate -run '^TestPerfGateFirstLook$' ./cmd/pg_sage_sidecar
func TestPerfGateFirstLook(t *testing.T) {
	scale, budget := firstLookScale(t)
	dsn := testdb.CreateDatabase(t, "perfgate_firstlook")
	ctx := context.Background()
	harness := perfHarnessPool(t, dsn)
	if err := perfgate.Prepare(ctx, harness); err != nil {
		t.Fatalf("prepare (pg_stat_statements must be preloaded): %v", err)
	}
	if err := schema.Bootstrap(ctx, harness); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	built := time.Now()
	if err := perfgate.BuildCatalog(ctx, harness, scale); err != nil {
		t.Fatalf("build catalog: %v", err)
	}
	t.Logf("catalog: %d tables in %s", scale.Tables(), time.Since(built).Round(time.Second))
	monitored, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("monitored pool: %v", err)
	}
	defer monitored.Close()
	seqBefore := userSeqScans(t, ctx, harness)
	report, err := firstlook.Run(ctx, monitored, firstlook.Options{Database: "perf",
		Provider: "self-managed"})
	if err != nil {
		t.Fatalf("first look: %v", err)
	}
	took := report.FinishedAt.Sub(report.StartedAt)
	t.Logf("first look: %d relations, %d items, %s (budget %s)", report.Relations,
		len(report.Items), took.Round(time.Millisecond), budget)
	if report.Relations < firstLookMinRelation {
		t.Fatalf("catalog has %d relations, the gate needs at least %d", report.Relations,
			firstLookMinRelation)
	}
	if took > budget {
		t.Errorf("first look took %s over %d relations, budget %s", took, report.Relations,
			budget)
	}
	for _, c := range report.Checks {
		if c.Status == firstlook.CheckDegraded {
			t.Errorf("check %s degraded at scale: %s", c.Rule, c.Note)
		}
	}
	if err := testdb.FlushIdleSessions(ctx, monitored); err != nil {
		t.Fatalf("flush statistics: %v", err)
	}
	if after := userSeqScans(t, ctx, harness); after != seqBefore {
		t.Errorf("user table sequential scans %d -> %d: the first look scanned user data",
			seqBefore, after)
	}
	assertFirstLookStatementsWithinBudget(t, ctx, harness)
}

func userSeqScans(t *testing.T, ctx context.Context, p *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := p.QueryRow(ctx, `/* `+perfgate.HarnessTag+` */ SELECT
		COALESCE(sum(seq_scan), 0)::bigint FROM pg_stat_user_tables
		WHERE schemaname <> 'sage'`).Scan(&n); err != nil {
		t.Fatalf("user seq scans: %v", err)
	}
	return n
}

func assertFirstLookStatementsWithinBudget(t *testing.T, ctx context.Context,
	p *pgxpool.Pool) {
	t.Helper()
	budget := perfgate.DefaultBudgets().CatalogStatementMaxMs
	rows, err := p.Query(ctx, `/* `+perfgate.HarnessTag+` */ SELECT left(query, 80),
		max_exec_time FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND query LIKE '%pg_sage first_look%'`)
	if err != nil {
		t.Fatalf("read first look statements: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var q string
		var maxMs float64
		if err := rows.Scan(&q, &maxMs); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if maxMs > budget {
			t.Errorf("first look statement %q took %.0f ms, catalog budget %.0f ms", q,
				maxMs, budget)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read first look statements: %v", err)
	}
	if seen == 0 {
		t.Fatal("no first look statement in pg_stat_statements: they must carry the " +
			"/* pg_sage first_look */ tag")
	}
}
