package api

import (
	"context"
	"sort"
	"strings"
	"testing"
)

// footprint-06: a database named sage keeps its self findings in the list
// endpoint and the cases view; pg_sage's own schema-qualified objects stay
// hidden. Real PostgreSQL.
// No concurrent access tests: the filters are pure predicates over a read.

func seedSelfIdentityFindings(t *testing.T) {
	t.Helper()
	pool, ctx := phase2RequireDB(t)
	phase2CleanTables(t, pool, ctx)
	_, err := pool.Exec(ctx, `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, status)
		VALUES
		('sage_self_cost', 'warning', 'database', 'sage',
		 'pg_sage monitoring used 4200 ms of database time per collector cycle on sage',
		 '{"db_time_ms_per_cycle": 4200}', 'Lengthen the interval.', 'open'),
		('sage_footprint', 'warning', 'database', 'sage',
		 'pg_sage data uses 23.4% of database sage (limit 10%)',
		 '{"largest_tables": [{"table": "sage.snapshots"}]}', 'Shorten retention.', 'open'),
		('sage_self_budget', 'warning', 'database', 'sage',
		 'pg_sage monitoring exceeded its own cpu budget on sage',
		 '{"top_statements": [{"text": "/* pg_sage */ SELECT 1"}]}', 'Lengthen.', 'open'),
		('unused_index', 'info', 'index', 'sage.snapshots_pkey', 'Unused index',
		 '{}', 'Drop it.', 'open'),
		('table_bloat', 'info', 'table', 'sage', 'Table bloat', '{}', 'Vacuum.', 'open'),
		('db_size', 'info', 'database', 'sage', 'Database grows', '{}', 'Watch.', 'open'),
		('slow_query', 'warning', 'query', 'queryid:77', 'Slow query',
		 '{"query": "SELECT * FROM pg_stat_statements"}', 'Review.', 'open')`)
	if err != nil {
		t.Fatalf("seed findings: %v", err)
	}
}

func TestFindingsList_SelfFindingsOfDatabaseSage(t *testing.T) {
	seedSelfIdentityFindings(t)
	pool, _ := phase2RequireDB(t)
	h := findingsListHandler(phase2MgrWithPool(pool))
	resp, code := getList(t, h, "/api/v1/findings?database=testdb&status=open&limit=50")
	if code != 200 {
		t.Fatalf("GET /api/v1/findings = %d", code)
	}
	var got []string
	for _, f := range resp.Findings {
		got = append(got, f["category"].(string)+"/"+f["object_identifier"].(string))
		if strings.HasPrefix(f["category"].(string), "sage_") &&
			!strings.Contains(f["title"].(string), "pg_sage") {
			t.Errorf("title %q lost its pg_sage name", f["title"])
		}
	}
	sort.Strings(got)
	want := "db_size/sage,sage_footprint/sage,sage_self_budget/sage,sage_self_cost/sage," +
		"slow_query/queryid:77,table_bloat/sage"
	if strings.Join(got, ",") != want || resp.Total != 6 {
		t.Fatalf("findings = %v (total %d), want %s (sage.snapshots_pkey hidden)", got,
			resp.Total, want)
	}
}

func TestCases_SelfFindingsOfDatabaseSage(t *testing.T) {
	seedSelfIdentityFindings(t)
	pool, _ := phase2RequireDB(t)
	projected, err := queryProjectedCases(context.Background(), phase2MgrWithPool(pool),
		"testdb")
	if err != nil {
		t.Fatalf("queryProjectedCases: %v", err)
	}
	titles := map[string]bool{}
	for _, c := range projected {
		titles[c.Title] = true
	}
	for _, want := range []string{
		"pg_sage monitoring used 4200 ms of database time per collector cycle on sage",
		"pg_sage data uses 23.4% of database sage (limit 10%)",
		"pg_sage monitoring exceeded its own cpu budget on sage",
		"Table bloat",
	} {
		if !titles[want] {
			t.Errorf("cases miss %q; got %v", want, titles)
		}
	}
	if titles["Unused index"] {
		t.Errorf("cases show pg_sage's own index sage.snapshots_pkey: %v", titles)
	}
}
