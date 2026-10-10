package firstlook

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Every first-look statement reads only the system catalog and the
// statistics views, is bounded by a LIMIT, and carries this tag so its cost
// is visible in pg_stat_statements (the perf gate checks it).
const tag = "/* pg_sage first_look */ "

// userSchemas excludes system schemas and pg_sage's own.
const userSchemas = `n.nspname NOT IN ('pg_catalog', 'information_schema', 'sage')
  AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp'`

// Row bounds per catalog read.
const (
	maxIndexRows    = 100000
	maxFKRows       = 50000
	maxSequenceRows = 50000
	maxBloatRows    = 200
	maxSchemaRows   = 500
)

const relationsSQL = tag + `SELECT count(*)::int FROM pg_catalog.pg_class`

const statsWindowSQL = tag + `SELECT (SELECT stats_reset FROM pg_catalog.pg_stat_database
  WHERE datname = current_database()), pg_catalog.pg_postmaster_start_time()`

func readStatsWindow(ctx context.Context, tx pgx.Tx) (StatsWindow, error) {
	var reset *time.Time
	var started time.Time
	if err := tx.QueryRow(ctx, statsWindowSQL).Scan(&reset, &started); err != nil {
		return StatsWindow{}, fmt.Errorf("read statistics window: %w", err)
	}
	if reset != nil {
		return StatsWindow{Since: *reset, Source: "stats_reset", Known: true}, nil
	}
	return StatsWindow{Since: started, Source: "server_start", Known: true}, nil
}

// indexesSQL reads every user index. The scan count comes from the
// statistics function, not the pg_stat_user_indexes view: the view's
// join planned the read at millions of cost units and took 436-540 ms at
// 15,000 indexes (nightly perf gate).
const indexesSQL = tag + `SELECT i.indexrelid, i.indrelid, n.nspname::text, t.relname::text,
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

func readIndexes(ctx context.Context, tx pgx.Tx) ([]Index, bool, error) {
	rows, err := tx.Query(ctx, indexesSQL, maxIndexRows)
	if err != nil {
		return nil, false, fmt.Errorf("read indexes: %w", err)
	}
	defer rows.Close()
	var out []Index
	for rows.Next() {
		var x Index
		if err := rows.Scan(&x.OID, &x.TableOID, &x.Schema, &x.Table, &x.Name, &x.Columns,
			&x.KeyColumns, &x.OpClasses, &x.Collations, &x.AccessMethod, &x.Unique, &x.Primary,
			&x.ConstraintBacked, &x.Valid, &x.Ready, &x.Predicate, &x.Expressions,
			&x.SizeBytes, &x.Scans); err != nil {
			return nil, false, fmt.Errorf("scan index: %w", err)
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read indexes: %w", err)
	}
	return out, len(out) == maxIndexRows, nil
}

const foreignKeysSQL = tag + `SELECT con.conname::text, n.nspname::text, t.relname::text,
  con.conrelid, con.conkey,
  ARRAY(SELECT a.attname::text FROM unnest(con.conkey) WITH ORDINALITY k(attnum, ord)
        JOIN pg_catalog.pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
        ORDER BY k.ord),
  rn.nspname::text || '.' || rt.relname::text, t.reltuples::float8
FROM pg_catalog.pg_constraint con
JOIN pg_catalog.pg_class t ON t.oid = con.conrelid
JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
JOIN pg_catalog.pg_class rt ON rt.oid = con.confrelid
JOIN pg_catalog.pg_namespace rn ON rn.oid = rt.relnamespace
WHERE con.contype = 'f' AND con.conparentid = 0 AND ` + userSchemas + `
ORDER BY con.oid
LIMIT $1`

func readForeignKeys(ctx context.Context, tx pgx.Tx) ([]ForeignKey, error) {
	rows, err := tx.Query(ctx, foreignKeysSQL, maxFKRows)
	if err != nil {
		return nil, fmt.Errorf("read foreign keys: %w", err)
	}
	defer rows.Close()
	var out []ForeignKey
	for rows.Next() {
		var fk ForeignKey
		if err := rows.Scan(&fk.Name, &fk.Schema, &fk.Table, &fk.TableOID, &fk.Columns,
			&fk.ColumnNames, &fk.RefTable, &fk.TableRows); err != nil {
			return nil, fmt.Errorf("scan foreign key: %w", err)
		}
		out = append(out, fk)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read foreign keys: %w", err)
	}
	return out, nil
}

const xidSQL = tag + `SELECT datname::text, age(datfrozenxid)::bigint,
  mxid_age(datminmxid)::bigint, current_setting('autovacuum_freeze_max_age')::bigint
FROM pg_catalog.pg_database WHERE datname = current_database()`

const oldestTablesSQL = tag + `SELECT n.nspname::text, c.relname::text,
  age(c.relfrozenxid)::bigint
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'm', 't') AND c.relfrozenxid <> '0'::xid
ORDER BY age(c.relfrozenxid) DESC
LIMIT 3`

func readXID(ctx context.Context, tx pgx.Tx) (XIDState, error) {
	var x XIDState
	if err := tx.QueryRow(ctx, xidSQL).Scan(&x.Database, &x.DatFrozenXIDAge,
		&x.DatMinMXIDAge, &x.FreezeMaxAge); err != nil {
		return x, fmt.Errorf("read transaction ID horizon: %w", err)
	}
	rows, err := tx.Query(ctx, oldestTablesSQL)
	if err != nil {
		return x, fmt.Errorf("read oldest tables: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var t TableAge
		if err := rows.Scan(&t.Schema, &t.Table, &t.Age); err != nil {
			return x, fmt.Errorf("scan oldest table: %w", err)
		}
		x.OldestTables = append(x.OldestTables, t)
	}
	return x, rows.Err()
}

// sequencesSQL reads every user sequence with the column it feeds: the
// column that owns it (serial, identity, OWNED BY) or any column whose
// DEFAULT calls nextval() on it. When several columns use one sequence the
// narrowest integer type caps it, so each sequence stays one row.
// last_value is NULL when the role may not read the sequence, and also
// when it was never used; the privilege column tells the two apart.
//
// The plan must not depend on catalog statistics: a freshly restored or
// migrated database has not analyzed pg_depend yet, and the earlier read
// (through pg_sequences, matching names) then read pg_depend by classid
// alone for every sequence: 11-14 s at 5,000 sequences. So the read starts
// at pg_sequence and joins by OID, without a relkind filter whose stale
// estimate is zero rows, and each pg_depend lookup is keyed by the
// sequence: (classid, objid) for the owning column and (refclassid,
// refobjid) for DEFAULTs. The attrdef side compares classid to the
// tableoid of pg_attrdef instead of a constant, so no constant-classid
// prefix of pg_depend_depender_index can compete with the keyed index.
// The last_value CASE matches pg_sequences on every major version.
const sequencesSQL = tag + `SELECT n.nspname::text, sc.relname::text,
  CASE WHEN pg_catalog.has_sequence_privilege(sc.oid, 'SELECT,USAGE')
    THEN pg_catalog.pg_sequence_last_value(sc.oid::regclass) END,
  sq.seqmin, sq.seqmax, sq.seqincrement, sq.seqcycle,
  pg_catalog.has_sequence_privilege(sc.oid, 'SELECT,USAGE'),
  COALESCE(col.name, ''), COALESCE(col.type, '')
FROM pg_catalog.pg_sequence sq
JOIN pg_catalog.pg_class sc ON sc.oid = sq.seqrelid
JOIN pg_catalog.pg_namespace n ON n.oid = sc.relnamespace
LEFT JOIN LATERAL (
  SELECT dn.nspname::text || '.' || dt.relname::text || '.' || a.attname::text AS name,
    pg_catalog.format_type(a.atttypid, a.atttypmod) AS type
  FROM (SELECT d.refobjid AS relid, d.refobjsubid AS attnum FROM pg_catalog.pg_depend d
        WHERE d.classid = 'pg_catalog.pg_class'::regclass AND d.objid = sq.seqrelid
          AND d.refclassid = 'pg_catalog.pg_class'::regclass
          AND d.deptype IN ('a', 'i') AND d.refobjsubid > 0
        UNION
        SELECT ad.adrelid, ad.adnum FROM pg_catalog.pg_depend d
        JOIN pg_catalog.pg_attrdef ad ON ad.tableoid = d.classid AND ad.oid = d.objid
        WHERE d.refclassid = 'pg_catalog.pg_class'::regclass AND d.refobjid = sq.seqrelid
          AND d.deptype = 'n') u
  JOIN pg_catalog.pg_attribute a ON a.attrelid = u.relid AND a.attnum = u.attnum
    AND NOT a.attisdropped
  JOIN pg_catalog.pg_class dt ON dt.oid = u.relid
  JOIN pg_catalog.pg_namespace dn ON dn.oid = dt.relnamespace
  ORDER BY CASE a.atttypid WHEN 'pg_catalog.int2'::regtype THEN 1
    WHEN 'pg_catalog.int4'::regtype THEN 2 WHEN 'pg_catalog.int8'::regtype THEN 3
    ELSE 4 END, 1
  LIMIT 1) col ON true
WHERE ` + userSchemas + `
ORDER BY 1, 2
LIMIT $1`

func readSequences(ctx context.Context, tx pgx.Tx) ([]Sequence, error) {
	rows, err := tx.Query(ctx, sequencesSQL, maxSequenceRows)
	if err != nil {
		return nil, fmt.Errorf("read sequences: %w", err)
	}
	defer rows.Close()
	var out []Sequence
	for rows.Next() {
		var s Sequence
		var readable bool
		if err := rows.Scan(&s.Schema, &s.Name, &s.LastValue, &s.Min, &s.Max, &s.Increment,
			&s.Cycle, &readable, &s.OwnerColumn, &s.OwnerType); err != nil {
			return nil, fmt.Errorf("scan sequence: %w", err)
		}
		if readable && s.LastValue == nil {
			continue // never used: nothing consumed
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read sequences: %w", err)
	}
	return out, nil
}

const bloatSQL = tag + `SELECT s.schemaname::text, s.relname::text,
  c.relpages::bigint * current_setting('block_size')::bigint,
  s.n_live_tup, s.n_dead_tup, s.last_autovacuum
FROM pg_catalog.pg_stat_user_tables s
JOIN pg_catalog.pg_class c ON c.oid = s.relid
WHERE s.schemaname <> 'sage'
  AND c.relpages::bigint * current_setting('block_size')::bigint >= $1
  AND s.n_dead_tup > 0
ORDER BY s.n_dead_tup DESC
LIMIT $2`

func readTableStats(ctx context.Context, tx pgx.Tx, minBytes int64) ([]TableStat, error) {
	rows, err := tx.Query(ctx, bloatSQL, minBytes, maxBloatRows)
	if err != nil {
		return nil, fmt.Errorf("read table statistics: %w", err)
	}
	defer rows.Close()
	var out []TableStat
	for rows.Next() {
		var t TableStat
		if err := rows.Scan(&t.Schema, &t.Table, &t.SizeBytes, &t.Live, &t.Dead,
			&t.LastAutovacuum); err != nil {
			return nil, fmt.Errorf("scan table statistics: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read table statistics: %w", err)
	}
	return out, nil
}

const testSchemasSQL = tag + `SELECT n.nspname::text, count(c.oid)::int,
  COALESCE(sum(COALESCE(s.seq_scan, 0) + COALESCE(s.idx_scan, 0) + COALESCE(s.n_tup_ins, 0)
    + COALESCE(s.n_tup_upd, 0) + COALESCE(s.n_tup_del, 0)), 0)::bigint
FROM pg_catalog.pg_namespace n
LEFT JOIN pg_catalog.pg_class c ON c.relnamespace = n.oid AND c.relkind IN ('r', 'p')
LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid
WHERE ` + userSchemas + ` AND n.nspname ~* $1
GROUP BY n.nspname
ORDER BY n.nspname
LIMIT $2`

func readTestSchemas(ctx context.Context, tx pgx.Tx, pattern string) ([]Schema, error) {
	rows, err := tx.Query(ctx, testSchemasSQL, pattern, maxSchemaRows)
	if err != nil {
		return nil, fmt.Errorf("read test schemas: %w", err)
	}
	defer rows.Close()
	var out []Schema
	for rows.Next() {
		var s Schema
		if err := rows.Scan(&s.Name, &s.Tables, &s.Activity); err != nil {
			return nil, fmt.Errorf("scan test schema: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read test schemas: %w", err)
	}
	return out, nil
}

// extensionsSQL reads extension state without superuser: pg_settings hides
// settings the role may not read instead of failing.
const extensionsSQL = tag + `SELECT current_user::text,
  EXISTS (SELECT 1 FROM pg_catalog.pg_extension WHERE extname = 'pg_stat_statements'),
  EXISTS (SELECT 1 FROM pg_catalog.pg_extension WHERE extname = 'hypopg'),
  EXISTS (SELECT 1 FROM pg_catalog.pg_available_extensions WHERE name = 'hypopg'),
  EXISTS (SELECT 1 FROM pg_catalog.pg_settings WHERE name LIKE 'auto\_explain.%'),
  EXISTS (SELECT 1 FROM pg_catalog.pg_settings WHERE name = 'pg_stat_statements.max'),
  (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'shared_preload_libraries'),
  pg_catalog.pg_has_role(current_user, 'pg_read_all_stats', 'USAGE')`

func readExtensions(ctx context.Context, tx pgx.Tx) (Extensions, error) {
	var e Extensions
	var gucs, readStats bool
	if err := tx.QueryRow(ctx, extensionsSQL).Scan(&e.Role, &e.StatStatementsInstalled,
		&e.HypoPGInstalled, &e.HypoPGAvailable, &e.AutoExplainLoaded, &gucs,
		&e.PreloadLibraries, &readStats); err != nil {
		return e, fmt.Errorf("read extensions: %w", err)
	}
	loaded := gucs
	if e.PreloadLibraries != nil {
		loaded = loaded || preloads(*e.PreloadLibraries, "pg_stat_statements")
	}
	if e.PreloadLibraries != nil || gucs {
		e.StatStatementsLoaded = &loaded
	}
	if e.StatStatementsInstalled {
		e.QueryTextVisible = &readStats
	}
	return e, nil
}

func preloads(list, lib string) bool {
	for _, l := range strings.Split(list, ",") {
		if strings.TrimSpace(l) == lib {
			return true
		}
	}
	return false
}
