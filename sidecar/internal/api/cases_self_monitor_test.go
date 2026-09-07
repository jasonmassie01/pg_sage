package api

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/fleet"
)

func TestQueryFindings_SuppressesExistingSelfMonitoringRows(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)

	_, err := pool.Exec(ctx,
		`INSERT INTO sage.findings
		 (category, severity, object_type, object_identifier,
		  title, detail, recommendation, status)
		 VALUES
		 ('slow_query', 'warning', 'query', 'queryid:901',
		  'Slow pg_sage query',
		  '{"query":"SELECT data FROM sage.snapshots WHERE category = $1"}',
		  'Review pg_sage internals.', 'open'),
		 ('slow_query', 'warning', 'query', 'queryid:902',
		  'Slow application query',
		  '{"query":"SELECT * FROM public.orders WHERE id = $1"}',
		  'Review application plan.', 'open')`)
	if err != nil {
		t.Fatalf("insert findings: %v", err)
	}

	rows, total, err := queryFindings(ctx, pool,
		fleet.FindingFilters{Status: "open", Limit: 50}, "testdb")
	if err != nil {
		t.Fatalf("queryFindings: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if got := rows[0]["object_identifier"]; got != "queryid:902" {
		t.Fatalf("object_identifier = %v, want queryid:902", got)
	}
}

func TestQueryFindings_DoesNotSuppressStatStatementsPressure(t *testing.T) {
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)

	_, err := pool.Exec(ctx,
		`INSERT INTO sage.findings
		 (category, severity, object_type, object_identifier,
		  title, detail, recommendation, status)
		 VALUES
		 ('stat_statements_pressure', 'critical', 'database',
		  'pg_stat_statements',
		  'pg_stat_statements at 96% capacity',
		  '{"used":4819,"max":5000}',
		  'Increase pg_stat_statements.max or review tracked queries.',
		  'open')`)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}

	rows, total, err := queryFindings(ctx, pool,
		fleet.FindingFilters{Status: "open", Limit: 50}, "testdb")
	if err != nil {
		t.Fatalf("queryFindings: %v", err)
	}
	if total != 1 {
		t.Fatalf("total = %d, want 1", total)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if got := rows[0]["category"]; got != "stat_statements_pressure" {
		t.Fatalf("category = %v, want stat_statements_pressure", got)
	}
}

func TestFilterSelfMonitoringHintRowsRemovesPgSageQueryText(t *testing.T) {
	rows := []map[string]any{
		{
			"queryid":    int64(100),
			"query_text": "SELECT data FROM sage.snapshots",
		},
		{
			"queryid":    int64(200),
			"query_text": "SELECT * FROM public.orders",
		},
	}

	got := filterSelfMonitoringHintRows(rows)
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	if got[0]["queryid"] != int64(200) {
		t.Fatalf("queryid = %v, want 200", got[0]["queryid"])
	}
}
