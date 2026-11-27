package main

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// vacuum-04 end to end on real PostgreSQL: a bloated table of at least
// the 1 GiB floor reaches the ledger as one high-risk parked
// bloat_remediation decision (before the fix the contract-less insert
// failed sage.decision's risk_tier CHECK, SQLSTATE 23514), and repeats
// fold into that row. No concurrency test: one custodian per database
// runs in a serial loop.

const bloatFixture = "public.vacuum04_bloat_fixture"

// buildGiBBloatFixture writes a 1.1 GiB unlogged heap of 1 KB rows and
// removes 60% of them; VACUUM leaves the pages, so the measured bloat is
// about 60%.
func buildGiBBloatFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, sql := range []string{
		"DROP TABLE IF EXISTS " + bloatFixture,
		"CREATE UNLOGGED TABLE " + bloatFixture + ` (id bigint, payload text)
			WITH (autovacuum_enabled = false)`,
		"INSERT INTO " + bloatFixture + ` SELECT g, repeat(md5(g::text), 30)
			FROM generate_series(1, 1100000) g`,
		"DELETE FROM " + bloatFixture + " WHERE id % 10 >= 4",
		"VACUUM " + bloatFixture,
		"ANALYZE " + bloatFixture,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("build bloat fixture: %s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+bloatFixture)
	})
	var bytes int64
	if err := pool.QueryRow(ctx, `SELECT relpages::bigint * current_setting('block_size')::bigint
		FROM pg_class WHERE oid = $1::regclass`, bloatFixture).Scan(&bytes); err != nil ||
		bytes < config.DefaultTableBloatMinBytes {
		t.Fatalf("fixture heap = %d bytes (%v), want at least 1 GiB", bytes, err)
	}
}

func TestFreezeBloatPlanRecordsHighParkedDecision(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 1.1 GiB fixture; skipped in -short runs")
	}
	pool := autonomyPool(t)
	ctx := context.Background()
	buildGiBBloatFixture(t, pool)
	custodian := newFreezeCustodian(pool, config.DefaultConfig(), "testdb")
	exec := autonomousExecutor(pool)
	exec.EnableStandingPolicyDocument(policy.UnattendedProfile(), nil)
	router := executorProposalRouter{executor: exec,
		isReplica: func(context.Context) bool { return false }}

	for cycle := 1; cycle <= 2; cycle++ {
		proposals, err := custodian.Scan(ctx)
		if err != nil {
			t.Fatalf("cycle %d Scan: %v", cycle, err)
		}
		routed := 0
		for _, proposal := range proposals {
			if len(proposal.TargetObjects) != 1 || proposal.TargetObjects[0] != bloatFixture {
				continue
			}
			routed++
			if proposal.ResponseKind != "park_bloat" || proposal.SQL != "" {
				t.Fatalf("cycle %d fixture proposal = %#v", cycle, proposal)
			}
			if err := router.Route(ctx, proposal); err != nil {
				t.Fatalf("cycle %d Route: %v", cycle, err)
			}
		}
		if routed != 1 {
			t.Fatalf("cycle %d routed %d fixture proposals, want 1", cycle, routed)
		}
	}
	assertBloatDecision(t, pool)
}

// assertBloatDecision is CHECK-01, plus the fold of the second cycle.
func assertBloatDecision(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var rows, repeats int
	var reason, source string
	err := pool.QueryRow(context.Background(), `SELECT count(*), COALESCE(max(repeat_count), 0),
		COALESCE(max(reason), ''), COALESCE(max(evidence->>'bloat_source'), '')
		FROM sage.decision
		WHERE feature = 'bloat_remediation' AND risk_tier = 'high' AND verdict = 'parked'
		  AND target_objects @> to_jsonb(ARRAY[$1::text])`, bloatFixture).
		Scan(&rows, &repeats, &reason, &source)
	if err != nil {
		t.Fatalf("read decisions: %v", err)
	}
	if rows != 1 || repeats != 2 || source != "estimate" {
		t.Fatalf("bloat decisions: rows=%d repeat_count=%d reason=%q source=%q, "+
			"want one row repeated twice from the estimate", rows, repeats, reason, source)
	}
}
