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
	seedLargeMaintenanceProposals(t, ctx, pool)
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

// Six thousand distinct 8 KiB proposal objects exercise the real JSON/TOAST
// cost, above the generic-plan scan threshold. This fixture is deliberately
// larger than a normal operator-maintenance ledger; a keep check must probe
// its matching expression index rather than scan/decompress every proposal.
func seedLargeMaintenanceProposals(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(ctx, `WITH binding AS (
	    INSERT INTO sage.sre_database_bindings (deployment_id, database_id,
	        runtime_key, identity_strength, cluster_epoch)
	    VALUES (gen_random_uuid(), gen_random_uuid(), 'maintenance-plan-gate',
	            'configured', 'retention-plan-test')
	    RETURNING deployment_id, database_id
	), investigations AS (
	    INSERT INTO sage.sre_investigations (deployment_id, database_id, id,
	        source_case_id, trigger_kind, trigger_fingerprint, state, expires_at)
	    SELECT b.deployment_id, b.database_id, gen_random_uuid(), 'case:' || g,
	           'operator', sha256(convert_to(g::text, 'UTF8')), 'concluded',
	           now()+interval '1 day'
	    FROM binding b CROSS JOIN generate_series(1,6000) g
	    RETURNING deployment_id, database_id, id
	)
	INSERT INTO sage.sre_maintenance_proposals
	    (id, deployment_id, database_id, investigation_id, state, proposal)
	SELECT gen_random_uuid(), i.deployment_id, i.database_id, i.id, 'executed',
	    jsonb_build_object('finding_id', row_number() OVER () + 1000000,
	        'action_log_id', row_number() OVER () + 1000000,
	        'padding', (SELECT string_agg(md5(i.id::text || ':' || salt), '')
	                    FROM generate_series(1,256) salt))
	FROM investigations i`); err != nil {
		t.Fatalf("seed large maintenance proposals: %v", err)
	}
	var count int
	var minBytes int
	if err := pool.QueryRow(ctx, `SELECT count(*), min(pg_column_size(proposal))
		FROM sage.sre_maintenance_proposals`).Scan(&count, &minBytes); err != nil {
		t.Fatal(err)
	}
	if count != 6000 || minBytes <= 2000 {
		t.Fatalf("maintenance scan fixture too small: rows=%d min_stored_bytes=%d", count, minBytes)
	}
	t.Logf("generic purge plans include %d maintenance proposals, min stored body %d bytes", count, minBytes)
}
