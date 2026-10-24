//go:build perfgate

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/changereview/diffscan"
	"github.com/pg-sage/sidecar/internal/changereview/impact"
	"github.com/pg-sage/sidecar/internal/changereview/reasoner"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// Change Review at catalog scale: one review of a migration touching six
// tables of the first-look catalog (10k+ relations), with the shipped
// catalog-read timeouts, completes inside the deterministic stage's 20 s
// without a timed-out read, and no statement it runs exceeds the
// catalog incident budget. The engine only reads and plans: no user table
// is scanned by it.

const changeReviewPerfSQL = `
ALTER TABLE perf_app_000.t_0000 RENAME COLUMN payload TO body;
ALTER TABLE perf_app_000.t_0001 ADD COLUMN note text;
CREATE INDEX CONCURRENTLY t_0002_tenant_payload ON perf_app_000.t_0002 (tenant_id, payload);
DROP INDEX perf_app_000.t_0003_i1;
ALTER TABLE perf_app_000.t_0004 ALTER COLUMN payload TYPE varchar(40);
ALTER TABLE perf_app_000.t_0005 ADD CONSTRAINT t_0005_tenant_fk
  FOREIGN KEY (tenant_id) REFERENCES perf_app_000.t_0006 (id);
UPDATE perf_app_000.t_0007 SET payload = 'x' WHERE tenant_id = 3;
`

// Run: go test -tags=perfgate -run '^TestPerfGateChangeReview$' ./cmd/pg_sage_sidecar
func TestPerfGateChangeReview(t *testing.T) {
	scale, _ := firstLookScale(t)
	dsn := testdb.CreateDatabase(t, "perfgate_change_review")
	ctx := context.Background()
	harness := perfHarnessPool(t, dsn)
	if err := perfgate.Prepare(ctx, harness); err != nil {
		t.Fatalf("prepare (pg_stat_statements must be preloaded): %v", err)
	}
	if err := schema.Bootstrap(ctx, harness); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := perfgate.BuildCatalog(ctx, harness, scale); err != nil {
		t.Fatalf("build catalog: %v", err)
	}
	monitored, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("monitored pool: %v", err)
	}
	defer monitored.Close()
	runChangeReviewWorkload(t, ctx, monitored)
	svc := perfChangeReviewService(t, ctx, monitored)
	seqBefore := userSeqScans(t, ctx, harness)
	// From here on the database runs only the review and the harness. PG18
	// keeps no leading comment in pg_stat_statements, so the statements are
	// told apart by time, not by tag.
	if _, err := harness.Exec(ctx, "SELECT pg_stat_statements_reset()"); err != nil {
		t.Fatalf("reset pg_stat_statements: %v", err)
	}
	started := time.Now()
	got, _, err := svc.Submit(ctx, changereview.Caller{Actor: "perf", Source: "cli"},
		changereview.Request{SQL: changeReviewPerfSQL, MigrationTool: "flyway",
			WaitSeconds: changereview.MaxWaitSeconds})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	took := time.Since(started)
	t.Logf("review %s: %s/%s, %d statements, %d findings in %s", got.ID, got.State,
		got.Verdict, len(got.Statements), len(got.Findings), took.Round(time.Millisecond))
	if got.State != changereview.StateComplete || len(got.Statements) != 7 ||
		took > changereview.DefaultDeterministicTimeout {
		t.Fatalf("review %s (%s), %d statements, took %s", got.State, got.FailureReason,
			len(got.Statements), took)
	}
	for _, e := range got.Evidence {
		if strings.Contains(e.Label, "timed out") {
			t.Errorf("evidence %s: %s", e.ID, e.Label)
		}
	}
	if err := testdb.FlushIdleSessions(ctx, monitored); err != nil {
		t.Fatalf("flush statistics: %v", err)
	}
	if after := userSeqScans(t, ctx, harness); after != seqBefore {
		t.Errorf("user table sequential scans %d -> %d during the review", seqBefore, after)
	}
	assertChangeReviewStatementsWithinBudget(t, ctx, harness)
}

// runChangeReviewWorkload gives pg_stat_statements statements on the
// reviewed tables, so the workload index has work at scale.
func runChangeReviewWorkload(t *testing.T, ctx context.Context, p *pgxpool.Pool) {
	t.Helper()
	for i := 0; i < 50; i++ {
		for _, q := range []string{
			"SELECT payload FROM perf_app_000.t_0000 WHERE id = $1",
			"SELECT count(*) FROM perf_app_000.t_0004 WHERE tenant_id = $1",
		} {
			if _, err := p.Exec(ctx, q, i+1); err != nil {
				t.Fatalf("workload %s: %v", q, err)
			}
		}
	}
}

func perfChangeReviewService(t *testing.T, ctx context.Context,
	p *pgxpool.Pool) *changereview.Service {
	t.Helper()
	crCfg := config.DefaultChangeReviewConfig()
	engine, err := impact.New(impact.Deps{Pool: p, Config: crCfg,
		Timeouts: catalogread.FromSafety(config.DefaultConfig().Safety), Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	r, err := reasoner.New(reasoner.Deps{Tools: engine, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := changereview.New(changereview.Deps{Database: "perf", Pool: p, Config: crCfg,
		Analyzer: engine, Reasoner: r, HunkFilter: diffscan.HunkFilter,
		Redact: diffscan.Redact, Logf: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return svc
}

func assertChangeReviewStatementsWithinBudget(t *testing.T, ctx context.Context,
	p *pgxpool.Pool) {
	t.Helper()
	budget := perfgate.DefaultBudgets().CatalogStatementMaxMs
	rows, err := p.Query(ctx, `/* `+perfgate.HarnessTag+` */ SELECT left(query, 90),
		max_exec_time FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		  AND calls > 0 AND query NOT LIKE '%pg_stat_statements%'`)
	if err != nil {
		t.Fatalf("read the review's statements: %v", err)
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
			t.Errorf("%.0f ms over the %.0f ms budget: %s", maxMs, budget, q)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen < 10 {
		t.Fatalf("only %d statements recorded during the review", seen)
	}
	t.Logf("%d statements during the review, each within %.0f ms", seen, budget)
}
