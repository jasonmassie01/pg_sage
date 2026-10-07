package autonomy

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// windowStructuralPathologySQL is the pre-v2.3.1 structural scan (a window
// over every column of every table). The per-table aggregate that replaced
// it must return exactly its rows, in its order.
const windowStructuralPathologySQL = `
WITH columns AS (
    SELECT ns.nspname AS schema_name, tbl.relname AS table_name,
           att.attname AS column_name, typ.typname AS type_name,
           count(*) OVER (PARTITION BY tbl.oid) AS column_count,
           count(*) FILTER (WHERE typ.typname IN ('text','varchar'))
             OVER (PARTITION BY tbl.oid) AS text_count
    FROM pg_class tbl
    JOIN pg_namespace ns ON ns.oid=tbl.relnamespace
    JOIN pg_attribute att ON att.attrelid=tbl.oid
      AND att.attnum>0 AND NOT att.attisdropped
    JOIN pg_type typ ON typ.oid=att.atttypid
    WHERE tbl.relkind IN ('r','p')
      AND ns.nspname NOT IN ('pg_catalog','information_schema','pg_toast','sage')
)
SELECT DISTINCT schema_name, table_name, '' AS column_name, 'everything_text' AS kind
FROM columns WHERE column_count>=3 AND text_count=column_count
UNION ALL
SELECT schema_name, table_name, column_name, 'type_tightening' AS kind
FROM columns WHERE type_name IN ('text','varchar')
  AND (column_name='count_text' OR column_name ~ '(_id|_count|_number)$')
ORDER BY 1,2,4,3`

func structuralRows(t *testing.T, sql string) []string {
	t.Helper()
	pool := requireAutonomyDB(t)
	rows, err := pool.Query(context.Background(), sql)
	if err != nil {
		t.Fatalf("structural scan: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s, tb, c, k string
		if err := rows.Scan(&s, &tb, &c, &k); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s", s, tb, c, k))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}

func TestStructuralPathologySQLMatchesTheWindowScan(t *testing.T) {
	pool := requireAutonomyDB(t)
	execAll(t, pool, "DROP SCHEMA IF EXISTS structfx CASCADE", "CREATE SCHEMA structfx",
		// everything text, three columns or more; two of them tightening candidates
		"CREATE TABLE structfx.all_text (a text, user_id varchar(10), hit_count text)",
		// everything text but only two columns: no finding
		"CREATE TABLE structfx.two_text (a text, b text)",
		// mixed: tightening candidates only, including count_text and a dropped one
		`CREATE TABLE structfx.mixed (id int, order_number text, count_text text,
			note text, gone_id text, "Weird_ID" text)`,
		"ALTER TABLE structfx.mixed DROP COLUMN gone_id",
		// a numeric _id column is not a candidate
		"CREATE TABLE structfx.typed (account_id bigint, label text, z_id text)",
		// a partitioned parent counts, its partition too
		"CREATE TABLE structfx.parted (k int, shard_id text) PARTITION BY RANGE (k)",
		"CREATE TABLE structfx.parted_1 PARTITION OF structfx.parted FOR VALUES FROM (0) TO (10)",
		// a view never counts
		"CREATE VIEW structfx.v AS SELECT 'x'::text AS a, 'y'::text AS b, 'z'::text AS c_id",
		// sage's own tables are excluded
		"CREATE SCHEMA IF NOT EXISTS sage",
		"CREATE TABLE IF NOT EXISTS sage.structfx_probe (a text, b text, c_id text)")
	t.Cleanup(func() {
		execAll(t, pool, "DROP SCHEMA IF EXISTS structfx CASCADE",
			"DROP TABLE IF EXISTS sage.structfx_probe")
	})
	want := structuralRows(t, windowStructuralPathologySQL)
	got := structuralRows(t, structuralPathologySQL)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("aggregate scan differs from the window scan:\n got %q\nwant %q", got, want)
	}
	mine := 0
	for _, r := range got {
		if strings.HasPrefix(r, "structfx|") {
			mine++
		}
	}
	// all_text: everything_text + user_id + hit_count; mixed: order_number,
	// count_text, Weird_ID is not lower-case _id; typed: z_id; parted and
	// parted_1: shard_id.
	if mine != 8 {
		t.Fatalf("fixture rows = %d, want 8:\n%s", mine, strings.Join(got, "\n"))
	}
}

func TestStructuralPathologySQLHasNoWindowOverEveryColumn(t *testing.T) {
	if strings.Contains(structuralPathologySQL, "OVER (") {
		t.Fatalf("structural scan still windows every column:\n%s", structuralPathologySQL)
	}
}
