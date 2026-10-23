package classify

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// userRelation restricts the catalog to relations agents may be granted:
// tables, partitioned tables, views, materialized views and foreign tables
// outside the system schemas and pg_sage's own; partitions are classified
// through their parent.
const userRelation = `c.relkind IN ('r','p','v','m','f') AND NOT c.relispartition
  AND n.nspname NOT IN ('sage', 'pg_catalog', 'information_schema')
  AND n.nspname NOT LIKE 'pg\_%'`

const resolveColumnSQL = `/* pg_sage agent_classify v1 */
SELECT c.oid::bigint, a.attnum, n.nspname::text, c.relname::text, a.attname::text,
  pg_catalog.format_type(a.atttypid, a.atttypmod),
  COALESCE(pg_catalog.col_description(c.oid, a.attnum), '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0
  AND NOT a.attisdropped
WHERE n.nspname = $1 AND c.relname = $2 AND a.attname = $3 AND ` + userRelation

const resolveTableSQL = `/* pg_sage agent_classify v1 */
SELECT c.oid::bigint, 0::smallint, n.nspname::text, c.relname::text, ''::text, ''::text,
  COALESCE(pg_catalog.obj_description(c.oid, 'pg_class'), '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2 AND ` + userRelation

// liveColumnSQL re-reads a column (or, with attnum 0, a table) by its keys.
const liveColumnSQL = `/* pg_sage agent_classify v1 */
SELECT c.oid::bigint, COALESCE(a.attnum, 0::smallint), n.nspname::text, c.relname::text,
  COALESCE(a.attname::text, ''),
  COALESCE(pg_catalog.format_type(a.atttypid, a.atttypmod), ''), ''::text
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum = $2
  AND a.attnum > 0 AND NOT a.attisdropped
WHERE c.oid = $1 AND ($2 = 0 OR a.attnum IS NOT NULL) AND ` + userRelation

func scanColumn(row pgx.Row) (Column, error) {
	var c Column
	var relid int64
	if err := row.Scan(&relid, &c.AttNum, &c.Schema, &c.Table, &c.Name, &c.Type,
		&c.Comment); err != nil {
		return Column{}, err
	}
	c.RelID = uint32(relid)
	return c, nil
}

func (s *Store) readColumn(ctx context.Context, what, sql string, args ...any) (Column,
	error) {
	if s.pool == nil {
		return Column{}, ErrNoStore
	}
	c, err := scanColumn(s.pool.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Column{}, fmt.Errorf("%w: %s", ErrColumnNotFound, what)
	}
	if err != nil {
		return Column{}, fmt.Errorf("resolve %s: %w", what, err)
	}
	return c, nil
}

// ResolveColumn finds a user column by name.
func (s *Store) ResolveColumn(ctx context.Context, schema, table, column string) (Column,
	error) {
	return s.readColumn(ctx, schema+"."+table+"."+column, resolveColumnSQL, schema, table,
		column)
}

// ResolveTable finds a user relation by name (AttNum 0).
func (s *Store) ResolveTable(ctx context.Context, schema, table string) (Column, error) {
	return s.readColumn(ctx, schema+"."+table, resolveTableSQL, schema, table)
}

func (s *Store) liveColumn(ctx context.Context, relid uint32, attnum int16) (Column,
	error) {
	return s.readColumn(ctx, fmt.Sprintf("relation %d column %d", relid, attnum),
		liveColumnSQL, int64(relid), attnum)
}

// Cursor is a scan position in (relid, attnum) order.
type Cursor struct {
	RelID  uint32 `json:"relid"`
	AttNum int16  `json:"attnum"`
}

const unclassifiedSQL = `/* pg_sage agent_classify v1 */
SELECT c.oid::bigint, a.attnum, n.nspname::text, c.relname::text, a.attname::text,
  pg_catalog.format_type(a.atttypid, a.atttypmod),
  COALESCE(pg_catalog.col_description(c.oid, a.attnum), '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum > 0
  AND NOT a.attisdropped
WHERE ` + userRelation + `
  AND (c.oid, a.attnum) > ($1::oid, $2::smallint)
  AND NOT EXISTS (SELECT 1 FROM sage.facts f WHERE f.fact_type = 'column_class'
                  AND f.subject_kind = 'column' AND f.subject_relid = c.oid
                  AND f.subject_attnum = a.attnum)
ORDER BY c.oid, a.attnum
LIMIT $3`

// UnclassifiedColumns reads up to limit user columns after the cursor that
// have no classification at all (any status), and the cursor after them.
func (s *Store) UnclassifiedColumns(ctx context.Context, after Cursor, limit int) (
	[]Column, Cursor, error) {
	if s.pool == nil {
		return nil, after, ErrNoStore
	}
	rows, err := s.pool.Query(ctx, unclassifiedSQL, int64(after.RelID), after.AttNum, limit)
	if err != nil {
		return nil, after, fmt.Errorf("read unclassified columns: %w", err)
	}
	defer rows.Close()
	var out []Column
	for rows.Next() {
		c, err := scanColumn(rows)
		if err != nil {
			return nil, after, fmt.Errorf("read unclassified columns: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, after, fmt.Errorf("read unclassified columns: %w", err)
	}
	next := after
	if len(out) > 0 {
		last := out[len(out)-1]
		next = Cursor{RelID: last.RelID, AttNum: last.AttNum}
	}
	return out, next, nil
}
