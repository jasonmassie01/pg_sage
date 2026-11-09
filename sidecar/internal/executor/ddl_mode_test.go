package executor

import "testing"

// No concurrent access tests: executionMode and ddlLockTimeoutMS are pure.

// executor-08: the execution route is the typed action's, never a keyword
// anywhere in the text.
func TestExecutionModeByActionType(t *testing.T) {
	topLevel := []string{"create_index_concurrently", "reindex_concurrently",
		"drop_unused_index", "revert_created_index", "vacuum_table", "alter_system_guc"}
	for _, actionType := range topLevel {
		if got := executionMode(actionType); got != execTopLevel {
			t.Errorf("executionMode(%q) = %v, want top-level", actionType, got)
		}
	}
	inTx := []string{"", "alter_table", "set_table_autovacuum", "analyze_table",
		"alter_database_guc", "cancel_backend", "terminate_backend", "create_statistics",
		"revert_created_statistics", "apply_query_hint", "retire_query_hint", "bogus"}
	for _, actionType := range inTx {
		if got := executionMode(actionType); got != execInTransaction {
			t.Errorf("executionMode(%q) = %v, want in-transaction", actionType, got)
		}
	}
}

// A table whose name contains "concurrently" or "vacuum" is an ordinary
// in-transaction ALTER TABLE; real top-level statements stay top-level.
func TestStatementRouteIgnoresKeywordsInNames(t *testing.T) {
	cases := map[string]bool{
		"ALTER TABLE public.concurrently_log SET (fillfactor=90)":        false,
		`ALTER TABLE public."CONCURRENTLY" SET (fillfactor=90)`:          false,
		"ALTER TABLE public.vacuum_log SET (fillfactor=90)":              false,
		"CREATE INDEX idx_concurrently ON public.t (a)":                  false,
		"VACUUM FULL public.t":                                           false,
		"VACUUM":                                                         false,
		"REINDEX SCHEMA CONCURRENTLY public":                             false,
		"":                                                               false,
		"CREATE INDEX CONCURRENTLY idx ON public.t (a)":                  true,
		"DROP INDEX CONCURRENTLY IF EXISTS public.idx":                   true,
		"REINDEX INDEX CONCURRENTLY public.idx":                          true,
		"VACUUM (FREEZE) public.t":                                       true,
		"ALTER SYSTEM SET work_mem = '64MB'":                             true,
		"ALTER SYSTEM RESET work_mem":                                    true,
		"ALTER TABLE public.t SET (autovacuum_vacuum_scale_factor=0.05)": false,
	}
	for sql, want := range cases {
		if got := statementRunsTopLevel(sql); got != want {
			t.Errorf("statementRunsTopLevel(%q) = %v, want %v", sql, got, want)
		}
	}
}

// Only top-level types keep the safety lock timeout; every in-transaction
// type, and an unclassified statement, takes the policy ceiling.
func TestDDLLockTimeoutByActionType(t *testing.T) {
	tests := []struct {
		actionType string
		safety     int
		ceiling    int64
		want       int
	}{
		{"create_index_concurrently", 30000, 3000, 30000},
		{"vacuum_table", 30000, 3000, 30000},
		{"alter_system_guc", 30000, 3000, 30000},
		{"alter_table", 30000, 3000, 3000},
		{"", 30000, 3000, 3000},
		{"alter_table", 2000, 3000, 2000},
		{"alter_table", 30000, 0, 30000},
		{"alter_table", 30000, 30000, 30000},
		{"alter_table", 30000, 29999, 29999},
	}
	for _, tt := range tests {
		got := ddlLockTimeoutMS(tt.actionType, tt.safety, tt.ceiling)
		if got != tt.want {
			t.Errorf("ddlLockTimeoutMS(%q, %d, %d) = %d, want %d",
				tt.actionType, tt.safety, tt.ceiling, got, tt.want)
		}
	}
}

// The inputs of the removed substring-routing tests (executor-08), routed
// by type. Changed: VACUUM FULL and a bare VACUUM have no contract and no
// longer run top-level (they fail closed in a transaction block); an
// ALTER DATABASE, DROP INDEX and plain CREATE INDEX stay in-transaction.
func TestStatementRunsTopLevelFormerSubstringCases(t *testing.T) {
	cases := map[string]bool{
		"CREATE INDEX CONCURRENTLY idx ON t (c)":      true,
		"create index concurrently idx on t (c)":      true,
		"REINDEX INDEX CONCURRENTLY idx":              true,
		"DROP INDEX CONCURRENTLY idx":                 true,
		"VACUUM t":                                    true,
		"VACUUM ANALYZE t":                            true,
		"vacuum analyze t":                            true,
		"  VACUUM t":                                  true,
		"VACUUM (VERBOSE) public.large_table":         true,
		"ALTER SYSTEM SET work_mem = '64MB'":          true,
		"alter system set max_wal_size = '4GB'":       true,
		"VACUUM FULL t":                               false,
		"vacuum full t":                               false,
		`ALTER DATABASE "mydb" SET work_mem = '64MB'`: false,
		"CREATE INDEX idx ON t (c)":                   false,
		"ANALYZE t":                                   false,
		"SELECT pg_terminate_backend(123)":            false,
		"DROP INDEX idx":                              false,
		"SELECT 1":                                    false,
	}
	for sql, want := range cases {
		if got := statementRunsTopLevel(sql); got != want {
			t.Errorf("statementRunsTopLevel(%q) = %v, want %v", sql, got, want)
		}
	}
}
