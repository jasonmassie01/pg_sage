package analyzer

import (
	"context"
	"testing"
)

func TestUpsertFindings_SkipsSelfMonitoringFindings(t *testing.T) {
	pool := phase2Pool(t)
	phase2CleanFindings(t, pool)
	ctx := context.Background()
	t.Cleanup(func() { phase2CleanFindings(t, pool) })

	findings := []Finding{
		{
			Category:         "test_phase2_self_monitoring",
			Severity:         "warning",
			ObjectType:       "query",
			ObjectIdentifier: "queryid:9001",
			Title:            "Slow pg_sage query",
			Detail: map[string]any{
				"query": "SELECT data FROM sage.snapshots WHERE category = $1",
			},
			Recommendation: "Review pg_sage internals.",
		},
		{
			Category:         "test_phase2_self_monitoring",
			Severity:         "warning",
			ObjectType:       "query",
			ObjectIdentifier: "queryid:9002",
			Title:            "Slow application query",
			Detail: map[string]any{
				"query": "SELECT * FROM public.orders WHERE id = $1",
			},
			Recommendation: "Review application plan.",
		},
	}

	if err := UpsertFindings(ctx, pool, findings); err != nil {
		t.Fatalf("UpsertFindings: %v", err)
	}

	var total int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM sage.findings
		 WHERE category = 'test_phase2_self_monitoring'`).Scan(&total)
	if err != nil {
		t.Fatalf("count findings: %v", err)
	}
	if total != 1 {
		t.Fatalf("persisted findings = %d, want 1", total)
	}

	var objectIdentifier string
	err = pool.QueryRow(ctx,
		`SELECT object_identifier FROM sage.findings
		 WHERE category = 'test_phase2_self_monitoring'`).Scan(&objectIdentifier)
	if err != nil {
		t.Fatalf("select persisted finding: %v", err)
	}
	if objectIdentifier != "queryid:9002" {
		t.Fatalf("object_identifier = %q, want queryid:9002", objectIdentifier)
	}
}
