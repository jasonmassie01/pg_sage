//go:build integration

package advisor

import (
	"context"
	"testing"
)

func TestRewriteCandidates_LivePG(t *testing.T) {
	pool := testPool(t)
	rows, err := pool.Query(context.Background(),
		`SELECT queryid, calls, mean_exec_time, temp_blks_written,
		        left(query, 120) as query_preview
		 FROM pg_stat_statements
		 WHERE calls > 50 AND mean_exec_time > 10
		 ORDER BY calls * mean_exec_time DESC
		 LIMIT 10`)
	if err != nil {
		t.Skipf("pg_stat_statements: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var queryid, calls, tempBlks int64
		var meanExec float64
		var preview string
		if err := rows.Scan(&queryid, &calls, &meanExec, &tempBlks, &preview); err != nil {
			t.Fatal(err)
		}
		t.Logf("  qid=%d calls=%d mean=%.1fms temp=%d: %s",
			queryid, calls, meanExec, tempBlks, preview)
		count++
	}
	t.Logf("found %d candidate queries", count)
}

func TestRewriteCandidates_LivePG_SpillingQueries(t *testing.T) {
	pool := testPool(t)
	var count int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_stat_statements
		 WHERE temp_blks_written > 0 AND calls > 50`).Scan(&count)
	if err != nil {
		t.Skipf("pg_stat_statements: %v", err)
	}
	t.Logf("queries with temp spills (calls>50): %d", count)
}
