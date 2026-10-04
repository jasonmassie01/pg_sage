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
	// Other packages reset pg_stat_statements on the shared matrix servers
	// (cluster-wide), so targets and their texts are read in one statement,
	// and a reset between running and reading retries the whole sequence.
	for attempt := 0; attempt < 3; attempt++ {
		checked := targetTexts(t, exec, ctx, table)
		if len(checked) == 0 {
			continue
		}
		for id, text := range checked {
			if workload.Excluded(text) {
				t.Errorf("verification target %d is not workload: %q", id, text)
			}
		}
		return
	}
	t.Fatal("no target after 3 attempts: the application SELECT must be one")
}

// targetTexts runs the table's statements, then reads the verification
// targets with their texts in one snapshot of pg_stat_statements.
func targetTexts(t *testing.T, exec *Executor, ctx context.Context,
	table string) map[int64]string {
	t.Helper()
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
	rows, err := exec.pool.Query(context.Background(), `WITH t AS (`+tableStatementsSQL+`)
		SELECT t.queryid, min(s.query) FROM t JOIN pg_stat_statements s USING (queryid)
		GROUP BY t.queryid`, wordPattern(table), verifyTargetLimit)
	if err != nil {
		t.Fatalf("read targets: %v", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			t.Fatal(err)
		}
		out[id] = text
	}
	return out
}
