package retention

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/perfgate"
)

// Perf gate offenders on sage.verification and sage.action_log (perf
// v1.8.3, perf-selfexcl): the decision purges' keep check (no
// verification points at the decision) planned as a hash anti join that
// read all of sage.verification on every run, and the verification
// purge's credited-action check read all of sage.action_log. Purges run
// as prepared statements, so their generic plans are what runs. No purge
// may scan a large sage table: each keep check is an index probe per
// candidate.
func TestPurgeGenericPlansProbeLargeTablesByIndex(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "purge_plans"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	testdb.RequireServerVersion(t, pool, 160000, "EXPLAIN (GENERIC_PLAN)")
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.SeedHistory(ctx, pool, perfgate.SmallScale(),
		perfgate.NewBinding("startup:purge_plans")); err != nil {
		t.Fatal(err)
	}
	if err := perfgate.AnalyzeSage(ctx, pool); err != nil {
		t.Fatal(err)
	}
	large := largeSageTables(t, ctx, pool)
	var stmts []perfgate.Statement
	for _, r := range purgeRules(config.DefaultConfig()) {
		if r.partitioned == nil && large["sage."+r.table] {
			stmts = append(stmts, perfgate.Statement{Query: purgeSQL(r, "sage."+r.table, 500)})
		}
	}
	if len(stmts) < 4 {
		t.Fatalf("%d purges of large tables, want decision (2), verification, action_log...",
			len(stmts))
	}
	plans, err := perfgate.ExplainStatements(ctx, pool, stmts)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if p.Err != "" {
			t.Errorf("explain %.80s: %s", p.Statement.Query, p.Err)
		}
		for _, s := range p.SeqScans {
			if large[s.Schema+"."+s.Relation] {
				t.Errorf("purge scans %s.%s sequentially: %.120s", s.Schema, s.Relation,
					p.Statement.Query)
			}
		}
	}
}

// largeSageTables are the sage tables above the perf gate's seq scan
// threshold (5,000 rows).
func largeSageTables(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT 'sage.' || relname FROM pg_class
		WHERE relnamespace = 'sage'::regnamespace AND relkind = 'r' AND reltuples > 5000`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		out[name] = true
	}
	return out
}
