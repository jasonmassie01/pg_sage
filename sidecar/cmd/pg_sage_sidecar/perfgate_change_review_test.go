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
// catalog incident budget. The history it reads is a mature one: a week
// of query_store samples (one a minute) for 100 hot statements on a
// reviewed table, and a 'tables' snapshot of 20,000 relations. The engine
// only reads and plans: no user table is scanned by it.

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
	harness := preparePerfChangeReview(t, ctx, dsn, scale)
	monitored, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("monitored pool: %v", err)
	}
	defer monitored.Close()
	svc := perfChangeReviewService(t, ctx, monitored)
	// From here on the database runs only the workload, the seeding and the
	// review. PG18 keeps no leading comment in pg_stat_statements, so the
	// harness's own statements carry its tag at their end.
	if _, err := harness.Exec(ctx, "SELECT pg_stat_statements_reset()"); err != nil {
		t.Fatalf("reset pg_stat_statements: %v", err)
	}
	runChangeReviewWorkload(t, ctx, monitored)
	seedChangeReviewHistory(t, ctx, harness)
	if err := testdb.FlushIdleSessions(ctx, monitored); err != nil {
		t.Fatalf("flush statistics: %v", err)
	}
	seqBefore := userSeqScans(t, ctx, harness)
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
	assertRatedWorkload(t, got)
	if err := testdb.FlushIdleSessions(ctx, monitored); err != nil {
		t.Fatalf("flush statistics: %v", err)
	}
	if after := userSeqScans(t, ctx, harness); after != seqBefore {
		t.Errorf("user table sequential scans %d -> %d during the review", seqBefore, after)
	}
	assertChangeReviewStatementsWithinBudget(t, ctx, harness)
}

// preparePerfChangeReview builds the perf catalog and pg_sage's schema in
// dsn and returns the harness pool.
func preparePerfChangeReview(t *testing.T, ctx context.Context, dsn string,
	scale perfgate.Scale) *pgxpool.Pool {
	t.Helper()
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
	return harness
}

// assertRatedWorkload fails when a read timed out or the hot workload
// statements lost their seeded query_store peak rate.
func assertRatedWorkload(t *testing.T, got changereview.Review) {
	t.Helper()
	rated := 0
	for _, e := range got.Evidence {
		if strings.Contains(e.Label, "timed out") {
			t.Errorf("evidence %s: %s", e.ID, e.Label)
		}
		if strings.HasPrefix(e.ID, "queryid:") && strings.Contains(string(e.Data),
			`"peak_calls_per_s":1`) {
			rated++
		}
	}
	if rated < 50 {
		t.Errorf("%d workload statements carry their seeded peak rate, want the hot ones",
			rated)
	}
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
	runHotStatements(t, ctx, p)
}

// hotStatements is how many distinct hot statements read t_0000.
const hotStatements = 100

// runHotStatements gives pg_stat_statements hotStatements distinct
// statements on t_0000 (one more OR term each).
func runHotStatements(t *testing.T, ctx context.Context, p *pgxpool.Pool) {
	t.Helper()
	for k := 1; k <= hotStatements; k++ {
		q := "SELECT payload FROM perf_app_000.t_0000 WHERE id = $1" +
			strings.Repeat(" OR id = $1", k)
		if _, err := p.Exec(ctx, q, k); err != nil {
			t.Fatalf("hot statement %d: %v", k, err)
		}
	}
}

// seedChangeReviewHistory writes a week of query_store samples, one a
// minute, at one call per second for every workload statement on
// t_0000 (the hot statements included), and a 'tables' snapshot two hours
// old of 20,000 relations.
func seedChangeReviewHistory(t *testing.T, ctx context.Context, p *pgxpool.Pool) {
	t.Helper()
	tag := " /* " + perfgate.HarnessTag + " */"
	if _, err := p.Exec(ctx, `INSERT INTO sage.query_store (captured_at, queryid, calls,
		total_exec_time, mean_exec_time)
		SELECT now() - interval '7 days' + g * interval '1 minute', s.queryid, 60 * g,
		       0.01 * g, 0.01
		  FROM (SELECT DISTINCT queryid FROM pg_stat_statements
		         WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
		           AND query LIKE '%perf_app_000.t_0000%') s
		 CROSS JOIN generate_series(0, 7 * 24 * 60) g`+tag); err != nil {
		t.Fatalf("seed query_store: %v", err)
	}
	if _, err := p.Exec(ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		SELECT now() - interval '2 hours', 'tables', jsonb_agg(jsonb_build_object(
		         'schemaname', 'perf_app_' || lpad((g / 1000)::text, 3, '0'),
		         'relname', 't_' || lpad((g % 1000)::text, 4, '0'),
		         'n_tup_ins', g, 'n_tup_upd', g, 'n_tup_del', 0, 'n_live_tup', g))
		  FROM generate_series(0, 19999) g`+tag); err != nil {
		t.Fatalf("seed the tables snapshot: %v", err)
	}
	if _, err := p.Exec(ctx, "ANALYZE sage.query_store, sage.snapshots"+tag); err != nil {
		t.Fatal(err)
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
		  AND calls > 0 AND query NOT LIKE '%pg_stat_statements%'
		  AND query NOT LIKE '%`+perfgate.HarnessTag+`%'`)
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
