package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/workload"
)

// Dogfood round 2: verification targets are workload. A VACUUM, ANALYZE
// or EXPLAIN of the table names it (the table-statement regex matched
// them) but is not a query an action can help or hurt.
func TestTargetSQLAppliesTheWorkloadRule(t *testing.T) {
	for name, sql := range map[string]string{
		"tableStatementsSQL":    tableStatementsSQL,
		"workloadStatementsSQL": workloadStatementsSQL,
	} {
		if !strings.Contains(sql, workload.AdviceSQL("s.query")) {
			t.Errorf("%s lacks the workload predicate:\n%s", name, sql)
		}
	}
}

func TestTableTargetsLeaveOutDiagnosticStatements(t *testing.T) {
	table, exec, ctx := verifiedTable(t, "public")
	var present bool
	if err := exec.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_extension
		WHERE extname = 'pg_stat_statements')`).Scan(&present); err != nil || !present {
		t.Skip("pg_stat_statements not installed in the test database")
	}
	for _, sql := range []string{
		"EXPLAIN (ANALYZE) SELECT * FROM public." + table + " WHERE a = 1",
		"VACUUM public." + table,
		"ANALYZE public." + table,
		"SELECT count(*) FROM public." + table + " WHERE b = 2",
	} {
		for i := 0; i < 3; i++ {
			if _, err := exec.pool.Exec(ctx, sql); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
	}
	ids := exec.statementIDs(ctx, tableStatementsSQL, wordPattern(table), verifyTargetLimit)
	if len(ids) == 0 {
		t.Fatal("no target: the application SELECT must be one")
	}
	for _, id := range ids {
		var text string
		if err := exec.pool.QueryRow(context.Background(), `SELECT query
			FROM pg_stat_statements WHERE queryid = $1 LIMIT 1`, id).Scan(&text); err != nil {
			t.Fatalf("read target %d: %v", id, err)
		}
		if workload.Excluded(text) {
			t.Errorf("verification target %d is not workload: %q", id, text)
		}
	}
}
