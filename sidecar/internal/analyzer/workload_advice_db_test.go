package analyzer

import (
	"context"
	"testing"
	"time"
)

// A slow_query finding opened for a diagnostic statement before this rule
// existed (lifeos finding 18021) resolves on the next cycle, because the
// rules no longer emit it; the application query's finding stays open.
func TestLegacyDiagnosticFindingResolvesNextCycle(t *testing.T) {
	a, pool, _ := lifecycleAnalyzer(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO sage.findings (category, severity, object_type,
		object_identifier, title, detail, status, last_seen, occurrence_count)
		VALUES ('slow_query', 'warning', 'query', 'queryid:-3534472208683223210',
		        'Slow query (2352.2ms mean, 2.4x threshold)',
		        jsonb_build_object('query', $1::text, 'calls', 4), 'open', now(), 9)`,
		diagExplain)
	if err != nil {
		t.Fatalf("seed legacy finding: %v", err)
	}
	cur := adviceSnapshot(time.Now(), 1)
	findings := dropNonWorkloadFindings(ruleSlowQueries(cur, nil, adviceConfig(), nil))
	a.finalizeCycle(ctx, findings, map[string]bool{"slow_query": true})

	if n := countFindingRows(t, pool, "slow_query", "queryid:-3534472208683223210",
		"open"); n != 0 {
		t.Fatalf("legacy finding about the EXPLAIN probe still open (%d rows)", n)
	}
	if n := countFindingRows(t, pool, "slow_query", "queryid:-3534472208683223210",
		"resolved"); n != 1 {
		t.Fatalf("legacy finding resolved rows = %d, want 1", n)
	}
	if n := countFindingRows(t, pool, "slow_query", "queryid:1", "open"); n != 1 {
		t.Fatalf("application slow query open rows = %d, want 1", n)
	}
}

// UpsertFindings is the chokepoint for every producer: a finding about a
// diagnostic statement is never persisted, whoever produced it.
func TestUpsertFindingsSkipsDiagnosticStatements(t *testing.T) {
	_, pool, _ := lifecycleAnalyzer(t)
	ctx := context.Background()
	err := UpsertFindings(ctx, pool, []Finding{
		{Category: "plan_regression", Severity: "warning", ObjectType: "query",
			ObjectIdentifier: "queryid:77", Title: "Plan regression",
			Detail: map[string]any{"query": "EXPLAIN SELECT * FROM orders"}},
		{Category: "plan_regression", Severity: "warning", ObjectType: "query",
			ObjectIdentifier: "queryid:78", Title: "Plan regression",
			Detail: map[string]any{"query": "SELECT * FROM orders"}},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if n := countFindingRows(t, pool, "plan_regression", "queryid:77", "open"); n != 0 {
		t.Fatalf("finding about an EXPLAIN statement persisted (%d rows)", n)
	}
	if n := countFindingRows(t, pool, "plan_regression", "queryid:78", "open"); n != 1 {
		t.Fatalf("application finding rows = %d, want 1", n)
	}
}
