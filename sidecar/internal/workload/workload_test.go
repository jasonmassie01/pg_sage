package workload

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// lifeosProbe is the statement the lifeos briefing told the operator to
// "investigate" (2026-10-04): an untagged EXPLAIN ANALYZE of an old
// sequence-runway probe, as pg_stat_statements stores it.
const lifeosProbe = "EXPLAIN (ANALYZE, BUFFERS, SUMMARY ON)\nWITH s AS (\n" +
	"    SELECT q.seqrelid, pg_catalog.quote_ident(n.nspname) || $1 ||\n" +
	"           pg_catalog.quote_ident(c.relname) AS seq\n" +
	"    FROM pg_catalog.pg_sequence q\n" +
	"    JOIN pg_catalog.pg_class c ON c.oid = q.seqrelid\n" +
	"    WHERE q.seqincrement > $3\n) SELECT * FROM s"

type classifyCase struct {
	name  string
	query string
	want  Reason
}

// classifyCases is shared with the SQL parity test: the Go rule and the
// SQL predicate must agree on every one of them.
var classifyCases = []classifyCase{
	// Application workload stays.
	{"select", "SELECT * FROM orders WHERE id = $1", Workload},
	{"cte", "WITH s AS (SELECT 1) SELECT * FROM s", Workload},
	{"insert", "INSERT INTO orders (id) VALUES ($1)", Workload},
	{"update", "UPDATE orders SET status = $1 WHERE id = $2", Workload},
	{"copy from stdin", "COPY public.events (id, payload) FROM stdin", Workload},
	{"copy from with options",
		"COPY events FROM STDIN WITH (FORMAT csv, HEADER true)", Workload},
	{"refresh matview", "REFRESH MATERIALIZED VIEW daily_totals", Workload},
	{"explain word inside", "SELECT explain FROM docs WHERE id = $1", Workload},
	{"analyze column", "SELECT analyze_count FROM stats", Workload},
	{"vacuum in string", "SELECT * FROM jobs WHERE name = 'VACUUM'", Workload},
	{"index word", "SELECT * FROM create_index_log", Workload},
	{"explanation table", "SELECT * FROM explanations", Workload},
	{"copyright table", "SELECT * FROM copyrights", Workload},
	{"copy table named to", `COPY "to" FROM stdin`, Workload},
	{"reset in select list", "SELECT reset_token FROM users WHERE id = $1", Workload},
	{"clusters table", "SELECT * FROM clusters", Workload},
	{"empty", "", Workload},
	{"whitespace", "   \n\t", Workload},
	// Diagnostic tooling.
	{"lifeos probe", lifeosProbe, Explain},
	{"explain", "EXPLAIN SELECT * FROM orders", Explain},
	{"explain lower", "explain analyze select 1", Explain},
	{"explain leading space", "  \n EXPLAIN (FORMAT JSON) SELECT 1", Explain},
	{"explain after comment", "/* app:report */ EXPLAIN SELECT 1", Explain},
	{"explain after line comment", "-- check\nEXPLAIN SELECT 1", Explain},
	{"explain after starred comment", "/** x **/EXPLAIN SELECT 1", Explain},
	// Maintenance.
	{"vacuum", "VACUUM public.orders", Maintenance},
	{"vacuum options", "vacuum (verbose, analyze) orders", Maintenance},
	{"bare vacuum", "VACUUM", Maintenance},
	{"analyze", "ANALYZE orders", Maintenance},
	{"analyse", "ANALYSE orders", Maintenance},
	{"bare analyze", "ANALYZE", Maintenance},
	{"create index", "CREATE INDEX idx_o ON orders (id)", Maintenance},
	{"create unique index", "create unique index concurrently u ON t (a)", Maintenance},
	{"create index if not exists",
		"CREATE INDEX IF NOT EXISTS ix ON delivery_queue (dedupe_key)", Maintenance},
	{"reindex", "REINDEX INDEX CONCURRENTLY idx_o", Maintenance},
	{"cluster", "CLUSTER orders USING idx_o", Maintenance},
	{"checkpoint", "CHECKPOINT", Maintenance},
	// Statistics resets.
	{"pgss reset", "SELECT pg_stat_statements_reset()", StatsReset},
	{"pgss reset args", "select pg_stat_statements_reset($1, $2, $3)", StatsReset},
	{"pg_stat_reset", "SELECT pg_stat_reset()", StatsReset},
	{"pg_stat_reset_shared", "SELECT pg_catalog.pg_stat_reset_shared($1)", StatsReset},
	// Backups.
	{"pg_dump copy",
		"COPY test_health_1.events (id, user_id) TO stdout;", Backup},
	{"copy to file", "COPY orders TO '/tmp/orders.csv'", Backup},
	{"copy query to", "COPY (SELECT * FROM orders) TO STDOUT WITH (FORMAT csv)", Backup},
	{"copy quoted", `COPY "Weird ""Name""".t TO STDOUT`, Backup},
	{"copy binary", "COPY BINARY orders TO stdout", Backup},
	// pg_sage's own statements.
	{"tagged", "/* pg_sage */ SELECT 1", Self},
	{"tagged explain", "EXPLAIN /* pg_sage */ (GENERIC_PLAN, FORMAT JSON) SELECT 1", Self},
	{"sage schema", "SELECT * FROM sage.findings", Self},
	{"tagged copy", `copy /* pg_sage */ "sage"."runway_samples" ("kind") from stdin binary`,
		Self},
}

func TestClassify(t *testing.T) {
	for _, tc := range classifyCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.query); got != tc.want {
				t.Fatalf("Classify(%q) = %q, want %q", tc.query, got, tc.want)
			}
			if got := Excluded(tc.query); got != (tc.want != Workload) {
				t.Fatalf("Excluded(%q) = %v, want %v", tc.query, got, tc.want != Workload)
			}
		})
	}
}

// IsDiagnostic is the pattern alone (the SQL DiagnosticSQL twin):
// whether pg_sage ran the statement is selfmonitor's question.
func TestIsDiagnosticIsThePatternAlone(t *testing.T) {
	if IsDiagnostic("/* pg_sage */ SELECT 1") {
		t.Fatal("a tagged pg_sage SELECT is not a diagnostic statement")
	}
	if !IsDiagnostic("EXPLAIN /* pg_sage */ (GENERIC_PLAN) SELECT 1") {
		t.Fatal("a tagged EXPLAIN is still an EXPLAIN")
	}
	if !IsDiagnostic(lifeosProbe) {
		t.Fatal("the lifeos EXPLAIN ANALYZE probe is diagnostic")
	}
	if IsDiagnostic("SELECT * FROM orders") {
		t.Fatal("an application SELECT is not diagnostic")
	}
}

func TestQueriesKeepsOnlyWorkloadInOrder(t *testing.T) {
	in := []collector.QueryStats{
		{QueryID: 1, Query: "SELECT * FROM orders"},
		{QueryID: 2, Query: lifeosProbe},
		{QueryID: 3, Query: "VACUUM orders"},
		{QueryID: 4, Query: "UPDATE orders SET a = $1"},
		{QueryID: 5, Query: "/* pg_sage */ SELECT 1"},
		{QueryID: 6, Query: "COPY orders TO stdout"},
	}
	got := Queries(in)
	if len(got) != 2 || got[0].QueryID != 1 || got[1].QueryID != 4 {
		t.Fatalf("Queries kept %+v, want queryids 1 and 4 in order", got)
	}
	if len(in) != 6 || in[1].QueryID != 2 {
		t.Fatal("Queries must not modify its input (raw views keep every statement)")
	}
}

func TestQueriesNilAndEmpty(t *testing.T) {
	if got := Queries(nil); len(got) != 0 {
		t.Fatalf("Queries(nil) = %v, want empty", got)
	}
	if got := Queries([]collector.QueryStats{}); len(got) != 0 {
		t.Fatalf("Queries(empty) = %v, want empty", got)
	}
	allDiag := []collector.QueryStats{{QueryID: 9, Query: "ANALYZE t"}}
	if got := Queries(allDiag); len(got) != 0 {
		t.Fatalf("Queries(all diagnostic) = %v, want empty", got)
	}
}

func TestFindingExcluded(t *testing.T) {
	cases := []struct {
		name   string
		detail map[string]any
		want   bool
	}{
		{"nil detail", nil, false},
		{"empty detail", map[string]any{}, false},
		{"workload query", map[string]any{"query": "SELECT * FROM orders"}, false},
		{"explain query", map[string]any{"query": lifeosProbe, "calls": 4}, true},
		{"query_text key", map[string]any{"query_text": "VACUUM t"}, true},
		{"normalized_query key", map[string]any{"normalized_query": "CHECKPOINT"}, true},
		{"sample_query key", map[string]any{"sample_query": "COPY t TO stdout"}, true},
		{"statement key", map[string]any{"statement": "EXPLAIN SELECT 1"}, true},
		{"self query", map[string]any{"query": "/* pg_sage */ SELECT 1"}, true},
		{"non-string query", map[string]any{"query": 42}, false},
		{"bytes query", map[string]any{"query": []byte("EXPLAIN SELECT 1")}, true},
		{"unrelated key", map[string]any{"recommendation": "EXPLAIN it"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FindingExcluded(tc.detail); got != tc.want {
				t.Fatalf("FindingExcluded(%v) = %v, want %v", tc.detail, got, tc.want)
			}
		})
	}
}

func TestAdviceSQLDefaultsColumnAndComposesSelfRule(t *testing.T) {
	got := AdviceSQL("")
	if !strings.Contains(got, "COALESCE(query, '')") {
		t.Fatalf("AdviceSQL(\"\") = %q, want the query column by default", got)
	}
	if !strings.Contains(got, "pg_sage") {
		t.Fatalf("AdviceSQL must also leave out pg_sage's own statements: %q", got)
	}
	if strings.Contains(got, "%%") {
		t.Fatalf("AdviceSQL must be plain SQL, not a format string: %q", got)
	}
	if !strings.Contains(DiagnosticSQL("s.query"), "COALESCE(s.query, '')") {
		t.Fatalf("DiagnosticSQL ignores its column: %q", DiagnosticSQL("s.query"))
	}
}

func TestFindingAdviceSQLCoversEveryDetailKey(t *testing.T) {
	got := FindingAdviceSQL("detail")
	for _, key := range detailQueryKeys {
		if !strings.Contains(got, "detail->>'"+key+"'") {
			t.Errorf("FindingAdviceSQL omits detail key %q: %s", key, got)
		}
	}
}

// No concurrent access tests: the package is stateless (compiled
// patterns are read-only after init).
