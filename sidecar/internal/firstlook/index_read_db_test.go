package firstlook

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// The index read took 436-540 ms at 15,000 indexes on CI, over the 500 ms
// catalog budget: the join to pg_stat_user_indexes (a view over pg_class,
// pg_index and pg_namespace again) for one counter. The read must return
// exactly the indexes the view join returned.
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

// previousIndexesSQL is the read before the large-catalog rework (#152):
// one row per index with its arrays, flags and names in separate columns.
// At 15,000 indexes it took 331-529 ms on CI (planned as nested loops that
// tested every row against every schema) and sent 3.3 MB. The rework must
// return exactly the same indexes.
const previousIndexesSQL = tag + `SELECT i.indexrelid, i.indrelid, n.nspname::text, t.relname::text,
  c.relname::text, i.indkey::int2[], i.indnkeyatts::int, i.indclass::oid[],
  i.indcollation::oid[],
  am.amname::text, i.indisunique, i.indisprimary,
  EXISTS (SELECT 1 FROM pg_catalog.pg_constraint con WHERE con.conindid = i.indexrelid
          AND con.contype IN ('p', 'u', 'x')),
  i.indisvalid, i.indisready,
  COALESCE(pg_catalog.pg_get_expr(i.indpred, i.indrelid), ''),
  COALESCE(pg_catalog.pg_get_expr(i.indexprs, i.indrelid), ''),
  c.relpages::bigint * current_setting('block_size')::bigint,
  pg_catalog.pg_stat_get_numscans(i.indexrelid)
FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
JOIN pg_catalog.pg_am am ON am.oid = c.relam
WHERE ` + userSchemas + `
ORDER BY i.indexrelid
LIMIT $1`

// legacyIndexes runs a reference query (19 columns, arrays and flags as
// separate columns) and scans it as the read did before the rework.
func legacyIndexes(t *testing.T, ctx context.Context, tx pgx.Tx, sql string,
	limit int) []Index {
	t.Helper()
	rows, err := tx.Query(ctx, sql, limit)
	if err != nil {
		t.Fatalf("reference read: %v", err)
	}
	defer rows.Close()
	var out []Index
	for rows.Next() {
		var x Index
		if err := rows.Scan(&x.OID, &x.TableOID, &x.Schema, &x.Table, &x.Name, &x.Columns,
			&x.KeyColumns, &x.OpClasses, &x.Collations, &x.AccessMethod, &x.Unique, &x.Primary,
			&x.ConstraintBacked, &x.Valid, &x.Ready, &x.Predicate, &x.Expressions,
			&x.SizeBytes, &x.Scans); err != nil {
			t.Fatalf("reference scan: %v", err)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reference read: %v", err)
	}
	return out
}

// snapshotTx is a read-only repeatable-read transaction, as the first look
// runs in: both reads of a comparison see the same catalog and counters.
func snapshotTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

func requireSameIndexes(t *testing.T, got, want []Index) {
	t.Helper()
	if len(want) == 0 {
		t.Fatal("reference read returned no indexes: the fixture is missing")
	}
	if reflect.DeepEqual(got, want) {
		return
	}
	if len(got) != len(want) {
		t.Fatalf("index read returned %d indexes, reference %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("index %d differs:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
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
	} {
		if _, err := admin.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA IF EXISTS idxread CASCADE")
	})
	scanOnce(t, ctx, admin, "SELECT count(*) FROM idxread.t WHERE a = 3")
	tx := snapshotTx(t, ctx, admin)
	want := legacyIndexes(t, ctx, tx, viewJoinIndexesSQL, maxIndexRows)
	got, truncated, err := readIndexes(ctx, tx)
	if err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	if truncated {
		t.Fatalf("%d indexes reported as truncated at %d", len(got), maxIndexRows)
	}
	requireSameIndexes(t, got, want)
	if scans := byName(t, got, "idxread", "t_a").Scans; scans < 1 {
		t.Fatalf("t_a was scanned, the read says %d scans", scans)
	}
}

// scanOnce runs query by index on one session and flushes that session's
// statistics, so the scan is counted before the read (PostgreSQL 14's
// collector applies reports asynchronously).
func scanOnce(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for _, s := range []string{"SET enable_seqscan = off", query, "RESET enable_seqscan"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := testdb.FlushStats(ctx, conn); err != nil {
		t.Fatalf("flush statistics: %v", err)
	}
}

func TestIndexReadDoesNotJoinTheStatisticsView(t *testing.T) {
	if strings.Contains(indexesSQL, "pg_stat_user_indexes") ||
		!strings.Contains(indexesSQL, "pg_stat_get_numscans(i.indexrelid)") {
		t.Fatalf("index read still joins the statistics view:\n%s", indexesSQL)
	}
}

// A correlated EXISTS per index made the planner cost the read at millions
// of units and pick nested loops that compared each index's table against
// every schema (1.28 million comparisons at 15,000 indexes). The constraint
// lookup is one hashed set instead.
func TestIndexReadLooksUpConstraintsAsOneSet(t *testing.T) {
	if strings.Contains(indexesSQL, "EXISTS") ||
		!strings.Contains(indexesSQL, "SELECT DISTINCT con.conindid") {
		t.Fatalf("index read looks constraints up per index:\n%s", indexesSQL)
	}
}

// The read sends each index's key columns, operator classes and collations
// as the vectors' text form (one value each) and its five flags as one
// integer: half the bytes of one array and one boolean column apiece.
func TestIndexReadSendsCompactColumns(t *testing.T) {
	for _, want := range []string{"i.indkey::text", "i.indclass::text",
		"i.indcollation::text"} {
		if !strings.Contains(indexesSQL, want) {
			t.Errorf("index read does not send %s", want)
		}
	}
	for _, wide := range []string{"::int2[]", "::oid[]"} {
		if strings.Contains(indexesSQL, wide) {
			t.Errorf("index read still sends %s arrays", wide)
		}
	}
}

func byName(t *testing.T, idx []Index, schema, name string) Index {
	t.Helper()
	for _, x := range idx {
		if x.Schema == schema && x.Name == name {
			return x
		}
	}
	t.Fatalf("index %s.%s not read", schema, name)
	return Index{}
}

// variedIndexes creates every index shape the rules read: partial,
// expression, INCLUDE, unique, primary key, unique and exclusion
// constraints, an invalid index, several per table, non-btree methods,
// explicit collations and operator classes, a partitioned table, a
// materialized view and names that need quoting.
func variedIndexes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	s := uniqueSchema("Idx Read ")
	q := pgx.Identifier{s}.Sanitize()
	execAll(t, ctx, pool,
		"CREATE SCHEMA "+q,
		"CREATE TABLE "+q+`.t (id int PRIMARY KEY, a int, b text, c text COLLATE "C",
			r int4range, j jsonb, u int UNIQUE, EXCLUDE USING gist (r WITH &&))`,
		"INSERT INTO "+q+".t SELECT g, g % 10, g::text, g::text, int4range(g, g + 1), "+
			"jsonb_build_object('k', g), g FROM generate_series(1, 500) g",
		"CREATE INDEX t_a ON "+q+".t (a)",
		"CREATE INDEX t_a_dup ON "+q+".t (a)",
		"CREATE INDEX t_a_b ON "+q+".t (a, b DESC NULLS LAST)",
		"CREATE INDEX t_part ON "+q+".t (b) WHERE a = 1 AND b <> 'x'",
		"CREATE INDEX t_expr ON "+q+".t (lower(b), (a + 1))",
		"CREATE INDEX t_incl ON "+q+".t (a) INCLUDE (b, c)",
		"CREATE UNIQUE INDEX t_uniq ON "+q+".t (b)",
		"CREATE INDEX t_coll ON "+q+`.t (b COLLATE "C", c)`,
		"CREATE INDEX t_ops ON "+q+".t (b text_pattern_ops)",
		"CREATE INDEX t_hash ON "+q+".t USING hash (a)",
		"CREATE INDEX t_gin ON "+q+".t USING gin (j)",
		"CREATE INDEX t_brin ON "+q+".t USING brin (id)",
		"CREATE INDEX t_wide ON "+q+".t (a, id, u, b, c)",
		`CREATE INDEX "Odd ""Name""" ON `+q+".t (u)",
		"CREATE TABLE "+q+".p (id int, k int, PRIMARY KEY (id, k)) PARTITION BY RANGE (k)",
		"CREATE TABLE "+q+".p1 PARTITION OF "+q+".p FOR VALUES FROM (0) TO (100)",
		"CREATE INDEX p_k ON "+q+".p (k)",
		"CREATE MATERIALIZED VIEW "+q+".mv AS SELECT a, count(*) n FROM "+q+".t GROUP BY a",
		"CREATE UNIQUE INDEX mv_a ON "+q+".mv (a)",
		"CREATE TABLE "+q+".dupvals (v int)",
		"INSERT INTO "+q+".dupvals VALUES (1), (1)",
		"ANALYZE "+q+".t",
	)
	if _, err := pool.Exec(ctx, "CREATE UNIQUE INDEX CONCURRENTLY dupvals_v ON "+q+
		".dupvals (v)"); err == nil {
		t.Fatal("unique build over duplicate values unexpectedly succeeded")
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+q+" CASCADE")
	})
	return s
}

func TestIndexReadMatchesThePreviousRead(t *testing.T) {
	pool, ctx := livePool(t)
	s := variedIndexes(t, ctx, pool)
	tx := snapshotTx(t, ctx, pool)
	want := legacyIndexes(t, ctx, tx, previousIndexesSQL, maxIndexRows)
	got, truncated, err := readIndexes(ctx, tx)
	if err != nil {
		t.Fatalf("read indexes: %v", err)
	}
	if truncated {
		t.Fatalf("%d indexes reported as truncated", len(got))
	}
	requireSameIndexes(t, got, want)
	requireVariedShapes(t, got, s)
}

// requireVariedShapes checks the fixture's shapes arrived as the rules
// need them, so the comparison above covers every kind.
func requireVariedShapes(t *testing.T, got []Index, s string) {
	t.Helper()
	checks := []struct {
		name string
		ok   func(Index) bool
	}{
		{"t_pkey", func(x Index) bool { return x.Primary && x.Unique && x.ConstraintBacked }},
		{"t_u_key", func(x Index) bool { return x.Unique && x.ConstraintBacked && !x.Primary }},
		{"t_r_excl", func(x Index) bool {
			return x.ConstraintBacked && !x.Unique && x.AccessMethod == "gist"
		}},
		{"t_uniq", func(x Index) bool { return x.Unique && !x.ConstraintBacked }},
		{"t_part", func(x Index) bool { return strings.Contains(x.Predicate, "a = 1") }},
		{"t_expr", func(x Index) bool {
			return strings.Contains(x.Expressions, "lower(b)") &&
				reflect.DeepEqual(x.Columns, []int16{0, 0})
		}},
		{"t_incl", func(x Index) bool { return x.KeyColumns == 1 && len(x.Columns) == 3 }},
		{"t_coll", func(x Index) bool { return len(x.Collations) == 2 && x.Collations[0] != 0 }},
		{"t_ops", func(x Index) bool { return len(x.OpClasses) == 1 && x.OpClasses[0] != 0 }},
		{"t_hash", func(x Index) bool { return x.AccessMethod == "hash" }},
		{"t_gin", func(x Index) bool { return x.AccessMethod == "gin" }},
		{"t_brin", func(x Index) bool { return x.AccessMethod == "brin" }},
		{"t_wide", func(x Index) bool {
			return reflect.DeepEqual(x.Columns, []int16{2, 1, 7, 3, 4})
		}},
		{`Odd "Name"`, func(x Index) bool { return x.Table == "t" && x.Valid }},
		{"p_k", func(x Index) bool { return x.Table == "p" && x.Valid }},
		{"mv_a", func(x Index) bool { return x.Table == "mv" && x.Unique }},
		{"dupvals_v", func(x Index) bool { return !x.Valid && x.Unique }},
		{"t_a", func(x Index) bool { return x.SizeBytes > 0 && x.Ready && x.Valid }},
	}
	for _, c := range checks {
		if x := byName(t, got, s, c.name); !c.ok(x) {
			t.Errorf("%s read as %+v", c.name, x)
		}
	}
}

// The row bound keeps the lowest OIDs and reports truncation when the
// bound is reached: one under, at and over the number of indexes.
func TestIndexReadBoundary(t *testing.T) {
	pool, ctx := livePool(t)
	variedIndexes(t, ctx, pool)
	tx := snapshotTx(t, ctx, pool)
	all := legacyIndexes(t, ctx, tx, previousIndexesSQL, maxIndexRows)
	n := len(all)
	for _, c := range []struct {
		limit     int
		truncated bool
	}{{1, true}, {n - 1, true}, {n, true}, {n + 1, false}} {
		got, truncated, err := readIndexesUpTo(ctx, tx, c.limit)
		if err != nil {
			t.Fatalf("limit %d: %v", c.limit, err)
		}
		want := all[:min(c.limit, n)]
		requireSameIndexes(t, got, want)
		if truncated != c.truncated {
			t.Errorf("limit %d of %d indexes: truncated %t, want %t", c.limit, n, truncated,
				c.truncated)
		}
	}
	got, truncated, err := readIndexesUpTo(ctx, tx, 0)
	if err != nil || len(got) != 0 || !truncated {
		t.Errorf("limit 0: %d indexes, truncated %t, err %v; want none, truncated", len(got),
			truncated, err)
	}
}

// Failures name the step: an aborted transaction and a cancelled context.
func TestIndexReadErrors(t *testing.T) {
	pool, ctx := livePool(t)
	tx := snapshotTx(t, ctx, pool)
	if _, err := tx.Exec(ctx, "SELECT 1/0"); err == nil {
		t.Fatal("division by zero succeeded")
	}
	if _, _, err := readIndexes(ctx, tx); err == nil ||
		!strings.Contains(err.Error(), "read indexes") {
		t.Fatalf("aborted transaction: %v, want a read indexes error", err)
	}
	tx2 := snapshotTx(t, ctx, pool)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, _, err := readIndexes(cancelled, tx2)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context: %v, want context.Canceled", err)
	}
}

// No concurrent-access test: readIndexes keeps no state; each call reads
// in the caller's transaction.
