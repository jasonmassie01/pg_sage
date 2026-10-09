package firstlook

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// The index read took 436-540 ms at 15,000 indexes on CI, over the 500 ms
// catalog budget: the join to pg_stat_user_indexes (a view over pg_class,
// pg_index and pg_namespace again) for one counter. The read now calls the
// counter's function directly and must return exactly the rows the view
// join returned.
const viewJoinIndexesSQL = tag + `SELECT i.indexrelid, i.indrelid, n.nspname::text, t.relname::text,
  c.relname::text, i.indkey::int2[], i.indnkeyatts::int, i.indclass::oid[],
  i.indcollation::oid[],
  am.amname::text, i.indisunique, i.indisprimary,
  EXISTS (SELECT 1 FROM pg_catalog.pg_constraint con WHERE con.conindid = i.indexrelid
          AND con.contype IN ('p', 'u', 'x')),
  i.indisvalid, i.indisready,
  COALESCE(pg_catalog.pg_get_expr(i.indpred, i.indrelid), ''),
  COALESCE(pg_catalog.pg_get_expr(i.indexprs, i.indrelid), ''),
  c.relpages::bigint * current_setting('block_size')::bigint,
  COALESCE(s.idx_scan, 0)
FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
JOIN pg_catalog.pg_am am ON am.oid = c.relam
LEFT JOIN pg_catalog.pg_stat_user_indexes s ON s.indexrelid = i.indexrelid
WHERE ` + userSchemas + `
ORDER BY i.indexrelid
LIMIT $1`

func indexRows(t *testing.T, ctx context.Context, sql string) []string {
	t.Helper()
	pool, _ := livePool(t)
	rows, err := pool.Query(ctx, sql, maxIndexRows)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(vals))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestIndexReadMatchesTheStatisticsViewJoin(t *testing.T) {
	admin, ctx := livePool(t)
	seedProblems(t, ctx, admin)
	// Scanned and never-scanned indexes, a partial and an expression index.
	for _, s := range []string{
		"DROP SCHEMA IF EXISTS idxread CASCADE", "CREATE SCHEMA idxread",
		"CREATE TABLE idxread.t (id int PRIMARY KEY, a int, b text)",
		"INSERT INTO idxread.t SELECT g, g % 10, g::text FROM generate_series(1, 2000) g",
		"CREATE INDEX t_a ON idxread.t (a)",
		"CREATE INDEX t_part ON idxread.t (b) WHERE a = 1",
		"CREATE INDEX t_expr ON idxread.t (lower(b))",
		"ANALYZE idxread.t",
		"SET enable_seqscan = off",
		"SELECT count(*) FROM idxread.t WHERE a = 3",
		"SELECT pg_stat_force_next_flush()",
		"RESET enable_seqscan",
	} {
		if _, err := admin.Exec(ctx, s); err != nil && !strings.Contains(s, "flush") {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS idxread CASCADE")
	})
	want := indexRows(t, ctx, viewJoinIndexesSQL)
	got := indexRows(t, ctx, indexesSQL)
	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("index read differs from the view join:\n got %d rows %v\nwant %d rows %v",
			len(got), got, len(want), want)
	}
}

func TestIndexReadDoesNotJoinTheStatisticsView(t *testing.T) {
	if strings.Contains(indexesSQL, "pg_stat_user_indexes") ||
		!strings.Contains(indexesSQL, "pg_stat_get_numscans(i.indexrelid)") {
		t.Fatalf("index read still joins the statistics view:\n%s", indexesSQL)
	}
}
