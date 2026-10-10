package firstlook

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// indexesSQL reads every user index in one statement shaped for large
// catalogs (15,000 indexes in the nightly perf gate):
//   - the scan count comes from the statistics function, not the
//     pg_stat_user_indexes view, whose join planned at millions of cost units;
//   - constraint-backed indexes are one hashed set, not an EXISTS per index:
//     the correlated subquery's cost steered the planner to nested loops that
//     compared each index's table with every schema (1.28 million times);
//   - the subquery (OFFSET 0 keeps it) computes the narrow output columns
//     before the sort, so the sort holds short text instead of 64-byte names
//     and fits in work_mem;
//   - the vectors travel as their text form and the five flags as one
//     integer: 1.9 MB instead of 3.3 MB at 15,000 indexes.
const indexesSQL = tag + `SELECT x.* FROM (
  SELECT i.indexrelid, i.indrelid, n.nspname::text AS schema, t.relname::text AS tbl,
    c.relname::text AS name, i.indkey::text AS indkey, i.indnkeyatts::int AS nkeys,
    i.indclass::text AS indclass, i.indcollation::text AS indcollation,
    am.amname::text AS am,
    (i.indisunique::int + 2 * i.indisprimary::int + 4 * (con.conindid IS NOT NULL)::int
      + 8 * i.indisvalid::int + 16 * i.indisready::int)::int2 AS flags,
    COALESCE(pg_catalog.pg_get_expr(i.indpred, i.indrelid), '') AS pred,
    COALESCE(pg_catalog.pg_get_expr(i.indexprs, i.indrelid), '') AS exprs,
    c.relpages::bigint * (SELECT current_setting('block_size')::bigint) AS size,
    pg_catalog.pg_stat_get_numscans(i.indexrelid) AS scans
  FROM pg_catalog.pg_index i
  JOIN pg_catalog.pg_class c ON c.oid = i.indexrelid
  JOIN pg_catalog.pg_class t ON t.oid = i.indrelid
  JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
  JOIN pg_catalog.pg_am am ON am.oid = c.relam
  LEFT JOIN (SELECT DISTINCT con.conindid FROM pg_catalog.pg_constraint con
             WHERE con.contype IN ('p', 'u', 'x')) con ON con.conindid = i.indexrelid
  WHERE ` + userSchemas + `
  OFFSET 0) x
ORDER BY x.indexrelid
LIMIT $1`

// Bits of indexesSQL's flags column.
const (
	flagUnique = 1 << iota
	flagPrimary
	flagConstraint
	flagValid
	flagReady
)

func (x *Index) setFlags(f int16) {
	x.Unique = f&flagUnique != 0
	x.Primary = f&flagPrimary != 0
	x.ConstraintBacked = f&flagConstraint != 0
	x.Valid = f&flagValid != 0
	x.Ready = f&flagReady != 0
}

func readIndexes(ctx context.Context, tx pgx.Tx) ([]Index, bool, error) {
	return readIndexesUpTo(ctx, tx, maxIndexRows)
}

// readIndexesUpTo reads the first limit indexes by OID; truncated reports
// that the limit was reached.
func readIndexesUpTo(ctx context.Context, tx pgx.Tx, limit int) ([]Index, bool, error) {
	rows, err := tx.Query(ctx, indexesSQL, limit)
	if err != nil {
		return nil, false, fmt.Errorf("read indexes: %w", err)
	}
	defer rows.Close()
	var out []Index
	for rows.Next() {
		x, err := scanIndex(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, x)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("read indexes: %w", err)
	}
	return out, len(out) == limit, nil
}

func scanIndex(rows pgx.Rows) (Index, error) {
	var x Index
	var keys, classes, collations string
	var flags int16
	if err := rows.Scan(&x.OID, &x.TableOID, &x.Schema, &x.Table, &x.Name, &keys,
		&x.KeyColumns, &classes, &collations, &x.AccessMethod, &flags, &x.Predicate,
		&x.Expressions, &x.SizeBytes, &x.Scans); err != nil {
		return x, fmt.Errorf("scan index: %w", err)
	}
	x.setFlags(flags)
	var err error
	if x.Columns, err = parseInt2Vector(keys); err != nil {
		return x, fmt.Errorf("scan index %d: %w", x.OID, err)
	}
	if x.OpClasses, err = parseOIDVector(classes); err != nil {
		return x, fmt.Errorf("scan index %d: %w", x.OID, err)
	}
	if x.Collations, err = parseOIDVector(collations); err != nil {
		return x, fmt.Errorf("scan index %d: %w", x.OID, err)
	}
	return x, nil
}

// parseInt2Vector parses an int2vector's text form ("2 1 7").
func parseInt2Vector(s string) ([]int16, error) {
	fields := strings.Fields(s)
	out := make([]int16, len(fields))
	for i, f := range fields {
		n, err := strconv.ParseInt(f, 10, 16)
		if err != nil {
			return nil, fmt.Errorf("parse int2vector %q: %w", s, err)
		}
		out[i] = int16(n)
	}
	return out, nil
}

// parseOIDVector parses an oidvector's text form ("3124 3126").
func parseOIDVector(s string) ([]uint32, error) {
	fields := strings.Fields(s)
	out := make([]uint32, len(fields))
	for i, f := range fields {
		n, err := strconv.ParseUint(f, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("parse oidvector %q: %w", s, err)
		}
		out[i] = uint32(n)
	}
	return out, nil
}
