package ask

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// describe_table's read of the monitored database: one read-only
// transaction bounded by statement_timeout and lock_timeout. A partitioned
// parent owns no storage, keeps reltuples at -1 until analyzed by hand and
// is never vacuumed, so its own numbers are replaced by its leaves'.

var (
	// maxDescribePartitions caps the leaves whose sizes, rows and vacuum
	// times are summed; the leaf count itself is always complete.
	maxDescribePartitions = 2000
	// describeStatementTimeout and describeLockTimeout bound the read; a
	// zero lock timeout disables it (tests use that to hit the other bound).
	describeStatementTimeout = 2 * time.Second
	describeLockTimeout      = time.Second
)

// tableShape is one relation's catalog row as describe_table reports it.
type tableShape struct {
	oid                                        uint32
	schema, name, relkind, partKey             string
	size, rows                                 int64
	comment, columns, indexes, vacuum, analyze string
	ncols                                      int
}

// leafStats aggregates the leaves of a partitioned table: total counts
// every leaf, the rest cover the first covered leaves in oid order.
type leafStats struct {
	total, covered, analyzed, size, rows       int64
	vacOldest, vacNewest, anaOldest, anaNewest string
}

const tableShapeSQL = `/* pg_sage */ SELECT c.oid, n.nspname, c.relname,
	c.relkind::text, COALESCE(pg_get_partkeydef(c.oid), ''), pg_total_relation_size(c.oid),
	c.reltuples::bigint, COALESCE(obj_description(c.oid, 'pg_class'), ''),
	(SELECT count(*) FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attnum > 0
	   AND NOT a.attisdropped),
	COALESCE((SELECT string_agg(a.attname || ' ' || format_type(a.atttypid,
	   a.atttypmod), ', ' ORDER BY a.attnum) FROM pg_attribute a
	   WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped), ''),
	COALESCE((SELECT string_agg(pg_get_indexdef(i.indexrelid), '; ' ORDER BY
	   i.indexrelid) FROM pg_index i WHERE i.indrelid = c.oid), ''),
	COALESCE(GREATEST(s.last_vacuum, s.last_autovacuum)::text, 'never'),
	COALESCE(GREATEST(s.last_analyze, s.last_autoanalyze)::text, 'never')
	FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
	LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
	WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p', 'm')`

// leafStatsSQL walks pg_inherits rather than calling pg_partition_tree,
// which holds AccessShareLock on every partition until commit: on a table
// with thousands of partitions that could exhaust the shared lock table.
// Only pg_total_relation_size opens a leaf, and it releases its lock.
const leafStatsSQL = `/* pg_sage */ WITH RECURSIVE tree AS (
	  SELECT $1::oid AS relid
	  UNION ALL SELECT i.inhrelid FROM pg_inherits i JOIN tree t ON i.inhparent = t.relid),
	leaves AS (
	  SELECT c.oid AS relid, row_number() OVER (ORDER BY c.oid) AS n
	  FROM tree t JOIN pg_class c ON c.oid = t.relid WHERE c.relkind <> 'p'),
	covered AS (
	  SELECT c.oid, c.reltuples, GREATEST(s.last_vacuum, s.last_autovacuum) AS vac,
	         GREATEST(s.last_analyze, s.last_autoanalyze) AS ana
	  FROM leaves l JOIN pg_class c ON c.oid = l.relid
	  LEFT JOIN pg_stat_user_tables s ON s.relid = l.relid
	  WHERE l.n <= $2)
	SELECT (SELECT count(*) FROM leaves), count(*), count(*) FILTER (WHERE reltuples >= 0),
	  COALESCE(sum(pg_total_relation_size(oid)), 0)::bigint,
	  COALESCE(sum(reltuples) FILTER (WHERE reltuples > 0), 0)::bigint,
	  CASE WHEN bool_and(vac IS NOT NULL) THEN min(vac)::text ELSE 'never' END,
	  COALESCE(max(vac)::text, 'never'),
	  CASE WHEN bool_and(ana IS NOT NULL) THEN min(ana)::text ELSE 'never' END,
	  COALESCE(max(ana)::text, 'never')
	FROM covered`

func (ss *session) tableText(ctx context.Context, schema, name string) (string, error) {
	tx, err := ss.s.d.Pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", fmt.Errorf("begin read-only transaction: %w", err)
	}
	// Read-only: nothing to undo, and a failed rollback discards the conn.
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true),
		set_config('lock_timeout', $2, true)`, millis(describeStatementTimeout),
		millis(describeLockTimeout)); err != nil {
		return "", fmt.Errorf("bound the read: %w", err)
	}
	t, err := resolveTable(ctx, tx, schema, name)
	if err != nil {
		return "", err
	}
	if t.relkind != "p" {
		return plainTableText(t), nil
	}
	var l leafStats
	if err := tx.QueryRow(ctx, leafStatsSQL, t.oid, int64(maxDescribePartitions)).Scan(
		&l.total, &l.covered, &l.analyzed, &l.size, &l.rows, &l.vacOldest, &l.vacNewest,
		&l.anaOldest, &l.anaNewest); err != nil {
		return "", fmt.Errorf("read partitions: %w", err)
	}
	return partitionedText(t, l), nil
}

func millis(d time.Duration) string { return fmt.Sprintf("%dms", d.Milliseconds()) }

// resolveTable matches schema and name exactly, then, on a miss, as SQL
// would fold them unquoted. The argument rule admits no quotes, so an
// exact match is the only way to reach a mixed-case name.
func resolveTable(ctx context.Context, tx pgx.Tx, schema, name string) (tableShape, error) {
	t, err := scanShape(tx.QueryRow(ctx, tableShapeSQL, schema, name))
	ls, ln := strings.ToLower(schema), strings.ToLower(name)
	if errors.Is(err, pgx.ErrNoRows) && (ls != schema || ln != name) {
		t, err = scanShape(tx.QueryRow(ctx, tableShapeSQL, ls, ln))
	}
	return t, err
}

func scanShape(row pgx.Row) (tableShape, error) {
	var t tableShape
	err := row.Scan(&t.oid, &t.schema, &t.name, &t.relkind, &t.partKey, &t.size, &t.rows,
		&t.comment, &t.ncols, &t.columns, &t.indexes, &t.vacuum, &t.analyze)
	return t, err
}

// rowPhrase: reltuples is -1 until the first ANALYZE (PG 14+).
func rowPhrase(rows int64) string {
	if rows < 0 {
		return "row estimate unavailable (never analyzed)"
	}
	return fmt.Sprintf("about %d rows", rows)
}

func plainTableText(t tableShape) string {
	var b strings.Builder
	fmt.Fprintf(&b, "table %s.%s: %d bytes total, %s, %d columns\n", t.schema, t.name,
		t.size, rowPhrase(t.rows), t.ncols)
	shapeBody(&b, t)
	fmt.Fprintf(&b, "last vacuum %s, last analyze %s", t.vacuum, t.analyze)
	return b.String()
}

func partitionedText(t tableShape, l leafStats) string {
	rows := rowPhrase(-1)
	if l.analyzed > 0 {
		rows = rowPhrase(l.rows)
		if never := l.covered - l.analyzed; never > 0 {
			rows += fmt.Sprintf(" (%d partitions never analyzed)", never)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "partitioned table %s.%s (%s): %d partitions, %d bytes in partitions, "+
		"%s, %d columns\n", t.schema, t.name, t.partKey, l.total, l.size, rows, t.ncols)
	if l.covered < l.total {
		fmt.Fprintf(&b, "sizes, rows and vacuum times cover the first %d of %d partitions\n",
			l.covered, l.total)
	}
	shapeBody(&b, t)
	fmt.Fprintf(&b, "partition vacuum oldest %s, newest %s; partition analyze oldest %s, "+
		"newest %s", l.vacOldest, l.vacNewest, l.anaOldest, l.anaNewest)
	return b.String()
}

func shapeBody(b *strings.Builder, t tableShape) {
	optional(b, "comment", oneLine(t.comment))
	fmt.Fprintf(b, "columns: %s\n", clip(t.columns, 1500))
	optional(b, "indexes", clip(t.indexes, 1500))
}
