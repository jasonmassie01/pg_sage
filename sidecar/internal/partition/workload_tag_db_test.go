package partition

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// partitionStatementMarks pick out statements only this package runs.
var partitionStatementMarks = []string{
	"pg_try_advisory_lock(pg_catalog.hashtext",
	"pg_get_indexdef(i.indexrelid)",
	"pg_get_serial_sequence",
	"relpartbound",
	"pg_advisory_xact_lock(",
}

// The conversion and partition upkeep run on the monitored database, so
// pg_stat_statements records them. They are pg_sage's own statements and
// must carry its tag, or Ask Sage and the MCP top_queries tool list them
// as the operator's workload (t2-test-round2).
// No concurrent access test: the statements are constants; the
// concurrent conversion paths are covered in convert_safe_db_test.go.
func TestStatementsCarryThePgSageTag(t *testing.T) {
	pool, ctx := requireDB(t)
	exec(t, ctx, pool, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements")
	// Only this database's entries: other packages share the cluster.
	exec(t, ctx, pool, `SELECT pg_stat_statements_reset(0, (SELECT oid FROM pg_database
		WHERE datname = current_database()), 0)`)
	tbl := scratch(t, ctx, pool, []string{"id"})
	fill(t, ctx, pool, tbl, 200)
	if res, err := Convert(ctx, pool, tbl); err != nil || !res.Converted {
		t.Fatalf("Convert = %+v, %v", res, err)
	}
	if _, err := Ensure(ctx, pool, tbl, time.Now(), 3); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if parts, err := List(ctx, pool, tbl); err != nil || len(parts) == 0 {
		t.Fatalf("List = %v, %v", parts, err)
	}
	rows, err := pool.Query(ctx, `SELECT query FROM pg_stat_statements
		WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())`)
	if err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var q string
		if err := rows.Scan(&q); err != nil {
			t.Fatalf("scan statement: %v", err)
		}
		if !hasPartitionMark(q) {
			continue
		}
		seen++
		if !selfmonitor.IsStatementText(q) {
			t.Errorf("an untagged pg_sage statement reads as workload: %s", q)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read pg_stat_statements: %v", err)
	}
	if seen < len(partitionStatementMarks) {
		t.Fatalf("premise: %d partition statements recorded, want at least %d", seen,
			len(partitionStatementMarks))
	}
}

func hasPartitionMark(q string) bool {
	for _, m := range partitionStatementMarks {
		if strings.Contains(q, m) {
			return true
		}
	}
	return false
}
