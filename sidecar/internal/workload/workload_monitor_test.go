package workload

import "testing"

// monitorCases (footprint-06) are statements third-party monitors send:
// a Datadog-, pganalyze- or pgwatch-style read of the statistics views is
// another client's workload, never pg_sage's own; only the tag makes a
// catalog read pg_sage's. Shared with the real-Postgres parity test.
var monitorCases = []classifyCase{
	{"datadog pgss read", "SELECT pg_stat_statements.userid, pg_stat_statements.dbid, " +
		"pg_stat_statements.queryid, pg_stat_statements.query, calls, total_exec_time, " +
		"rows FROM pg_stat_statements LEFT JOIN pg_roles ON pg_stat_statements.userid = " +
		"pg_roles.oid LEFT JOIN pg_database ON pg_stat_statements.dbid = pg_database.oid " +
		"WHERE query != $1 AND query NOT LIKE $2 LIMIT $3", Workload},
	{"pgwatch activity read", "select datname, coalesce(state, $1), count(*) " +
		"from pg_stat_activity where pid != pg_backend_pid() and datname is not null " +
		"group by 1, 2", Workload},
	{"pganalyze table stats", "SELECT relid, schemaname, relname, seq_scan, n_dead_tup " +
		"FROM pg_catalog.pg_stat_user_tables", Workload},
	{"monitor index stats", "SELECT indexrelname, idx_scan FROM pg_stat_user_indexes",
		Workload},
	{"monitor replication", "SELECT * FROM pg_stat_replication", Workload},
	{"monitor database", "SELECT xact_commit FROM pg_stat_database WHERE datname = $1",
		Workload},
	{"monitor bgwriter", "select * from\npg_stat_bgwriter", Workload},
	{"monitor checkpointer", "SELECT num_timed FROM pg_stat_checkpointer", Workload},
	// Diagnostic tooling over the same views keeps its diagnostic reason.
	{"monitor explain", "EXPLAIN SELECT * FROM pg_stat_statements", Explain},
	{"monitor pgss reset", "SELECT pg_stat_statements_reset()", StatsReset},
	// pg_sage's own catalog reads are tagged.
	{"tagged pgss read", "/* pg_sage */ SELECT queryid, calls FROM pg_stat_statements",
		Self},
	{"tagged activity read", "/* pg_sage collector */ SELECT state, count(*) " +
		"FROM pg_stat_activity GROUP BY 1", Self},
}

func TestClassify_MonitorStatementsAreNotSelf(t *testing.T) {
	for _, tc := range monitorCases {
		if got := Classify(tc.query); got != tc.want {
			t.Errorf("%s: Classify = %q, want %q", tc.name, got, tc.want)
		}
		if got := Excluded(tc.query); got != (tc.want != Workload) {
			t.Errorf("%s: Excluded = %v, want %v", tc.name, got, tc.want != Workload)
		}
	}
}

// A finding about a monitor's statement is workload advice, not dropped.
func TestFindingExcluded_MonitorStatementKept(t *testing.T) {
	detail := map[string]any{"query": monitorCases[0].query, "calls": 120}
	if FindingExcluded(detail) {
		t.Fatal("a finding about a Datadog-style pg_stat_statements read is dropped")
	}
	tagged := map[string]any{"query": "/* pg_sage */ SELECT * FROM pg_stat_statements"}
	if !FindingExcluded(tagged) {
		t.Fatal("a finding about pg_sage's tagged catalog read is kept")
	}
}
