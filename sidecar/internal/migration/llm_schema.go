package migration

import (
	"context"
	"fmt"
	"strings"

	"github.com/pg-sage/sidecar/internal/catalogread"
)

// fetchTableSchema describes the table from pg_attribute, pg_constraint
// and pg_indexes for the script prompt, each read bounded by the safety
// timeouts. Returns "" on any error (non-fatal).
func (g *ScriptGenerator) fetchTableSchema(
	ctx context.Context, schema, table string,
) string {
	if g.pool == nil || table == "" {
		return ""
	}
	if schema == "" {
		schema = "public"
	}

	result, err := queryTableSchema(ctx, catalogread.New(g.pool, g.timeouts), schema, table)
	if err != nil {
		g.logFn("warn",
			"migration: schema query for %s.%s failed: %v",
			schema, table, err)
		return ""
	}
	return result
}

// queryTableSchema fetches column definitions and constraints from
// the catalog for prompt context.
func queryTableSchema(
	ctx context.Context, q catalogread.Querier,
	schema, table string,
) (string, error) {
	cols, err := queryColumns(ctx, q, schema, table)
	if err != nil {
		return "", fmt.Errorf("columns: %w", err)
	}

	constraints, err := queryConstraints(ctx, q, schema, table)
	if err != nil {
		return "", fmt.Errorf("constraints: %w", err)
	}

	indexes, err := queryIndexes(ctx, q, schema, table)
	if err != nil {
		return "", fmt.Errorf("indexes: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "-- Table: %s.%s\n", schema, table)
	if cols != "" {
		b.WriteString("-- Columns:\n")
		b.WriteString(cols)
	}
	if constraints != "" {
		b.WriteString("-- Constraints:\n")
		b.WriteString(constraints)
	}
	if indexes != "" {
		b.WriteString("-- Indexes:\n")
		b.WriteString(indexes)
	}
	return b.String(), nil
}

// The relation is resolved with to_regclass(format('%I.%I')): the names
// are quoted as identifiers, so mixed-case names resolve, and a missing
// table is no rows rather than an error (migration-10).
const columnQuery = `
SELECT a.attname,
       pg_catalog.format_type(a.atttypid, a.atttypmod),
       CASE WHEN a.attnotnull THEN 'NOT NULL' ELSE '' END,
       COALESCE(pg_catalog.pg_get_expr(d.adbin, d.adrelid), '')
FROM   pg_catalog.pg_attribute a
LEFT JOIN pg_catalog.pg_attrdef d
       ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE  a.attrelid = pg_catalog.to_regclass(pg_catalog.format('%I.%I', $1::text, $2::text))
  AND  a.attnum > 0
  AND  NOT a.attisdropped
ORDER BY a.attnum`

// queryColumns returns a formatted column listing.
func queryColumns(
	ctx context.Context, q catalogread.Querier,
	schema, table string,
) (string, error) {
	rows, err := q.Query(ctx, columnQuery, schema, table)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var name, typ, notNull, defVal string
		if err := rows.Scan(&name, &typ, &notNull, &defVal); err != nil {
			return "", err
		}
		line := fmt.Sprintf("--   %s %s", name, typ)
		if notNull != "" {
			line += " " + notNull
		}
		if defVal != "" {
			line += " DEFAULT " + defVal
		}
		b.WriteString(line + "\n")
	}
	return b.String(), rows.Err()
}

const constraintQuery = `
SELECT conname,
       pg_catalog.pg_get_constraintdef(c.oid, true)
FROM   pg_catalog.pg_constraint c
WHERE  c.conrelid = pg_catalog.to_regclass(pg_catalog.format('%I.%I', $1::text, $2::text))
ORDER BY conname`

// queryConstraints returns a formatted constraint listing.
func queryConstraints(
	ctx context.Context, q catalogread.Querier,
	schema, table string,
) (string, error) {
	rows, err := q.Query(ctx, constraintQuery, schema, table)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "--   %s: %s\n", name, def)
	}
	return b.String(), rows.Err()
}

const indexQuery = `
SELECT indexname, indexdef
FROM   pg_catalog.pg_indexes
WHERE  schemaname = $1
  AND  tablename  = $2
ORDER BY indexname`

// queryIndexes returns a formatted index listing.
func queryIndexes(
	ctx context.Context, q catalogread.Querier,
	schema, table string,
) (string, error) {
	rows, err := q.Query(ctx, indexQuery, schema, table)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var name, def string
		if err := rows.Scan(&name, &def); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "--   %s\n", def)
	}
	return b.String(), rows.Err()
}
